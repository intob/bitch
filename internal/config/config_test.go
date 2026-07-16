package config

import (
	"os"
	"path/filepath"
	"testing"
)

// setValid sets the minimum viable environment and returns the jail root.
func setValid(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BITCH_TELEGRAM_TOKEN", "123:abc")
	t.Setenv("BITCH_ALLOWED_USERS", "42")
	t.Setenv("BITCH_ROOTS", root)
	t.Setenv("BITCH_DEFAULT_DIR", "")
	// Load creates the media dir under StateDir; keep that out of the package dir.
	t.Setenv("BITCH_STATE_DIR", t.TempDir())
	return root
}

func TestLoadValid(t *testing.T) {
	root := setValid(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.DefaultDir != root {
		t.Errorf("DefaultDir = %q, want first root %q", c.DefaultDir, root)
	}
	if !c.AllowedUser(42) || c.AllowedUser(43) {
		t.Errorf("allowlist wrong: %v", c.AllowedUsers)
	}
	if c.DefaultTier != 1 || c.DailyBudgetUSD != 20 {
		t.Errorf("defaults wrong: tier=%d budget=%v", c.DefaultTier, c.DailyBudgetUSD)
	}
}

// Load must fail closed on every security-critical omission (§4.1, §4.3).
func TestLoadFailsClosed(t *testing.T) {
	cases := []struct{ name, key, val string }{
		{"no token", "BITCH_TELEGRAM_TOKEN", ""},
		{"no allowlist", "BITCH_ALLOWED_USERS", ""},
		{"non-numeric allowlist", "BITCH_ALLOWED_USERS", "@joey"},
		{"no roots", "BITCH_ROOTS", ""},
		{"bad tier", "BITCH_DEFAULT_TIER", "7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setValid(t)
			t.Setenv(c.key, c.val)
			if _, err := Load(); err == nil {
				t.Errorf("Load succeeded with %s=%q, want error", c.key, c.val)
			}
		})
	}
}

// Voice transcription is off unless an endpoint is configured (PLAN-6 §3), and
// the key is optional — a self-hosted endpoint may want no auth at all.
func TestLoadTranscribe(t *testing.T) {
	t.Run("off by default", func(t *testing.T) {
		setValid(t)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.TranscribeURL != "" {
			t.Errorf("TranscribeURL = %q, want empty (feature off)", c.TranscribeURL)
		}
		if c.TranscribeModel != "whisper-1" {
			t.Errorf("TranscribeModel = %q, want the whisper-1 default", c.TranscribeModel)
		}
	})

	t.Run("keyless is legal", func(t *testing.T) {
		setValid(t)
		t.Setenv("BITCH_TRANSCRIBE_URL", "http://127.0.0.1:8080/v1/audio/transcriptions")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load rejected a keyless endpoint: %v", err)
		}
		if c.TranscribeKey != "" {
			t.Errorf("TranscribeKey = %q, want empty", c.TranscribeKey)
		}
	})

	// Voice audio leaves the box, so the endpoint is https — unless it's on the
	// box, where there's no wire to sniff.
	cases := []struct {
		name string
		url  string
		ok   bool
	}{
		{"https", "https://api.openai.com/v1/audio/transcriptions", true},
		{"http on localhost", "http://localhost:8080/v1/audio/transcriptions", true},
		{"http on loopback ip", "http://127.0.0.1:8080/v1/audio/transcriptions", true},
		{"http on the internet", "http://api.openai.com/v1/audio/transcriptions", false},
		{"no scheme", "api.openai.com/v1/audio/transcriptions", false},
		{"not a url", "://nope", false},
		{"wrong scheme", "ftp://example.com/x", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setValid(t)
			t.Setenv("BITCH_TRANSCRIBE_URL", tc.url)
			_, err := Load()
			if tc.ok && err != nil {
				t.Errorf("Load(%q) = %v, want accepted", tc.url, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("Load accepted %q, want a refusal", tc.url)
			}
		})
	}
}

func TestWithinRoots(t *testing.T) {
	root := setValid(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path string
		want bool
	}{
		{root, true},
		{filepath.Join(root, "repo", "file.go"), true},
		{"/etc/passwd", false},
		{root + "-sibling", false}, // prefix match must respect path boundaries
		{filepath.Dir(root), false},
	}
	for _, tc := range cases {
		if got := c.WithinRoots(tc.path); got != tc.want {
			t.Errorf("WithinRoots(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// The media root must exist, be private, and be reachable through the jail —
// otherwise the agent can't Read what the user just sent (§7).
func TestMediaDirInJail(t *testing.T) {
	setValid(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(c.MediaDir)
	if err != nil {
		t.Fatalf("media dir not created: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("media dir mode = %o, want 700", perm)
	}
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"the root itself", c.MediaDir, true},
		{"a download", filepath.Join(c.MediaDir, "42", "img-20260716-150405-ab12.jpg"), true},
		{"escape by traversal", filepath.Dir(c.MediaDir), false},
		{"a sibling sharing the prefix", c.MediaDir + "-evil", false},
	}
	for _, tc := range cases {
		// The jail compares physical paths, so a traversal is cleaned first —
		// as policy.resolve does before calling WithinRoots (§4.3).
		if got := c.WithinRoots(filepath.Clean(tc.path)); got != tc.want {
			t.Errorf("%s: WithinRoots(%q) = %v, want %v", tc.name, tc.path, got, tc.want)
		}
	}
}

// A media root reached through a symlinked StateDir must be stored resolved:
// the jail compares physical paths, so an unresolved root would never match
// what policy.resolve produces (§4.3).
func TestMediaDirSymlinkCanonicalized(t *testing.T) {
	setValid(t)
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(tmp, "state")
	link := filepath.Join(tmp, "state-link")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BITCH_STATE_DIR", link)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(real, "media")
	if c.MediaDir != want {
		t.Errorf("MediaDir = %q, want resolved %q", c.MediaDir, want)
	}
	if !c.WithinRoots(want) {
		t.Errorf("resolved media path %q is not within roots %v", want, c.Roots)
	}
}

// A missing BITCH_ROOTS must still fail closed even though initMediaDir appends
// a root of its own — the media root must never stand in for the jail.
func TestMediaDirDoesNotMaskMissingRoots(t *testing.T) {
	setValid(t)
	t.Setenv("BITCH_ROOTS", "")
	if _, err := Load(); err == nil {
		t.Error("Load succeeded with no BITCH_ROOTS")
	}
}

// A media root already inside a configured root must not be appended twice.
func TestMediaDirNotDuplicatedInRoots(t *testing.T) {
	root := setValid(t)
	t.Setenv("BITCH_STATE_DIR", filepath.Join(root, "state"))
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Roots) != 1 {
		t.Errorf("Roots = %v, want just the configured root", c.Roots)
	}
}

// Roots given as symlinks are canonicalized at load, matching the policy
// jail's physical-path comparison (§4.3).
func TestRootSymlinkCanonicalized(t *testing.T) {
	setValid(t)
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(tmp, "real")
	link := filepath.Join(tmp, "link")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BITCH_ROOTS", link)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Roots[0] != real {
		t.Errorf("root not canonicalized: %q, want %q", c.Roots[0], real)
	}
}
