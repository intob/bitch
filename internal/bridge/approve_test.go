package bridge

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/intob/bitch/internal/config"
	"github.com/intob/bitch/internal/store"
)

// TestDecide exercises the permission gate end-to-end through the store: an
// unknown session fails closed, and auto allow/deny verdicts resolve without
// touching Telegram. (The Ask path needs a live bot and is covered manually.)
func TestDecide(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "repo")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(root, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	key := store.Key{ChatID: 1, ThreadID: 0, BotID: 42}
	if err := st.Upsert(&store.Session{Key: key, WorkDir: work, Tier: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetClaudeSession(key, "sess-abc"); err != nil {
		t.Fatal(err)
	}

	b := &Bridge{
		cfg:   &config.Config{Roots: []string{root}},
		store: st,
	}

	tests := []struct {
		name string
		req  hookRequest
		want string
	}{
		{"unknown session fails closed", hookRequest{SessionID: "nope", Tool: "Read"}, "deny"},
		{"read in jail auto-allows", hookRequest{
			SessionID: "sess-abc", Tool: "Read",
			Input: map[string]any{"file_path": filepath.Join(work, "a.go")},
		}, "allow"},
		{"read outside jail denies", hookRequest{
			SessionID: "sess-abc", Tool: "Read",
			Input: map[string]any{"file_path": "/etc/passwd"},
		}, "deny"},
		{"safe bash auto-allows at T1", hookRequest{
			SessionID: "sess-abc", Tool: "Bash",
			Input: map[string]any{"command": "go test ./..."},
		}, "allow"},
		{"edit in jail auto-allows at T1", hookRequest{
			SessionID: "sess-abc", Tool: "Write",
			Input: map[string]any{"file_path": filepath.Join(work, "b.go")},
		}, "allow"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := b.decide(tt.req)
			if got != tt.want {
				t.Errorf("decide = %q (%s), want %q", got, reason, tt.want)
			}
		})
	}
}
