package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/intob/bitch/internal/config"
)

// Voice transcription (§7, PLAN-6 §3). Claude can't read audio, so a voice note
// needs one external call before it can be a prompt: an OpenAI-compatible
// POST /v1/audio/transcriptions (multipart: file, model), which OpenAI, Groq and
// most self-hosted gateways all serve. The user picks the endpoint, including a
// local one — this is the only inbound type whose bytes leave the box, and
// DESIGN.md §4 says so out loud.
//
// The key lives in bridge env only. Nothing here has to defend that: childEnv
// builds the claude environment from scratch, so a bridge var it doesn't name
// simply isn't there (§4.6).
const (
	// transcribeTimeout bounds the whole call. A voice note is seconds of audio;
	// past this the endpoint is broken, and the user is waiting on a turn that
	// hasn't started.
	transcribeTimeout = 60 * time.Second

	// errBodyLimit is how much of a failing response is quoted back. Enough to
	// see the provider's complaint, not enough to paste an HTML error page into
	// the chat.
	errBodyLimit = 400
)

// transcriber calls the configured transcription endpoint. Nil when
// BITCH_TRANSCRIBE_URL is unset — the voice arm checks and declines (fail closed:
// no endpoint means no guessing at what was said).
type transcriber struct {
	url    string
	key    string
	model  string
	client *http.Client
}

// newTranscriber returns nil when the feature is off.
func newTranscriber(cfg *config.Config) *transcriber {
	if cfg.TranscribeURL == "" {
		return nil
	}
	return &transcriber{
		url:    cfg.TranscribeURL,
		key:    cfg.TranscribeKey,
		model:  cfg.TranscribeModel,
		client: &http.Client{Timeout: transcribeTimeout},
	}
}

// transcribe uploads the audio at path and returns what was said. Every failure
// path returns an error: the caller must not start a turn on a guess.
func (t *transcriber) transcribe(ctx context.Context, path string) (string, error) {
	body, contentType, err := multipartAudio(path, t.model)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, body)
	if err != nil {
		return "", fmt.Errorf("transcribe request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	if t.key != "" {
		// Empty is legal — a keyless self-hosted endpoint. An endpoint that wanted
		// a key and didn't get one answers 401, which surfaces as a normal failure.
		req.Header.Set("Authorization", "Bearer "+t.key)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		// The URL can carry a key in a query string on some gateways; report the
		// status, never the request.
		return "", fmt.Errorf("transcription endpoint unreachable: %w", scrubURL(err, t.url))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("transcription endpoint returned %s: %s", resp.Status, errSnippet(resp.Body))
	}

	// The de-facto response shape is {"text": "..."}; a body that isn't that is a
	// misconfigured endpoint, not a transcript.
	var out struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("transcription response was not JSON with a text field: %w", err)
	}
	text := strings.TrimSpace(out.Text)
	if text == "" {
		return "", fmt.Errorf("no speech in that recording")
	}
	return text, nil
}

// multipartAudio builds the request body. Voice notes are capped at 20 MB by the
// download, so buffering is bounded and the endpoint gets a Content-Length —
// some of them reject a chunked upload.
func multipartAudio(path, model string) (io.Reader, string, error) {
	audio, err := os.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("read audio: %w", err)
	}
	defer audio.Close()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	// The extension is what tells the endpoint how to decode; it's bridge-generated
	// (mediaFilename), never the sender's string.
	part, err := w.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return nil, "", fmt.Errorf("build upload: %w", err)
	}
	if _, err := io.Copy(part, audio); err != nil {
		return nil, "", fmt.Errorf("read audio: %w", err)
	}
	if err := w.WriteField("model", model); err != nil {
		return nil, "", fmt.Errorf("build upload: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, "", fmt.Errorf("build upload: %w", err)
	}
	return &buf, w.FormDataContentType(), nil
}

// errSnippet reads a bounded, single-line excerpt of a failing response.
func errSnippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, errBodyLimit))
	s := strings.TrimSpace(strings.ReplaceAll(string(b), "\n", " "))
	if s == "" {
		return "(empty response)"
	}
	return s
}

// scrubURL keeps a configured endpoint URL out of an error that reaches the
// chat: http errors quote the full URL, and some gateways put the key in it.
func scrubURL(err error, url string) error {
	if url == "" {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), url, "<endpoint>"))
}
