package bridge

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

var (
	testNow   = time.Date(2026, 7, 16, 15, 4, 5, 0, time.UTC)
	errNoFile = errors.New("file is gone")
)

func TestPickVoice(t *testing.T) {
	cases := []struct {
		name     string
		msg      *tgbotapi.Message
		wantMime string
		wantOK   bool
	}{
		{
			name:     "voice note",
			msg:      &tgbotapi.Message{Voice: &tgbotapi.Voice{FileID: "v1", MimeType: "audio/ogg", FileSize: 900}},
			wantMime: "audio/ogg",
			wantOK:   true,
		},
		{
			// VideoNote carries no MimeType of its own; without the default it would
			// land as .bin and the endpoint would have nothing to dispatch on.
			name:     "video note has no mime of its own",
			msg:      &tgbotapi.Message{VideoNote: &tgbotapi.VideoNote{FileID: "n1", FileSize: 900}},
			wantMime: "video/mp4",
			wantOK:   true,
		},
		{
			name:     "audio file",
			msg:      &tgbotapi.Message{Audio: &tgbotapi.Audio{FileID: "a1", MimeType: "audio/mpeg", FileName: "memo.mp3"}},
			wantMime: "audio/mpeg",
			wantOK:   true,
		},
		{"text is not voice", &tgbotapi.Message{Text: "hi"}, "", false},
		{"a photo is not voice", photoMsg("", 10), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			att, ok := pickVoice(tc.msg)
			if ok != tc.wantOK {
				t.Fatalf("pickVoice ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if att.kind != "voice" || att.mime != tc.wantMime {
				t.Errorf("pickVoice = %+v, want kind=voice mime=%q", att, tc.wantMime)
			}
		})
	}
}

// The name on disk is bridge-generated, and its extension is what the endpoint
// decodes by.
func TestVoiceFilename(t *testing.T) {
	cases := []struct {
		name    string
		att     attachment
		wantExt string
	}{
		{"telegram voice note", attachment{kind: "voice", mime: "audio/ogg"}, ".ogg"},
		{"opus", attachment{kind: "voice", mime: "audio/opus"}, ".ogg"},
		{"video note", attachment{kind: "voice", mime: "video/mp4"}, ".mp4"},
		{"audio file", attachment{kind: "voice", mime: "audio/mpeg", name: "memo.mp3"}, ".mp3"},
		{"unknown mime falls back to voice's default", attachment{kind: "voice", mime: "audio/x-weird"}, ".ogg"},
		// The sender's filename still never reaches the path.
		{"traversal in the name", attachment{kind: "voice", name: "../../etc/passwd"}, ".ogg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mediaFilename(tc.att, testNow)
			if !strings.HasPrefix(got, "voice-") || !strings.HasSuffix(got, tc.wantExt) {
				t.Errorf("mediaFilename = %q, want voice-…%q", got, tc.wantExt)
			}
		})
	}
}

// Every name the voice path can produce has to be one the endpoint will decode.
// The extension is the whole contract — OpenAI dispatches on it and rejects the
// rest by name, so a mapping that looks harmless (.oga for an ogg container,
// .opus for opus) fails every voice note at runtime with a 400. This is the
// regression test for that outage: verified live against gpt-4o-transcribe on
// 2026-07-16, where identical opus bytes passed as .ogg and failed as .oga.
func TestVoiceExtensionsAreTranscribable(t *testing.T) {
	// The accepted set, narrowest across the models we might be pointed at.
	// gpt-4o-transcribe takes fewer formats than whisper-1. .flac is the one
	// entry not in OpenAI's documented list but verified working live — it stays
	// because flac bytes have nowhere else to go: renaming them to a documented
	// extension would just fail the decode instead. If OpenAI ever drops it, flac
	// audio files break and nothing else does.
	accepted := map[string]bool{
		".ogg": true, ".mp3": true, ".mp4": true, ".mpeg": true,
		".mpga": true, ".m4a": true, ".wav": true, ".webm": true,
		".flac": true,
	}
	rejected := []string{".oga", ".opus"}

	// Anything mediaExt can hand a voice note must be decodable.
	for mime, ext := range mediaExt {
		if !strings.HasPrefix(mime, "audio/") && mime != "video/mp4" {
			continue // image/document mappings are Read's problem, not the endpoint's
		}
		if !accepted[ext] {
			t.Errorf("mediaExt[%q] = %q, which the transcription endpoint rejects; want one of %v", mime, ext, keysOf(accepted))
		}
	}
	// And the fallback, which is what an unknown mime lands on.
	fallback := mediaFilename(attachment{kind: "voice", mime: "audio/x-unknown"}, testNow)
	if !accepted[filepath.Ext(fallback)] {
		t.Errorf("voice fallback ext = %q, which the endpoint rejects", filepath.Ext(fallback))
	}
	for _, ext := range rejected {
		if accepted[ext] {
			t.Fatalf("test is wrong: %q is in both the accepted and rejected sets", ext)
		}
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestVoicePrompt(t *testing.T) {
	// The marker is what tells the agent to read homophones and missing
	// punctuation as transcription noise rather than intent.
	got := voicePrompt("deploy the thing")
	if !strings.HasPrefix(got, "(transcribed voice message) ") || !strings.HasSuffix(got, "deploy the thing") {
		t.Errorf("voicePrompt = %q", got)
	}
}

func TestEllipsize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"short", "short"},
		{"exactly-10", "exactly-10"}, // at the limit, nothing is cut
		{"eleven-char", "eleven-cha…"},
	}
	for _, tc := range cases {
		if got := ellipsize(tc.in, 10); got != tc.want {
			t.Errorf("ellipsize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// A cut must land on a rune boundary — half a rune renders as a replacement
	// char in the echo the user is supposed to sanity-check.
	got := ellipsize("aaaaaaaaaé", 9)
	if !strings.HasSuffix(got, "…") || strings.ContainsRune(got, '�') {
		t.Errorf("ellipsize split a rune: %q", got)
	}
}

// voiceBridge wires the two halves of the voice path — the Telegram download and
// the transcription endpoint — to local servers. No bot, no sender: this covers
// everything up to the turn.
func voiceBridge(t *testing.T, audio []byte, endpoint http.HandlerFunc) *Bridge {
	t.Helper()
	return &Bridge{
		media: newTestMedia(t, http.StatusOK, audio),
		voice: newTestTranscriber(t, endpoint),
	}
}

func voiceMsg() *tgbotapi.Message {
	return &tgbotapi.Message{
		Chat:  &tgbotapi.Chat{ID: 42},
		Voice: &tgbotapi.Voice{FileID: "v1", MimeType: "audio/ogg", FileSize: 8},
	}
}

func TestTranscribeVoice(t *testing.T) {
	b := voiceBridge(t, []byte("OGGBYTES"), func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"text":"restart the bridge"}`))
	})
	text, err := b.transcribeVoice(context.Background(), 42, voiceMsg())
	if err != nil {
		t.Fatalf("transcribeVoice: %v", err)
	}
	if text != "restart the bridge" {
		t.Errorf("text = %q", text)
	}
	// The audio was input to one HTTPS call and nothing else; it must not linger
	// on the box (PLAN-6 §3).
	assertNoFiles(t, b.media.dir)
}

// Fail closed on both halves: neither a download nor a transcription failure may
// leave audio behind or return text a turn could start on.
func TestTranscribeVoiceFailsClosed(t *testing.T) {
	t.Run("endpoint fails", func(t *testing.T) {
		b := voiceBridge(t, []byte("OGGBYTES"), func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		})
		_, err := b.transcribeVoice(context.Background(), 42, voiceMsg())
		if err == nil || !strings.Contains(err.Error(), "transcription failed") {
			t.Fatalf("err = %v, want a transcription failure", err)
		}
		assertNoFiles(t, b.media.dir)
	})

	t.Run("download fails", func(t *testing.T) {
		b := voiceBridge(t, nil, func(w http.ResponseWriter, r *http.Request) {
			t.Error("the endpoint was called for audio that never downloaded")
		})
		b.media.getFileURL = func(string) (string, error) { return "", errNoFile }
		_, err := b.transcribeVoice(context.Background(), 42, voiceMsg())
		if err == nil || !strings.Contains(err.Error(), "couldn't fetch") {
			t.Fatalf("err = %v, want a download failure", err)
		}
	})

	t.Run("oversize is refused before the fetch", func(t *testing.T) {
		b := voiceBridge(t, []byte("x"), func(w http.ResponseWriter, r *http.Request) {
			t.Error("the endpoint was called for an oversize recording")
		})
		msg := voiceMsg()
		msg.Voice.FileSize = maxMediaBytes + 1
		if _, err := b.transcribeVoice(context.Background(), 42, msg); err == nil {
			t.Fatal("transcribeVoice accepted an oversize recording")
		}
	})

	t.Run("no audio in the message", func(t *testing.T) {
		b := voiceBridge(t, nil, func(w http.ResponseWriter, r *http.Request) {
			t.Error("the endpoint was called for a message with no audio")
		})
		if _, err := b.transcribeVoice(context.Background(), 42, &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 42}}); err == nil {
			t.Fatal("transcribeVoice accepted a message with no audio")
		}
	})
}
