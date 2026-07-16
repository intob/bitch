package bridge

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/intob/bitch/internal/config"
)

// newTestTranscriber points a transcriber at a local stand-in for the endpoint.
func newTestTranscriber(t *testing.T, h http.HandlerFunc) *transcriber {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &transcriber{url: srv.URL, key: "sk-test", model: "whisper-1", client: srv.Client()}
}

// audioFile writes a stand-in recording and returns its path.
func audioFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "voice-20260716-150405-ab12.oga")
	if err := os.WriteFile(path, []byte("OGGBYTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The happy path, and the request shape the endpoint contract rests on: a
// multipart body with the audio under "file" and the configured model, plus the
// bearer key.
func TestTranscribe(t *testing.T) {
	var gotModel, gotAuth, gotFilename, gotBody string
	tr := newTestTranscriber(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Errorf("content type: %v", err)
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(part)
			switch part.FormName() {
			case "file":
				gotFilename, gotBody = part.FileName(), string(b)
			case "model":
				gotModel = string(b)
			}
		}
		w.Write([]byte(`{"text":"  deploy the thing  "}`))
	})

	text, err := tr.transcribe(context.Background(), audioFile(t))
	if err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	if text != "deploy the thing" {
		t.Errorf("text = %q, want the trimmed transcript", text)
	}
	if gotBody != "OGGBYTES" {
		t.Errorf("uploaded %q, want the audio bytes", gotBody)
	}
	// The extension is how the endpoint picks a decoder.
	if !strings.HasSuffix(gotFilename, ".oga") {
		t.Errorf("upload filename = %q, want the .oga name", gotFilename)
	}
	if gotModel != "whisper-1" || gotAuth != "Bearer sk-test" {
		t.Errorf("model = %q, auth = %q", gotModel, gotAuth)
	}
}

// A keyless self-hosted endpoint is legal: no key means no header, not an empty
// bearer (which some gateways reject outright).
func TestTranscribeKeyless(t *testing.T) {
	var sawAuth bool
	tr := newTestTranscriber(t, func(w http.ResponseWriter, r *http.Request) {
		_, sawAuth = r.Header["Authorization"]
		w.Write([]byte(`{"text":"hello"}`))
	})
	tr.key = ""
	if _, err := tr.transcribe(context.Background(), audioFile(t)); err != nil {
		t.Fatalf("transcribe: %v", err)
	}
	if sawAuth {
		t.Error("an Authorization header was sent for a keyless endpoint")
	}
}

// Every failure is an error — the caller must never start a turn on a guess
// (PLAN-6 §6).
func TestTranscribeFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{
			name: "401",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":{"message":"bad key"}}`))
			},
			want: "401",
		},
		{
			name: "500",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			want: "500",
		},
		{
			name:    "malformed json",
			handler: func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>oops</html>")) },
			want:    "not JSON",
		},
		{
			name:    "no text field",
			handler: func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"error":"nope"}`)) },
			want:    "no speech",
		},
		{
			name:    "silence",
			handler: func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"text":"   "}`)) },
			want:    "no speech",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTestTranscriber(t, tc.handler)
			text, err := tr.transcribe(context.Background(), audioFile(t))
			if err == nil {
				t.Fatalf("transcribe returned %q, want an error", text)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A hung endpoint must not hang the turn forever, and the timeout is an error
// like any other.
func TestTranscribeTimeout(t *testing.T) {
	block := make(chan struct{})
	tr := newTestTranscriber(t, func(w http.ResponseWriter, r *http.Request) { <-block })
	t.Cleanup(func() { close(block) })
	tr.client.Timeout = 50 * time.Millisecond

	if _, err := tr.transcribe(context.Background(), audioFile(t)); err == nil {
		t.Fatal("transcribe survived a hung endpoint")
	}
}

// A cancelled turn (/stop) must not leave the call running.
func TestTranscribeCancel(t *testing.T) {
	block := make(chan struct{})
	tr := newTestTranscriber(t, func(w http.ResponseWriter, r *http.Request) { <-block })
	t.Cleanup(func() { close(block) })

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if _, err := tr.transcribe(ctx, audioFile(t)); err == nil {
		t.Fatal("transcribe ignored a cancelled context")
	}
}

// A missing file is an error, not an empty upload the endpoint has to reject.
func TestTranscribeMissingFile(t *testing.T) {
	tr := newTestTranscriber(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("the endpoint was called for a file that doesn't exist")
	})
	if _, err := tr.transcribe(context.Background(), filepath.Join(t.TempDir(), "gone.oga")); err == nil {
		t.Fatal("transcribe accepted a missing file")
	}
}

// The endpoint URL can carry a key on some gateways; a connection error reaches
// the chat, so it must not carry the URL with it.
func TestTranscribeScrubsURL(t *testing.T) {
	tr := &transcriber{
		url:    "https://gw.example.invalid/v1/audio/transcriptions?token=hunter2",
		model:  "whisper-1",
		client: &http.Client{Timeout: time.Second},
	}
	_, err := tr.transcribe(context.Background(), audioFile(t))
	if err == nil {
		t.Fatal("transcribe reached an invalid host")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("endpoint URL leaked into the error: %v", err)
	}
}

// The feature is off — and the voice arm declines — unless a URL is configured.
func TestNewTranscriber(t *testing.T) {
	if tr := newTranscriber(&config.Config{}); tr != nil {
		t.Error("newTranscriber built a client with no endpoint configured")
	}
	tr := newTranscriber(&config.Config{
		TranscribeURL:   "https://example.com/v1/audio/transcriptions",
		TranscribeKey:   "k",
		TranscribeModel: "whisper-large-v3",
	})
	if tr == nil || tr.model != "whisper-large-v3" {
		t.Errorf("newTranscriber = %+v, want a configured client", tr)
	}
}
