package bridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/intob/bitch/internal/config"
	"github.com/intob/bitch/internal/store"
)

func TestSessionTitle(t *testing.T) {
	tests := []struct {
		name   string
		prompt string
		want   string
	}{
		{
			name:   "short prompt is kept whole",
			prompt: "fix the policy test flake",
			want:   "fix the policy test flake",
		},
		{
			name:   "the first non-empty line is the title",
			prompt: "\n\n  pull main and run the tests\nthen tell me what broke",
			want:   "pull main and run the tests",
		},
		{
			name:   "a long prompt is truncated",
			prompt: strings.Repeat("a", titleLen+20),
			want:   strings.Repeat("a", titleLen) + "…",
		},
		{
			// Truncation is rune-safe: cutting bytes here would leave a mangled
			// rune in the listing.
			name:   "multibyte truncation counts runes, not bytes",
			prompt: strings.Repeat("é", titleLen+5),
			want:   strings.Repeat("é", titleLen) + "…",
		},
		{
			name:   "multibyte under the limit is kept whole",
			prompt: "café ☕ déploiement",
			want:   "café ☕ déploiement",
		},
		{name: "empty prompt", prompt: "   \n  ", want: "(untitled)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionTitle(tc.prompt); got != tc.want {
				t.Errorf("sessionTitle() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHumanAge(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{5 * time.Minute, "5m ago"},
		{2 * time.Hour, "2h ago"},
		{25 * time.Hour, "1d ago"},
		{72 * time.Hour, "3d ago"},
	}
	for _, tc := range tests {
		if got := humanAge(tc.d); got != tc.want {
			t.Errorf("humanAge(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// resumeBridge is a Bridge with a real store and jail but no Telegram: enough
// for the decision half of /resume (resumeSession), which is where the rules
// are. It returns the bridge and the two roots-jailed dirs it set up.
func resumeBridge(t *testing.T) (b *Bridge, work, other string) {
	t.Helper()
	root := t.TempDir()
	// The jail compares physical paths, and macOS /var is a symlink — resolve
	// the root the same way config.Load does.
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	work = filepath.Join(root, "work")
	other = filepath.Join(root, "other")
	for _, d := range []string{work, other} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Bridge{
		cfg: &config.Config{
			Roots:       []string{root},
			DefaultDir:  work,
			DefaultTier: 1,
		},
		store: st,
		runs:  make(map[store.Key]*activeRun),
	}, work, other
}

// The headline: /resume restores the session id *and* the dir it belonged to.
// Claude Code sessions are project-scoped, so an id without its dir would
// resume a session from another project.
func TestResumeRestoresSessionAndWorkdir(t *testing.T) {
	b, work, other := resumeBridge(t)
	if err := b.store.RecordSession(testKey, "sess-old", other, "yesterday's debugging"); err != nil {
		t.Fatal(err)
	}
	// The chat has since moved on to a fresh session in a different dir.
	if err := b.store.Upsert(&store.Session{
		Key: testKey, WorkDir: work, Tier: 1, ClaudeSessionID: "sess-new",
	}); err != nil {
		t.Fatal(err)
	}

	rec, err := b.resumeSession(testKey, "1")
	if err != nil {
		t.Fatalf("resumeSession = %v, want success", err)
	}
	if rec.Title != "yesterday's debugging" {
		t.Errorf("resumed title = %q, want the recorded one", rec.Title)
	}
	sess, err := b.store.Get(testKey)
	if err != nil {
		t.Fatal(err)
	}
	if sess.ClaudeSessionID != "sess-old" {
		t.Errorf("session id = %q, want sess-old", sess.ClaudeSessionID)
	}
	if sess.WorkDir != other {
		t.Errorf("workdir = %q, want the session's own dir %q", sess.WorkDir, other)
	}
}

// The stored dir is re-jailed on the way back in: roots may have changed since
// the session ran, and a resume must not be a way around them.
func TestResumeRejailsTheStoredWorkdir(t *testing.T) {
	b, work, _ := resumeBridge(t)
	outside := t.TempDir() // deliberately not under cfg.Roots
	if err := b.store.RecordSession(testKey, "sess-outside", outside, "from before the roots changed"); err != nil {
		t.Fatal(err)
	}
	if err := b.store.Upsert(&store.Session{Key: testKey, WorkDir: work, Tier: 1}); err != nil {
		t.Fatal(err)
	}

	if _, err := b.resumeSession(testKey, "1"); err == nil {
		t.Fatal("resumeSession into a dir outside the roots = nil error, want a refusal")
	}
	// Fail closed: the refusal leaves the session exactly as it was.
	sess, err := b.store.Get(testKey)
	if err != nil {
		t.Fatal(err)
	}
	if sess.ClaudeSessionID != "" || sess.WorkDir != work {
		t.Errorf("session after a refused resume = %+v, want it untouched", sess)
	}
}

// Swapping the session id under a live turn would corrupt the id-routing the
// approval hook depends on (§4.3), so a busy key refuses.
func TestResumeRefusesWhileATurnIsRunning(t *testing.T) {
	b, work, other := resumeBridge(t)
	if err := b.store.RecordSession(testKey, "sess-old", other, "yesterday"); err != nil {
		t.Fatal(err)
	}
	if err := b.store.Upsert(&store.Session{
		Key: testKey, WorkDir: work, Tier: 1, ClaudeSessionID: "sess-new",
	}); err != nil {
		t.Fatal(err)
	}
	if got := b.claimOrQueue(testKey, "a turn", noopCancel); got != turnClaimed {
		t.Fatal("claim failed")
	}

	_, err := b.resumeSession(testKey, "1")
	if err == nil {
		t.Fatal("resumeSession while busy = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "/stop") {
		t.Errorf("refusal = %q, want it to point at /stop", err)
	}
	sess, _ := b.store.Get(testKey)
	if sess.ClaudeSessionID != "sess-new" {
		t.Errorf("session id = %q, want the live turn's id untouched", sess.ClaudeSessionID)
	}

	// It works once the turn is done.
	b.releaseTurn(testKey)
	if _, err := b.resumeSession(testKey, "1"); err != nil {
		t.Errorf("resumeSession after release = %v, want success", err)
	}
}

func TestResumeRejectsBadIndex(t *testing.T) {
	b, work, other := resumeBridge(t)
	if err := b.store.RecordSession(testKey, "sess-old", other, "yesterday"); err != nil {
		t.Fatal(err)
	}
	if err := b.store.Upsert(&store.Session{Key: testKey, WorkDir: work, Tier: 1}); err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{"", "0", "-1", "2", "abc", "1.5"} {
		if _, err := b.resumeSession(testKey, arg); err == nil {
			t.Errorf("resumeSession(%q) = nil error, want a refusal", arg)
		}
	}
	// The rejections leave the session alone.
	sess, _ := b.store.Get(testKey)
	if sess.ClaudeSessionID != "" {
		t.Errorf("session id = %q, want it untouched", sess.ClaudeSessionID)
	}
}
