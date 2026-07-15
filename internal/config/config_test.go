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
