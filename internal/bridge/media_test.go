package bridge

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestPickAttachment(t *testing.T) {
	photo := &tgbotapi.Message{Photo: []tgbotapi.PhotoSize{
		{FileID: "small", FileSize: 100},
		{FileID: "large", FileSize: 9000},
	}}
	att, ok := pickAttachment(photo)
	if !ok || att.fileID != "large" || att.kind != "img" {
		// Telegram's ladder is ascending; taking anything but the last gives the
		// agent a thumbnail to read.
		t.Errorf("photo: got %+v (ok=%v), want the largest size", att, ok)
	}

	doc := &tgbotapi.Message{Document: &tgbotapi.Document{
		FileID: "d1", FileName: "notes.pdf", MimeType: "application/pdf", FileSize: 5,
	}}
	if att, ok := pickAttachment(doc); !ok || att.kind != "doc" || att.mime != "application/pdf" {
		t.Errorf("document: got %+v (ok=%v)", att, ok)
	}

	if _, ok := pickAttachment(&tgbotapi.Message{Text: "hi"}); ok {
		t.Error("plain text reported as an attachment")
	}
}

func TestMediaFilename(t *testing.T) {
	now := time.Date(2026, 7, 16, 15, 4, 5, 0, time.UTC)
	cases := []struct {
		name       string
		att        attachment
		wantPrefix string
		wantExt    string
	}{
		{"photo has no mime", attachment{kind: "img"}, "img-20260716-150405-", ".jpg"},
		{"mime wins", attachment{kind: "doc", mime: "application/pdf", name: "x.txt"}, "doc-20260716-150405-", ".pdf"},
		{"mime is case- and space-tolerant", attachment{kind: "doc", mime: " Image/PNG "}, "doc-", ".png"},
		{"unknown mime falls back to the filename's ext", attachment{kind: "doc", mime: "application/x-weird", name: "a.log"}, "doc-", ".log"},
		{"no usable ext at all", attachment{kind: "doc", name: "Makefile"}, "doc-", ".bin"},
		// The sender's name never reaches the path: a traversal or an executable
		// extension must not survive into the filename.
		{"traversal in the name", attachment{kind: "doc", name: "../../etc/passwd"}, "doc-", ".bin"},
		{"nul-ish and odd ext", attachment{kind: "doc", name: "x.sh;rm"}, "doc-", ".bin"},
		{"long ext", attachment{kind: "doc", name: "x.verylongext"}, "doc-", ".bin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mediaFilename(tc.att, now)
			if !strings.HasPrefix(got, tc.wantPrefix) || !strings.HasSuffix(got, tc.wantExt) {
				t.Errorf("mediaFilename = %q, want %q…%q", got, tc.wantPrefix, tc.wantExt)
			}
			if got != filepath.Base(got) {
				t.Errorf("mediaFilename produced a path, not a name: %q", got)
			}
		})
	}
}

// Two files arriving in the same second (an album) must not collide — O_EXCL
// would fail the second one.
func TestMediaFilenameUnique(t *testing.T) {
	now := time.Date(2026, 7, 16, 15, 4, 5, 0, time.UTC)
	att := attachment{kind: "img"}
	if a, b := mediaFilename(att, now), mediaFilename(att, now); a == b {
		t.Errorf("same-second filenames collide: %q", a)
	}
}

func TestMediaPrompt(t *testing.T) {
	img := attachment{kind: "img"}
	doc := attachment{kind: "doc", name: "report.pdf"}
	path := "/state/media/42/img-20260716-150405-ab12.jpg"

	cases := []struct {
		name          string
		caption       string
		att           attachment
		wantContains  []string
		wantNotPrefix string
	}{
		{
			name:         "caption is the prompt",
			caption:      "why is this failing?",
			att:          img,
			wantContains: []string{"why is this failing?", path, "Read tool"},
		},
		{
			name:         "no caption gets an image default",
			att:          img,
			wantContains: []string{"describe what you see", path},
		},
		{
			name:         "no caption gets a file default",
			att:          doc,
			wantContains: []string{"sent a file", path},
		},
		{
			name:         "the sender's filename is context only",
			caption:      "summarize",
			att:          doc,
			wantContains: []string{"summarize", `"report.pdf"`, path},
		},
		{
			name:         "a whitespace caption counts as none",
			caption:      "   \n ",
			att:          img,
			wantContains: []string{"describe what you see"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mediaPrompt(tc.caption, tc.att, path)
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("prompt %q does not contain %q", got, want)
				}
			}
			if !strings.HasSuffix(got, "]") {
				t.Errorf("prompt note not closed: %q", got)
			}
		})
	}
}

func TestSafeName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"report.pdf", "report.pdf"},
		{"../../etc/passwd", "passwd"},
		{"/abs/path/x.png", "x.png"},
		{"..", ""},
		{"a<b>c.txt", "abc.txt"}, // the prompt is not HTML, but the reply path is
		{strings.Repeat("a", 200) + ".txt", strings.Repeat("a", 80)},
	}
	for _, tc := range cases {
		if got := safeName(tc.in); got != tc.want {
			t.Errorf("safeName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// newTestMedia serves body from a local server, standing in for Telegram.
func newTestMedia(t *testing.T, status int, body []byte) *media {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return &media{
		dir:        t.TempDir(),
		getFileURL: func(string) (string, error) { return srv.URL + "/file", nil },
		client:     srv.Client(),
	}
}

func photoMsg(caption string, size int) *tgbotapi.Message {
	return &tgbotapi.Message{
		Chat:    &tgbotapi.Chat{ID: 42},
		Caption: caption,
		Photo:   []tgbotapi.PhotoSize{{FileID: "f1", FileSize: size}},
	}
}

func TestFetchDownloads(t *testing.T) {
	m := newTestMedia(t, http.StatusOK, []byte("JPEGBYTES"))
	prompt, err := m.fetch(42, photoMsg("what is this?", 9))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !strings.Contains(prompt, "what is this?") {
		t.Errorf("caption missing from prompt: %q", prompt)
	}

	// The file must be where the prompt says it is, private, and under the
	// chat's subdir of the media root.
	path := promptPath(t, prompt)
	if dir := filepath.Dir(path); dir != filepath.Join(m.dir, "42") {
		t.Errorf("saved to %q, want the chat subdir of %q", dir, m.dir)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("saved file: %v", err)
	}
	if string(data) != "JPEGBYTES" {
		t.Errorf("saved %q, want the served bytes", data)
	}
	fi, _ := os.Stat(path)
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("saved file mode = %o, want 600", perm)
	}
}

// Fail closed: an oversize file is refused before the fetch, and nothing lands.
func TestFetchRefusesOversize(t *testing.T) {
	m := newTestMedia(t, http.StatusOK, []byte("x"))
	m.getFileURL = func(string) (string, error) {
		t.Error("getFile called for an oversize attachment; the cap should refuse first")
		return "", nil
	}
	_, err := m.fetch(42, photoMsg("", maxMediaBytes+1))
	if err == nil || !strings.Contains(err.Error(), "20 MB") {
		t.Fatalf("fetch error = %v, want a size refusal", err)
	}
	assertNoFiles(t, m.dir)
}

// A file that lies about its size must still be capped by the copy, and the
// partial write must not survive to be read as a prompt's attachment.
func TestFetchCapsLyingSize(t *testing.T) {
	m := newTestMedia(t, http.StatusOK, make([]byte, maxMediaBytes+1))
	if _, err := m.fetch(42, photoMsg("", 10)); err == nil {
		t.Fatal("fetch accepted a body over the cap")
	}
	assertNoFiles(t, m.dir)
}

func TestFetchHTTPError(t *testing.T) {
	m := newTestMedia(t, http.StatusNotFound, []byte("nope"))
	_, err := m.fetch(42, photoMsg("", 10))
	if err == nil {
		t.Fatal("fetch accepted a 404")
	}
	assertNoFiles(t, m.dir)
}

func TestFetchNoAttachment(t *testing.T) {
	m := newTestMedia(t, http.StatusOK, []byte("x"))
	if _, err := m.fetch(42, &tgbotapi.Message{Chat: &tgbotapi.Chat{ID: 42}, Text: "hi"}); err == nil {
		t.Fatal("fetch accepted a message with no file")
	}
}

func TestPruneMedia(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "42")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(sub, "old.jpg")
	fresh := filepath.Join(sub, "fresh.jpg")
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stale := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatal(err)
	}
	n, err := pruneMedia(dir, mediaRetention)
	if err != nil {
		t.Fatalf("pruneMedia: %v", err)
	}
	if n != 1 {
		t.Errorf("removed %d files, want 1", n)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("stale file survived the prune")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh file was pruned")
	}
}

// promptPath pulls the saved path back out of the prompt note.
func promptPath(t *testing.T, prompt string) string {
	t.Helper()
	_, rest, ok := strings.Cut(prompt, "saved at ")
	if !ok {
		t.Fatalf("no path in prompt: %q", prompt)
	}
	path, _, ok := strings.Cut(rest, " —")
	if !ok {
		t.Fatalf("unterminated path in prompt: %q", prompt)
	}
	return path
}

func assertNoFiles(t *testing.T, dir string) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			t.Errorf("file left behind after a failed fetch: %s", path)
		}
		return nil
	})
}
