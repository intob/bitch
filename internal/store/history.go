package store

import (
	"fmt"
	"time"
)

// Session history (DESIGN.md §6, PLAN-4-SESSION-HISTORY.md). The sessions table
// holds only the session a key is *currently* on, so /new and /cd used to drop
// the old Claude session id forever even though the conversation itself survives
// on disk. This table remembers every session the bridge has started for a key,
// so /sessions can list them and /resume can hop back.
//
// Rows are per key, so a job's history (its own synthetic key, JobKey) never
// shows up in the owning chat's listing — a job's continuity is the job's
// business, not the human's.

// historyLimit is how many sessions are kept per key. Pruned on insert: this is
// a "hop back to yesterday" list, not an archive, and an unbounded table would
// grow forever for a value nobody scrolls to.
const historyLimit = 20

// SessionRecord is one Claude session the bridge has started for a key.
type SessionRecord struct {
	Key
	ClaudeSessionID string
	WorkDir         string
	Title           string // first line of the session's first prompt, truncated
	CreatedAt       time.Time
	LastUsed        time.Time
}

// RecordSession remembers a newly started Claude session for key, then prunes
// the key's history back to historyLimit. Re-recording an id already present
// refreshes its workdir and last-used stamp rather than duplicating it.
func (s *Store) RecordSession(k Key, claudeID, workdir, title string) error {
	if claudeID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	if _, err := s.db.Exec(
		`INSERT INTO session_history
             (chat_id, thread_id, bot_id, claude_session_id, workdir, title, created_at, last_used)
         VALUES (?,?,?,?,?,?,?,?)
         ON CONFLICT(chat_id, thread_id, bot_id, claude_session_id) DO UPDATE SET
             workdir=excluded.workdir,
             last_used=excluded.last_used`,
		k.ChatID, k.ThreadID, k.BotID, claudeID, workdir, title, now, now); err != nil {
		return fmt.Errorf("store: record session: %w", err)
	}
	if _, err := s.db.Exec(
		`DELETE FROM session_history
          WHERE chat_id=? AND thread_id=? AND bot_id=?
            AND claude_session_id NOT IN (
                SELECT claude_session_id FROM session_history
                 WHERE chat_id=? AND thread_id=? AND bot_id=?
                 ORDER BY last_used DESC, rowid DESC LIMIT ?)`,
		k.ChatID, k.ThreadID, k.BotID, k.ChatID, k.ThreadID, k.BotID, historyLimit); err != nil {
		return fmt.Errorf("store: prune session history: %w", err)
	}
	return nil
}

// TouchSession stamps a session as used now, so /sessions orders by real
// recency rather than by when each session was first started. A missing row is
// not an error: history only starts at the session the bridge itself opened.
func (s *Store) TouchSession(k Key, claudeID string) error {
	if claudeID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`UPDATE session_history SET last_used=?
          WHERE chat_id=? AND thread_id=? AND bot_id=? AND claude_session_id=?`,
		time.Now().Unix(), k.ChatID, k.ThreadID, k.BotID, claudeID)
	if err != nil {
		return fmt.Errorf("store: touch session: %w", err)
	}
	return nil
}

// ListSessions returns up to n of key's sessions, most recently used first.
// Stamps are whole seconds, so rowid breaks ties: two sessions started within
// the same second still list in a stable, newest-first order.
func (s *Store) ListSessions(k Key, n int) ([]SessionRecord, error) {
	if n <= 0 {
		n = historyLimit
	}
	rows, err := s.db.Query(
		`SELECT claude_session_id, workdir, title, created_at, last_used
           FROM session_history
          WHERE chat_id=? AND thread_id=? AND bot_id=?
          ORDER BY last_used DESC, rowid DESC LIMIT ?`,
		k.ChatID, k.ThreadID, k.BotID, n)
	if err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	defer rows.Close()
	var out []SessionRecord
	for rows.Next() {
		rec := SessionRecord{Key: k}
		var created, used int64
		if err := rows.Scan(&rec.ClaudeSessionID, &rec.WorkDir, &rec.Title, &created, &used); err != nil {
			return nil, fmt.Errorf("store: scan session: %w", err)
		}
		rec.CreatedAt = time.Unix(created, 0)
		rec.LastUsed = time.Unix(used, 0)
		out = append(out, rec)
	}
	return out, rows.Err()
}

// DeleteSessionHistory drops one history row — used when a resume turns out to
// point at a session that no longer exists on disk. Returns (false, nil) if
// there was no such row.
func (s *Store) DeleteSessionHistory(k Key, claudeID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(
		`DELETE FROM session_history
          WHERE chat_id=? AND thread_id=? AND bot_id=? AND claude_session_id=?`,
		k.ChatID, k.ThreadID, k.BotID, claudeID)
	if err != nil {
		return false, fmt.Errorf("store: delete session history: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
