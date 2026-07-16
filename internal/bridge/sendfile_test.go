package bridge

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/intob/bitch/internal/config"
	"github.com/intob/bitch/internal/store"
)

// fakeUploader records what would have gone to Telegram.
type fakeUploader struct {
	photos, docs []upload
	photoErr     error
}

type upload struct {
	chatID  int64
	name    string
	size    int
	caption string
}

func (f *fakeUploader) SendPhoto(chatID int64, name string, data []byte, caption string) error {
	if f.photoErr != nil {
		return f.photoErr
	}
	f.photos = append(f.photos, upload{chatID, name, len(data), caption})
	return nil
}

func (f *fakeUploader) SendDocument(chatID int64, name string, data []byte, caption string) error {
	f.docs = append(f.docs, upload{chatID, name, len(data), caption})
	return nil
}

// sendFileFixture wires a Bridge with a real store, a jail root, and a stub
// uploader. It returns the bridge, the fake, and the workdir.
func sendFileFixture(t *testing.T) (*Bridge, *fakeUploader, string) {
	t.Helper()
	// Canonicalize like config.Load: the jail compares physical paths.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(root, "repo")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(root, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	key := store.Key{ChatID: 77, ThreadID: 0, BotID: 42}
	if err := st.Upsert(&store.Session{Key: key, WorkDir: work, Tier: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetClaudeSession(key, "sess-abc"); err != nil {
		t.Fatal(err)
	}

	up := &fakeUploader{}
	b := &Bridge{
		cfg:     &config.Config{Roots: []string{root}, HookSecret: "hs"},
		store:   st,
		uploads: up,
	}
	return b, up, work
}

// post drives the endpoint and returns the decoded response.
func postSendFile(t *testing.T, b *Bridge, secret string, body sendFileRequest) sendFileResponse {
	t.Helper()
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/sendfile", bytes.NewReader(data))
	req.Header.Set("X-Bitch-Secret", secret)
	w := httptest.NewRecorder()
	b.handleSendFile(w, req)
	var res sendFileResponse
	_ = json.NewDecoder(w.Body).Decode(&res)
	return res
}

func writeFile(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), n), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSendFileHappyPath proves an in-jail image goes out as an inline photo,
// named by its real basename and routed to the session's chat.
func TestSendFileHappyPath(t *testing.T) {
	b, up, work := sendFileFixture(t)
	writeFile(t, filepath.Join(work, "chart.png"), 100)

	res := postSendFile(t, b, "hs", sendFileRequest{
		SessionID: "sess-abc", Path: "chart.png", Caption: "the chart",
	})
	if !res.OK {
		t.Fatalf("send refused: %q", res.Error)
	}
	if len(up.photos) != 1 || len(up.docs) != 0 {
		t.Fatalf("photos=%d docs=%d, want 1/0", len(up.photos), len(up.docs))
	}
	got := up.photos[0]
	if got.chatID != 77 || got.name != "chart.png" || got.size != 100 || got.caption != "the chart" {
		t.Errorf("upload = %+v", got)
	}
}

// TestSendFileNonImageIsDocument: only the image extensions preview inline.
func TestSendFileNonImageIsDocument(t *testing.T) {
	b, up, work := sendFileFixture(t)
	writeFile(t, filepath.Join(work, "bitch"), 100)

	if res := postSendFile(t, b, "hs", sendFileRequest{SessionID: "sess-abc", Path: "bitch"}); !res.OK {
		t.Fatalf("send refused: %q", res.Error)
	}
	if len(up.docs) != 1 || len(up.photos) != 0 {
		t.Fatalf("photos=%d docs=%d, want 0/1", len(up.photos), len(up.docs))
	}
}

// TestSendFilePhotoFallsBackToDocument: Telegram rejects some valid images, and
// a refused preview must still deliver the file.
func TestSendFilePhotoFallsBackToDocument(t *testing.T) {
	b, up, work := sendFileFixture(t)
	up.photoErr = http.ErrHandlerTimeout
	writeFile(t, filepath.Join(work, "wide.png"), 100)

	if res := postSendFile(t, b, "hs", sendFileRequest{SessionID: "sess-abc", Path: "wide.png"}); !res.OK {
		t.Fatalf("send refused: %q", res.Error)
	}
	if len(up.docs) != 1 {
		t.Fatalf("docs=%d, want 1 (photo failure must fall back)", len(up.docs))
	}
}

// TestSendFileRejects is one case per fail-closed rule: nothing here may upload.
func TestSendFileRejects(t *testing.T) {
	b, up, work := sendFileFixture(t)
	root := filepath.Dir(work)

	writeFile(t, filepath.Join(work, "ok.png"), 10)
	// A file outside every root, and a symlink inside the jail pointing at it:
	// the physical path is what the check must see (§4.3).
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "id_rsa")
	writeFile(t, secret, 10)
	if err := os.Symlink(secret, filepath.Join(work, "link.png")); err != nil {
		t.Fatal(err)
	}
	// Oversize.
	big := filepath.Join(work, "big.bin")
	if err := os.WriteFile(big, bytes.Repeat([]byte("x"), maxSendBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(work, "empty.txt"), 0)

	tests := []struct {
		name string
		req  sendFileRequest
		want string // substring of the refusal
	}{
		{"unknown session", sendFileRequest{SessionID: "nope", Path: "ok.png"}, "no matching session"},
		{"empty session", sendFileRequest{SessionID: "", Path: "ok.png"}, "no matching session"},
		{"out of jail", sendFileRequest{SessionID: "sess-abc", Path: secret}, "outside the allowed roots"},
		{"traversal", sendFileRequest{SessionID: "sess-abc", Path: "../../etc/passwd"}, ""},
		{"symlink escape", sendFileRequest{SessionID: "sess-abc", Path: "link.png"}, "outside the allowed roots"},
		{"vault path", sendFileRequest{SessionID: "sess-abc", Path: "/etc/bitch/secrets.db"}, "not sendable"},
		{"vault dir", sendFileRequest{SessionID: "sess-abc", Path: secretsDir}, "not sendable"},
		{"home shorthand", sendFileRequest{SessionID: "sess-abc", Path: "~/.ssh/id_rsa"}, "not allowed"},
		{"directory", sendFileRequest{SessionID: "sess-abc", Path: work}, "not a regular file"},
		{"device", sendFileRequest{SessionID: "sess-abc", Path: "/dev/zero"}, ""},
		{"oversize", sendFileRequest{SessionID: "sess-abc", Path: "big.bin"}, "50 MB"},
		{"empty file", sendFileRequest{SessionID: "sess-abc", Path: "empty.txt"}, "is empty"},
		{"no path", sendFileRequest{SessionID: "sess-abc", Path: ""}, "no path"},
		{"missing file", sendFileRequest{SessionID: "sess-abc", Path: "nope.png"}, ""},
		{"root itself", sendFileRequest{SessionID: "sess-abc", Path: root}, "not a regular file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := postSendFile(t, b, "hs", tt.req)
			if res.OK {
				t.Fatalf("send was allowed, want refusal")
			}
			if tt.want != "" && !strings.Contains(res.Error, tt.want) {
				t.Errorf("error = %q, want it to mention %q", res.Error, tt.want)
			}
		})
	}
	if len(up.photos)+len(up.docs) != 0 {
		t.Errorf("a refused send uploaded something: photos=%v docs=%v", up.photos, up.docs)
	}
}

// TestSendFileSecretGate: the loopback endpoint is only for our own subprocesses.
func TestSendFileSecretGate(t *testing.T) {
	b, up, work := sendFileFixture(t)
	writeFile(t, filepath.Join(work, "ok.png"), 10)

	for _, secret := range []string{"", "wrong"} {
		data, _ := json.Marshal(sendFileRequest{SessionID: "sess-abc", Path: "ok.png"})
		req := httptest.NewRequest(http.MethodPost, "/sendfile", bytes.NewReader(data))
		if secret != "" {
			req.Header.Set("X-Bitch-Secret", secret)
		}
		w := httptest.NewRecorder()
		b.handleSendFile(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("secret %q: status = %d, want 403", secret, w.Code)
		}
	}
	// GET is not a send, whatever the secret.
	req := httptest.NewRequest(http.MethodGet, "/sendfile", nil)
	req.Header.Set("X-Bitch-Secret", "hs")
	w := httptest.NewRecorder()
	b.handleSendFile(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", w.Code)
	}
	if len(up.photos)+len(up.docs) != 0 {
		t.Errorf("ungated send uploaded something")
	}
}
