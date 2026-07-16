package store

import (
	"path/filepath"
	"testing"
)

func TestValidSecretName(t *testing.T) {
	// The load-bearing security unit: a stored name becomes an env var in a root
	// child, so anything that can redirect code loading must be refused
	// (PLAN-CREDENTIALS.md §4.3).
	good := []string{
		"GITHUB_TOKEN", "OPENAI_API_KEY", "STRIPE_SECRET_KEY",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "X", "A1_B2",
	}
	for _, n := range good {
		if err := ValidSecretName(n); err != nil {
			t.Errorf("ValidSecretName(%q) = %v, want nil", n, err)
		}
	}

	bad := []string{
		"", "path", "GitHub", "1ABC", "A-B", "A B", "A.B", // charset / case
		"PATH", "HOME", "TERM", // base process vars
		"LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES", // loader
		"BASH_ENV", "ENV", "IFS", "PS4", "PROMPT_COMMAND", // shell exec hooks
		"GIT_SSH_COMMAND", "GIT_ASKPASS", "SSH_ASKPASS", "PAGER", "EDITOR", // spawn a program
		"NODE_OPTIONS", "PYTHONPATH", "PERL5OPT", "RUBYOPT", "RUBYLIB", // interpreter opts
		"CLASSPATH", "GOPATH", "MANPATH", // *PATH suffix family
		"FOO_OPTS", "BAR_OPTIONS", "BAZ_PRELOAD", // suffix families
		"BITCH_HOOK_SECRET", "ANTHROPIC_API_KEY", "CLAUDE_CONFIG_DIR", // reserved namespaces
	}
	for _, n := range bad {
		if err := ValidSecretName(n); err == nil {
			t.Errorf("ValidSecretName(%q) = nil, want an error", n)
		}
	}
}

func TestSecretRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	if err := st.SaveSecret("GITHUB_TOKEN", "ghp_abc", "PAT for org X"); err != nil {
		t.Fatal(err)
	}
	// Upsert semantics: same name replaces value + description.
	if err := st.SaveSecret("GITHUB_TOKEN", "ghp_def", "PAT for org Y"); err != nil {
		t.Fatal(err)
	}

	// ListSecrets never leaks values.
	list, err := st.ListSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("ListSecrets len = %d, want 1", len(list))
	}
	if list[0].Value != "" {
		t.Errorf("ListSecrets leaked a value: %q", list[0].Value)
	}
	if list[0].Name != "GITHUB_TOKEN" || list[0].Description != "PAT for org Y" {
		t.Errorf("unexpected listing: %+v", list[0])
	}

	// AllSecrets carries values for injection.
	all, err := st.AllSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if all["GITHUB_TOKEN"] != "ghp_def" {
		t.Errorf("AllSecrets[GITHUB_TOKEN] = %q, want ghp_def", all["GITHUB_TOKEN"])
	}

	// Delete.
	ok, err := st.DeleteSecret("GITHUB_TOKEN")
	if err != nil || !ok {
		t.Fatalf("DeleteSecret = (%v, %v), want (true, nil)", ok, err)
	}
	ok, _ = st.DeleteSecret("GITHUB_TOKEN")
	if ok {
		t.Error("second DeleteSecret returned true, want false")
	}
}

func TestSaveSecretRejectsBadNameAndEmptyValue(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	if err := st.SaveSecret("LD_PRELOAD", "/tmp/evil.so", ""); err == nil {
		t.Error("SaveSecret(LD_PRELOAD) succeeded, want rejection")
	}
	if err := st.SaveSecret("GOOD_NAME", "", ""); err == nil {
		t.Error("SaveSecret with empty value succeeded, want rejection")
	}
}

// TestAllSecretsDropsUnsafeNames guards the injection-time re-validation: even a
// value written straight to the table (bypassing SaveSecret) must not surface a
// dangerous env-var name to childEnv.
func TestAllSecretsDropsUnsafeNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	// Write directly, bypassing SaveSecret's validation.
	if _, err := st.db.Exec(
		`INSERT INTO secrets (name, value, description, created_at, updated_at)
		 VALUES ('BASH_ENV','/tmp/x',' ',0,0), ('GITHUB_TOKEN','ghp',' ',0,0)`); err != nil {
		t.Fatal(err)
	}
	all, err := st.AllSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if _, bad := all["BASH_ENV"]; bad {
		t.Error("AllSecrets surfaced BASH_ENV, want it dropped")
	}
	if all["GITHUB_TOKEN"] != "ghp" {
		t.Error("AllSecrets dropped a valid secret")
	}
}
