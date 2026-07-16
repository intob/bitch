package store

import (
	"path/filepath"
	"testing"
)

func TestValidFactKey(t *testing.T) {
	good := []string{"email", "github_username", "work.email", "id-42", "X"}
	for _, k := range good {
		if err := ValidFactKey(k); err != nil {
			t.Errorf("ValidFactKey(%q) = %v, want nil", k, err)
		}
	}
	bad := []string{"", "has space", "-leading", ".dot", "new\nline", "a/b"}
	for _, k := range bad {
		if err := ValidFactKey(k); err == nil {
			t.Errorf("ValidFactKey(%q) = nil, want an error", k)
		}
	}
}

func TestFactRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	if err := st.SaveFact("email", "joey@rmrf.uk"); err != nil {
		t.Fatal(err)
	}
	// Values may contain spaces; upsert replaces.
	if err := st.SaveFact("full_name", "Joey Innes"); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveFact("email", "joey@example.com"); err != nil {
		t.Fatal(err)
	}

	list, err := st.ListFacts()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("ListFacts len = %d, want 2", len(list))
	}
	// Ordered by key: email, full_name.
	if list[0].Key != "email" || list[0].Value != "joey@example.com" {
		t.Errorf("unexpected fact[0]: %+v", list[0])
	}
	if list[1].Key != "full_name" || list[1].Value != "Joey Innes" {
		t.Errorf("unexpected fact[1]: %+v", list[1])
	}

	ok, err := st.DeleteFact("email")
	if err != nil || !ok {
		t.Fatalf("DeleteFact = (%v, %v), want (true, nil)", ok, err)
	}
	ok, _ = st.DeleteFact("email")
	if ok {
		t.Error("second DeleteFact returned true, want false")
	}
}

func TestSaveFactRejects(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	if err := st.SaveFact("has space", "x"); err == nil {
		t.Error("SaveFact with bad key succeeded, want rejection")
	}
	if err := st.SaveFact("ok", ""); err == nil {
		t.Error("SaveFact with empty value succeeded, want rejection")
	}
}
