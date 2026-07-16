package store

import (
	"fmt"
	"testing"
	"time"
)

var histKey = Key{ChatID: 7, ThreadID: 0, BotID: 42}

// histPast is well before any wall clock a test runs under, so a real
// TouchSession (which stamps time.Now) always sorts ahead of a pinned row.
var histPast = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

func TestSessionHistoryRoundTrip(t *testing.T) {
	st := openTestStore(t)

	// Not-found convention: an empty history is an empty list, not an error.
	if got, err := st.ListSessions(histKey, 0); err != nil || len(got) != 0 {
		t.Fatalf("ListSessions on empty = (%v, %v), want (empty, nil)", got, err)
	}

	if err := st.RecordSession(histKey, "sess-a", "/srv/work", "fix the policy test flake"); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordSession(histKey, "sess-b", "/srv/other", "write the deploy runbook"); err != nil {
		t.Fatal(err)
	}
	// Pin the stamps: they're whole seconds, and both records land in the same
	// one, so the ordering assertions below would otherwise ride on the tiebreak.
	if err := st.setLastUsedForTest(histKey, "sess-a", histPast); err != nil {
		t.Fatal(err)
	}
	if err := st.setLastUsedForTest(histKey, "sess-b", histPast.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	got, err := st.ListSessions(histKey, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ListSessions = %d rows, want 2", len(got))
	}
	// Newest first.
	if got[0].ClaudeSessionID != "sess-b" {
		t.Errorf("first row = %q, want the most recently used (sess-b)", got[0].ClaudeSessionID)
	}
	if got[0].WorkDir != "/srv/other" || got[0].Title != "write the deploy runbook" {
		t.Errorf("row = %+v, want the recorded dir and title", got[0])
	}
	if got[0].Key != histKey {
		t.Errorf("row key = %+v, want %+v", got[0].Key, histKey)
	}
	if got[0].CreatedAt.IsZero() || got[0].LastUsed.IsZero() {
		t.Errorf("row timestamps = (%v, %v), want both set", got[0].CreatedAt, got[0].LastUsed)
	}

	// Touch reorders: /sessions must rank by real recency, not by first use.
	if err := st.TouchSession(histKey, "sess-a"); err != nil {
		t.Fatal(err)
	}
	got, err = st.ListSessions(histKey, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].ClaudeSessionID != "sess-a" {
		t.Errorf("after touching sess-a, first row = %q, want sess-a", got[0].ClaudeSessionID)
	}

	// Delete drops exactly one row, and reports whether there was one.
	ok, err := st.DeleteSessionHistory(histKey, "sess-a")
	if err != nil || !ok {
		t.Fatalf("DeleteSessionHistory = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, _ := st.DeleteSessionHistory(histKey, "sess-a"); ok {
		t.Error("deleting a missing row = true, want false")
	}
	got, _ = st.ListSessions(histKey, 0)
	if len(got) != 1 || got[0].ClaudeSessionID != "sess-b" {
		t.Errorf("after delete = %+v, want only sess-b", got)
	}
}

// Re-recording a known id refreshes it rather than duplicating it — the id is
// part of the primary key.
func TestRecordSessionIsIdempotent(t *testing.T) {
	st := openTestStore(t)
	if err := st.RecordSession(histKey, "sess-a", "/srv/work", "first"); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordSession(histKey, "sess-a", "/srv/moved", "second"); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListSessions(histKey, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("ListSessions = %d rows, want 1", len(got))
	}
	if got[0].WorkDir != "/srv/moved" {
		t.Errorf("workdir = %q, want the refreshed /srv/moved", got[0].WorkDir)
	}
	// The title is the session's opening prompt; it doesn't change under it.
	if got[0].Title != "first" {
		t.Errorf("title = %q, want the original %q", got[0].Title, "first")
	}
}

// History is a hop-back list, not an archive: it stays bounded per key.
func TestRecordSessionPrunesToLimit(t *testing.T) {
	st := openTestStore(t)
	total := historyLimit + 5
	for i := range total {
		id := fmt.Sprintf("sess-%02d", i)
		if err := st.RecordSession(histKey, id, "/srv/work", id); err != nil {
			t.Fatal(err)
		}
		// last_used has second granularity; keep the order unambiguous.
		if err := st.setLastUsedForTest(histKey, id, histPast.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	// Re-record so the prune runs against the fixed stamps above.
	if err := st.RecordSession(histKey, "sess-new", "/srv/work", "newest"); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListSessions(histKey, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != historyLimit {
		t.Fatalf("history = %d rows, want it pruned to %d", len(got), historyLimit)
	}
	if got[0].ClaudeSessionID != "sess-new" {
		t.Errorf("newest row = %q, want sess-new", got[0].ClaudeSessionID)
	}
	// The oldest ones are what got dropped.
	for _, rec := range got {
		if rec.ClaudeSessionID == "sess-00" {
			t.Error("prune kept the oldest session, want it dropped")
		}
	}
}

// A job's sessions live on their own synthetic key (§6.1), so the owning chat's
// listing must not show them — a job's continuity is the job's business.
func TestListSessionsIsPerKey(t *testing.T) {
	st := openTestStore(t)
	jobKey := JobKey(histKey.ChatID, histKey.BotID, 3)
	if err := st.RecordSession(histKey, "sess-chat", "/srv/work", "interactive"); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordSession(jobKey, "sess-job", "/srv/work", "nightly tests"); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListSessions(histKey, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ClaudeSessionID != "sess-chat" {
		t.Errorf("chat listing = %+v, want only the interactive session", got)
	}
	// The job's own history is still there, under its own key.
	if got, err = st.ListSessions(jobKey, 0); err != nil || len(got) != 1 {
		t.Errorf("job listing = (%+v, %v), want its one session", got, err)
	}
}

func TestListSessionsHonoursLimit(t *testing.T) {
	st := openTestStore(t)
	for i := range 5 {
		if err := st.RecordSession(histKey, fmt.Sprintf("sess-%d", i), "/srv/work", "x"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.ListSessions(histKey, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("ListSessions(k, 2) = %d rows, want 2", len(got))
	}
}

// An empty id is the "no session yet" sentinel everywhere else in the store; it
// must never become a history row.
func TestRecordAndTouchIgnoreEmptyID(t *testing.T) {
	st := openTestStore(t)
	if err := st.RecordSession(histKey, "", "/srv/work", "nope"); err != nil {
		t.Fatal(err)
	}
	if err := st.TouchSession(histKey, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.ListSessions(histKey, 0); len(got) != 0 {
		t.Errorf("history = %+v, want empty", got)
	}
}

// Touching a session the bridge never recorded is a no-op, not an error.
func TestTouchUnknownSession(t *testing.T) {
	st := openTestStore(t)
	if err := st.TouchSession(histKey, "sess-unknown"); err != nil {
		t.Errorf("TouchSession on an unknown id = %v, want nil", err)
	}
}

// setLastUsedForTest pins a row's recency so ordering assertions don't race the
// one-second granularity of the real stamps.
func (s *Store) setLastUsedForTest(k Key, claudeID string, when time.Time) error {
	_, err := s.db.Exec(
		`UPDATE session_history SET last_used=?
          WHERE chat_id=? AND thread_id=? AND bot_id=? AND claude_session_id=?`,
		when.Unix(), k.ChatID, k.ThreadID, k.BotID, claudeID)
	return err
}
