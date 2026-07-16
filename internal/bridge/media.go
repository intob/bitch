package bridge

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Inbound attachments (§7). A photo of an error or a screenshot is the most
// phone-native input there is, so a photo with a caption becomes an ordinary
// turn: the bridge downloads the file into the media root and points the agent
// at it with the Read tool, which renders images natively.
//
// Two rules carry the security here. Files land under config.MediaDir — inside
// the jail roots, but never the session workdir, so a cloned repo doesn't
// collect binary blobs. And filenames are generated bridge-side: Telegram's
// Document.FileName is sender-controlled, i.e. a path-traversal surface, so it
// is never used to build a path — at most its sanitized basename is mentioned
// in the prompt text as context.
const (
	// maxMediaBytes is Telegram's own getFile ceiling for bots. Enforced here
	// too so an oversize file is a clear refusal rather than a mid-download
	// failure — and so a lying FileSize can't stream unbounded onto the disk.
	maxMediaBytes = 20 << 20

	// mediaRetention is how long downloads survive; the media root is cache, not
	// archive (the box itself is disposable, §4.5). Pruned at boot.
	mediaRetention = 7 * 24 * time.Hour
)

// media downloads Telegram attachments. The Telegram fetch sits behind
// getFileURL + client so tests can serve bytes from a local server.
type media struct {
	dir        string
	getFileURL func(fileID string) (string, error)
	client     *http.Client
}

func newMedia(bot *tgbotapi.BotAPI, dir string) *media {
	return &media{
		dir:        dir,
		getFileURL: bot.GetFileDirectURL,
		client:     &http.Client{Timeout: 2 * time.Minute},
	}
}

// attachment is what the bridge picked out of a message.
type attachment struct {
	fileID string
	kind   string // "img", "doc", or "voice" — the filename prefix, and picks the no-caption prompt
	mime   string // Telegram's MimeType; empty for photos
	name   string // sender-supplied filename; context only, never part of a path
	size   int    // Telegram's reported size; advisory, the copy is capped regardless
}

// pickAttachment returns the downloadable file in msg, or false. For photos
// Telegram sends an ascending ladder of sizes; the last is the largest.
func pickAttachment(msg *tgbotapi.Message) (attachment, bool) {
	switch {
	case len(msg.Photo) > 0:
		p := msg.Photo[len(msg.Photo)-1]
		return attachment{fileID: p.FileID, kind: "img", size: p.FileSize}, true
	case msg.Document != nil:
		d := msg.Document
		return attachment{fileID: d.FileID, kind: "doc", mime: d.MimeType, name: d.FileName, size: d.FileSize}, true
	}
	return attachment{}, false
}

// pickVoice returns the audio in msg, or false (§7, PLAN-6 §3). All three arms
// become one kind: what the endpoint gets is a file with an extension, and it —
// not the bridge — decides what it can decode. A video note is audio to us.
func pickVoice(msg *tgbotapi.Message) (attachment, bool) {
	switch {
	case msg.Voice != nil:
		v := msg.Voice
		return attachment{fileID: v.FileID, kind: "voice", mime: v.MimeType, size: v.FileSize}, true
	case msg.VideoNote != nil:
		// VideoNote carries no MimeType; Telegram records them as mp4.
		n := msg.VideoNote
		return attachment{fileID: n.FileID, kind: "voice", mime: "video/mp4", size: n.FileSize}, true
	case msg.Audio != nil:
		a := msg.Audio
		return attachment{fileID: a.FileID, kind: "voice", mime: a.MimeType, name: a.FileName, size: a.FileSize}, true
	}
	return attachment{}, false
}

// fetch downloads the attachment and returns the prompt for the turn it becomes.
func (m *media) fetch(chatID int64, msg *tgbotapi.Message) (string, error) {
	att, ok := pickAttachment(msg)
	if !ok {
		return "", fmt.Errorf("no downloadable file in that message")
	}
	path, err := m.fetchAttachment(chatID, att)
	if err != nil {
		return "", err
	}
	return mediaPrompt(msg.Caption, att, path), nil
}

// fetchAttachment downloads att into the chat's media dir and returns the path.
// Voice takes this path directly: its file is transcription input, not something
// the agent reads, so it never becomes a media prompt.
func (m *media) fetchAttachment(chatID int64, att attachment) (string, error) {
	if att.size > maxMediaBytes {
		return "", fmt.Errorf("that file is %s — Telegram caps bot downloads at 20 MB", humanBytes(att.size))
	}

	// The direct URL embeds the bot token (that's how the Bot API serves files).
	// It stays bridge-side: never log it, never let it reach the prompt.
	url, err := m.getFileURL(att.fileID)
	if err != nil {
		return "", fmt.Errorf("telegram getFile: %w", err)
	}

	dir := filepath.Join(m.dir, strconv.FormatInt(chatID, 10))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("media dir: %w", err)
	}
	path := filepath.Join(dir, mediaFilename(att, time.Now()))
	if err := m.download(url, path); err != nil {
		return "", err
	}
	return path, nil
}

// download streams url to path, refusing anything over the cap. The copy is
// capped independently of the advertised FileSize: the cap is the guarantee,
// the header is a hint.
func (m *media) download(url, path string) error {
	resp, err := m.client.Get(url)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: telegram returned %s", resp.Status)
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("media file: %w", err)
	}
	n, copyErr := io.Copy(f, io.LimitReader(resp.Body, maxMediaBytes+1))
	closeErr := f.Close()
	if copyErr == nil && closeErr == nil && n <= maxMediaBytes {
		return nil
	}
	// Fail closed: a partial or oversize file must not become a prompt.
	os.Remove(path)
	switch {
	case copyErr != nil:
		return fmt.Errorf("download failed: %w", copyErr)
	case closeErr != nil:
		return fmt.Errorf("media file: %w", closeErr)
	default:
		return fmt.Errorf("that file is over the 20 MB download cap")
	}
}

// mediaExt maps what Telegram tells us to a file extension. Claude Code's Read
// tool dispatches on extension, so getting this right is what makes an image
// render rather than land as bytes — and the audio entries do the same job for
// the transcription endpoint, which sniffs the upload's filename to pick a
// decoder (PLAN-6 §3).
// Two of these are counter-intuitive and load-bearing: a Telegram voice note is
// opus in an ogg container, whose pedantically-correct audio-only extension is
// .oga — but OpenAI's gpt-4o-transcribe rejects both ".oga" and ".opus" outright
// ("Unsupported file format oga") while accepting the identical bytes as ".ogg".
// whisper-1 takes all three, so this only bites on the newer models. .ogg is the
// name every endpoint agrees on; don't "correct" these back.
var mediaExt = map[string]string{
	"audio/ogg":        ".ogg", // Telegram voice notes: opus in an ogg container
	"audio/opus":       ".ogg", // opus is ogg-contained in practice; ".opus" is rejected
	"audio/mpeg":       ".mp3",
	"audio/mp4":        ".m4a",
	"audio/x-m4a":      ".m4a",
	"audio/wav":        ".wav",
	"audio/x-wav":      ".wav",
	"audio/flac":       ".flac",
	"audio/webm":       ".webm",
	"video/mp4":        ".mp4", // video notes
	"image/jpeg":       ".jpg",
	"image/png":        ".png",
	"image/gif":        ".gif",
	"image/webp":       ".webp",
	"application/pdf":  ".pdf",
	"text/plain":       ".txt",
	"text/markdown":    ".md",
	"text/csv":         ".csv",
	"application/json": ".json",
	"application/zip":  ".zip",
}

// mediaFilename generates the on-disk name: kind, timestamp, and four random
// hex. The random suffix is what makes the name collision-free — an album
// arrives as several updates within the same second, and O_EXCL would fail the
// second one on timestamp alone.
func mediaFilename(att attachment, now time.Time) string {
	ext := mediaExt[strings.ToLower(strings.TrimSpace(att.mime))]
	if ext == "" {
		ext = safeExt(att.name)
	}
	if ext == "" {
		switch att.kind {
		case "img":
			ext = ".jpg" // Telegram re-encodes every photo to JPEG
		case "voice":
			ext = ".ogg" // what Telegram records a voice note as; .ogg, not .oga (see mediaExt)
		default:
			ext = ".bin"
		}
	}
	return fmt.Sprintf("%s-%s-%s%s", att.kind, now.Format("20060102-150405"), randHex(2), ext)
}

// safeExt extracts an extension from a sender-supplied filename, but only if it
// is provably boring: lowercase alphanumerics, at most 8 of them. Anything else
// (no extension, path separators, dots, unicode) yields "" and the caller falls
// back. The name never reaches a path any other way.
func safeExt(name string) string {
	ext := strings.ToLower(filepath.Ext(filepath.Base(name)))
	if len(ext) < 2 || len(ext) > 9 {
		return ""
	}
	for _, r := range ext[1:] {
		if !('a' <= r && r <= 'z') && !('0' <= r && r <= '9') {
			return ""
		}
	}
	return ext
}

// safeName is the sender's filename reduced to a basename fit to print into a
// prompt: no separators, bounded length. Context for the agent, never a path.
func safeName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == '/' || r == '\\' || r == '<' || r == '>' {
			return -1
		}
		return r
	}, name)
	if name == "." || name == ".." {
		return ""
	}
	if len(name) > 80 {
		name = name[:80]
	}
	return name
}

// mediaPrompt builds the turn's prompt: the caption is the ask, and the file is
// appended as a pointer. With no caption the agent gets a sensible default
// rather than an empty turn.
func mediaPrompt(caption string, att attachment, path string) string {
	prompt := strings.TrimSpace(caption)
	if prompt == "" {
		if att.kind == "img" {
			prompt = "The user sent an image with no caption — describe what you see and ask what they want if it's unclear."
		} else {
			prompt = "The user sent a file with no caption — look at it and ask what they want if it's unclear."
		}
	}
	note := fmt.Sprintf("[Attached file saved at %s — use the Read tool to view it.", path)
	if n := safeName(att.name); n != "" {
		note += fmt.Sprintf(" The user's filename for it was %q.", n)
	}
	return prompt + "\n\n" + note + "]"
}

// pruneMedia deletes downloads older than mediaRetention. Best-effort and
// boot-time only: a failure here is housekeeping, never a reason not to start.
func pruneMedia(dir string, olderThan time.Duration) (int, error) {
	cutoff := time.Now().Add(-olderThan)
	var removed int
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable entry is not worth failing a boot over
		}
		info, err := d.Info()
		if err != nil || info.ModTime().After(cutoff) {
			return nil
		}
		if os.Remove(path) == nil {
			removed++
		}
		return nil
	})
	return removed, err
}

func humanBytes(n int) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1f kB", float64(n)/(1<<10))
}
