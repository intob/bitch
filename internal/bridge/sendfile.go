package bridge

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// The /sendfile loopback endpoint backs the send_file MCP tool (§7): it is how
// the agent delivers a binary artifact — a rendered image, a built binary, a
// screenshot — into the chat. It mirrors /store: same shared-secret gate, and
// the human confirmation already happened at the PreToolUse gate, which asks
// over Telegram below T2 before the tool is allowed to run.
//
// Unlike /store it has to name a chat, so the MCP server passes the Claude
// session id it was registered with (agent.writeMCPConfig) and the bridge routes
// session → chat, exactly as the hook does.
//
// Every validation here is a re-check, not a first check: the gate resolved and
// jailed the path already. That duplication is the point — this handler must not
// assume the gate ran, the same defense-in-depth stance as childEnv re-checking
// vault names (§4.7).

// secretsDir is the vault's fixed directory, denied here as a literal — the
// same literal policy.denyPatterns uses (§4.2). It is deliberately not derived
// from cfg.SecretsPath: both are fixed by design, and two independent statements
// of the same constant is what makes this a second check rather than a shared
// assumption.
const secretsDir = "/etc/bitch"

const (
	// maxSendBytes is Telegram's ceiling for bot uploads.
	maxSendBytes = 50 << 20

	// maxPhotoBytes is Telegram's ceiling for the inline-photo path; a bigger
	// image is still deliverable, just as a document.
	maxPhotoBytes = 10 << 20
)

// photoExts are the extensions that go out as an inline photo rather than a
// document — the preview is the whole point of sending an image to a phone.
var photoExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
}

// uploader is the upload surface /sendfile needs, behind an interface so the
// endpoint's validation is testable without Telegram.
type uploader interface {
	SendPhoto(chatID int64, filename string, data []byte, caption string) error
	SendDocument(chatID int64, filename string, data []byte, caption string) error
}

// sendFileRequest is what `bitch mcp` POSTs to deliver a file.
type sendFileRequest struct {
	SessionID string `json:"session_id"`
	Path      string `json:"path"`
	Caption   string `json:"caption"`
}

type sendFileResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func (b *Bridge) handleSendFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("X-Bitch-Secret") != b.cfg.HookSecret {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req sendFileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeSendFile(w, false, "bad request")
		return
	}

	// Unknown session → refuse: without a session there is no chat to send to,
	// and no tier that could have approved this (fail closed).
	sess, err := b.store.GetByClaudeSession(req.SessionID)
	if err != nil || sess == nil {
		writeSendFile(w, false, "no matching session")
		return
	}

	path, data, err := b.readSendable(sess.WorkDir, req.Path)
	if err != nil {
		// The refusal goes back through the MCP result so the agent can adapt
		// (compress it, split it, pick a path inside the workspace).
		writeSendFile(w, false, err.Error())
		return
	}

	if err := b.upload(sess.Key.ChatID, path, data, req.Caption); err != nil {
		log.Printf("send_file upload (chat %d): %v", sess.Key.ChatID, err)
		writeSendFile(w, false, "upload failed: "+err.Error())
		return
	}
	log.Printf("agent sent file %q (%s) to chat %d", filepath.Base(path), humanBytes(len(data)), sess.Key.ChatID)
	writeSendFile(w, true, "")
}

// readSendable resolves path against the session workdir, re-applies every rule
// the gate applied, and reads the bytes. It returns the resolved path so the
// upload names the file by its real basename.
func (b *Bridge) readSendable(workdir, arg string) (string, []byte, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "", nil, fmt.Errorf("no path given")
	}
	if strings.HasPrefix(arg, "~") {
		return "", nil, fmt.Errorf("~ paths are not allowed; use a path inside the workspace")
	}
	p := arg
	if !filepath.IsAbs(p) {
		p = filepath.Join(workdir, arg)
	}
	p = filepath.Clean(p)
	// The jail compares physical paths: resolve symlinks first, or a link inside
	// a root pointing outside it would launder the target (§4.3).
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	// The vault's own directory, explicitly. It is outside the roots anyway, so
	// the check below already covers it — belt and braces on the one path whose
	// exposure would be worst (§4.2, §4.7).
	if p == secretsDir || strings.HasPrefix(p, secretsDir+string(os.PathSeparator)) {
		return "", nil, fmt.Errorf("that path is not sendable")
	}
	if !b.cfg.WithinRoots(p) {
		return "", nil, fmt.Errorf("%s is outside the allowed roots", p)
	}

	info, err := os.Stat(p)
	if err != nil {
		return "", nil, fmt.Errorf("%s: %v", p, err)
	}
	// Regular files only: a directory has nothing to upload, and a device or
	// fifo would read forever.
	if !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("%s is not a regular file", p)
	}
	if info.Size() == 0 {
		return "", nil, fmt.Errorf("%s is empty", p)
	}
	if info.Size() > maxSendBytes {
		return "", nil, fmt.Errorf("%s is %s — Telegram caps bot uploads at 50 MB",
			p, humanBytes(int(info.Size())))
	}

	f, err := os.Open(p)
	if err != nil {
		return "", nil, fmt.Errorf("%s: %v", p, err)
	}
	defer f.Close()
	// Cap the read independently of the stat: the size checked above is a
	// different moment in time than the read (§7, same rule as inbound media).
	data, err := io.ReadAll(io.LimitReader(f, maxSendBytes+1))
	if err != nil {
		return "", nil, fmt.Errorf("read %s: %v", p, err)
	}
	if len(data) > maxSendBytes {
		return "", nil, fmt.Errorf("%s is over the 50 MB upload cap", p)
	}
	return p, data, nil
}

// upload dispatches to the photo or document path. An image that Telegram
// refuses as a photo (it enforces dimension and ratio limits of its own) still
// goes out as a document rather than failing the send.
func (b *Bridge) upload(chatID int64, path string, data []byte, caption string) error {
	name := filepath.Base(path)
	if photoExts[strings.ToLower(filepath.Ext(name))] && len(data) <= maxPhotoBytes {
		if err := b.uploads.SendPhoto(chatID, name, data, caption); err == nil {
			return nil
		}
	}
	return b.uploads.SendDocument(chatID, name, data, caption)
}

func writeSendFile(w http.ResponseWriter, ok bool, errMsg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sendFileResponse{OK: ok, Error: errMsg})
}
