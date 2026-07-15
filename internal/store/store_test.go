package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestSessionRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	key := Key{ChatID: 1, ThreadID: 0, BotID: 42}

	// Not-found convention.
	if got, err := st.Get(key); err != nil || got != nil {
		t.Fatalf("Get on empty = (%v, %v), want (nil, nil)", got, err)
	}

	in := &Session{Key: key, WorkDir: "/srv/work", Tier: 2, Model: "claude-sonnet-5", Effort: "low"}
	if err := st.Upsert(in); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkDir != "/srv/work" || got.Tier != 2 || got.Model != "claude-sonnet-5" || got.Effort != "low" {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	// Overwrite model/effort (e.g. /model default clears them).
	in.Model, in.Effort = "", ""
	if err := st.Upsert(in); err != nil {
		t.Fatal(err)
	}
	got, _ = st.Get(key)
	if got.Model != "" || got.Effort != "" {
		t.Errorf("clear failed: model=%q effort=%q", got.Model, got.Effort)
	}

	// The claude-session index path also carries model/effort.
	in.Model = "claude-opus-4-8"
	if err := st.Upsert(in); err != nil {
		t.Fatal(err)
	}
	if err := st.SetClaudeSession(key, "sess-xyz"); err != nil {
		t.Fatal(err)
	}
	byID, err := st.GetByClaudeSession("sess-xyz")
	if err != nil || byID == nil {
		t.Fatalf("GetByClaudeSession = (%v, %v)", byID, err)
	}
	if byID.Model != "claude-opus-4-8" || byID.Tier != 2 {
		t.Errorf("GetByClaudeSession mismatch: %+v", byID)
	}
}

// TestMigrateAddsColumns proves an old-schema database (without model/effort)
// is migrated in place rather than erroring on open.
func TestMigrateAddsColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.sqlite")

	// Create a pre-model/effort schema by hand.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`CREATE TABLE sessions (
        chat_id INTEGER NOT NULL, thread_id INTEGER NOT NULL, bot_id INTEGER NOT NULL,
        workdir TEXT NOT NULL, tier INTEGER NOT NULL,
        claude_session_id TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL,
        PRIMARY KEY (chat_id, thread_id, bot_id));
        INSERT INTO sessions VALUES (1,0,42,'/old',1,'',0);`)
	if err != nil {
		t.Fatal(err)
	}
	raw.Close()

	// Open through Store: migrate() must ALTER in the new columns.
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open old-schema db: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	got, err := st.Get(Key{ChatID: 1, ThreadID: 0, BotID: 42})
	if err != nil {
		t.Fatalf("Get after migrate: %v", err)
	}
	if got == nil || got.WorkDir != "/old" || got.Model != "" || got.Effort != "" {
		t.Errorf("migrated row wrong: %+v", got)
	}
}
