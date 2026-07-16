package bridge

import (
	"context"
	"errors"
	"net/http"
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
		{"telegram voice note", attachment{kind: "voice", mime: "audio/ogg"}, ".oga"},
		{"video note", attachment{kind: "voice", mime: "video/mp4"}, ".mp4"},
		{"audio file", attachment{kind: "voice", mime: "audio/mpeg", name: "memo.mp3"}, ".mp3"},
		{"unknown mime falls back to voice's default", attachment{kind: "voice", mime: "audio/x-weird"}, ".oga"},
		// The sender's filename still never reaches the path.
		{"traversal in the name", attachment{kind: "voice", name: "../../etc/passwd"}, ".oga"},
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
