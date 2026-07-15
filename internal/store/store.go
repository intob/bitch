// Package store is the small persistent session store described in DESIGN.md §6.
// Each Telegram (chat, thread, bot) triple maps to one persisted Claude session
// so follow-ups resume context. Keyed group-ready from day one (§11) so the
// multi-agent move is additive. Backed by pure-Go SQLite (no cgo).
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Key identifies a session group-ready: (chat, thread, bot). Group chats have
// negative chat IDs; forum topics add a thread ID (§11).
type Key struct {
	ChatID   int64
	ThreadID int64
	BotID    int64
}

// Session is the persisted per-chat state.
type Session struct {
	Key
	WorkDir         string // current working directory (within the jail)
	Tier            int    // current permission tier 0..3
	ClaudeSessionID string // Claude Code session id to --resume; empty = fresh
	UpdatedAt       time.Time
}

// Store wraps the SQLite database. All methods are safe for concurrent use.
type Store struct {
	db *sql.DB
	mu sync.Mutex // serialize writes; SQLite likes one writer
}

// Open creates (if needed) and opens the store at path, applying the schema.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("store: mkdir %s: %w", dir, err)
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)")
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	db.SetMaxOpenConns(1) // pure-Go SQLite: a single connection avoids lock churn
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS sessions (
    chat_id           INTEGER NOT NULL,
    thread_id         INTEGER NOT NULL,
    bot_id            INTEGER NOT NULL,
    workdir           TEXT    NOT NULL,
    tier              INTEGER NOT NULL,
    claude_session_id TEXT    NOT NULL DEFAULT '',
    updated_at        INTEGER NOT NULL,
    PRIMARY KEY (chat_id, thread_id, bot_id)
);
CREATE INDEX IF NOT EXISTS idx_sessions_claude ON sessions(claude_session_id);

CREATE TABLE IF NOT EXISTS spend (
    day INTEGER NOT NULL PRIMARY KEY, -- days since epoch, UTC
    usd REAL    NOT NULL
);`
	_, err := s.db.Exec(schema)
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// Get returns the session for key, or (nil, nil) if none exists yet.
func (s *Store) Get(k Key) (*Session, error) {
	row := s.db.QueryRow(
		`SELECT workdir, tier, claude_session_id, updated_at FROM sessions
         WHERE chat_id=? AND thread_id=? AND bot_id=?`,
		k.ChatID, k.ThreadID, k.BotID)
	var (
		sess Session
		ts   int64
	)
	sess.Key = k
	err := row.Scan(&sess.WorkDir, &sess.Tier, &sess.ClaudeSessionID, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get: %w", err)
	}
	sess.UpdatedAt = time.Unix(ts, 0)
	return &sess, nil
}

// GetByClaudeSession finds the session whose Claude session id matches. Used by
// the permission hook to route an approval to the right chat (§4.3). Returns
// (nil, nil) if not found.
func (s *Store) GetByClaudeSession(claudeID string) (*Session, error) {
	if claudeID == "" {
		return nil, nil
	}
	row := s.db.QueryRow(
		`SELECT chat_id, thread_id, bot_id, workdir, tier, updated_at FROM sessions
         WHERE claude_session_id=? LIMIT 1`, claudeID)
	var (
		sess Session
		ts   int64
	)
	sess.ClaudeSessionID = claudeID
	err := row.Scan(&sess.ChatID, &sess.ThreadID, &sess.BotID, &sess.WorkDir, &sess.Tier, &ts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get by claude session: %w", err)
	}
	sess.UpdatedAt = time.Unix(ts, 0)
	return &sess, nil
}

// Upsert writes the session, replacing any existing row for the same key.
func (s *Store) Upsert(sess *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO sessions (chat_id, thread_id, bot_id, workdir, tier, claude_session_id, updated_at)
         VALUES (?,?,?,?,?,?,?)
         ON CONFLICT(chat_id, thread_id, bot_id) DO UPDATE SET
             workdir=excluded.workdir,
             tier=excluded.tier,
             claude_session_id=excluded.claude_session_id,
             updated_at=excluded.updated_at`,
		sess.ChatID, sess.ThreadID, sess.BotID, sess.WorkDir, sess.Tier,
		sess.ClaudeSessionID, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("store: upsert: %w", err)
	}
	return nil
}

// SetClaudeSession records the Claude session id for a chat so the next turn can
// --resume it (§6). Called after each run once the id is known.
func (s *Store) SetClaudeSession(k Key, claudeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`UPDATE sessions SET claude_session_id=?, updated_at=?
         WHERE chat_id=? AND thread_id=? AND bot_id=?`,
		claudeID, time.Now().Unix(), k.ChatID, k.ThreadID, k.BotID)
	if err != nil {
		return fmt.Errorf("store: set claude session: %w", err)
	}
	return nil
}

// AddSpend adds usd to today's running total and returns the new total. The
// budget guard (§4.4) reads SpendToday before starting a run.
func (s *Store) AddSpend(usd float64) (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	day := today()
	if _, err := s.db.Exec(
		`INSERT INTO spend (day, usd) VALUES (?, ?)
         ON CONFLICT(day) DO UPDATE SET usd = usd + excluded.usd`,
		day, usd); err != nil {
		return 0, fmt.Errorf("store: add spend: %w", err)
	}
	return s.spendOn(day)
}

// SpendToday returns the total USD spent today (UTC).
func (s *Store) SpendToday() (float64, error) {
	return s.spendOn(today())
}

func (s *Store) spendOn(day int64) (float64, error) {
	var usd float64
	err := s.db.QueryRow(`SELECT usd FROM spend WHERE day=?`, day).Scan(&usd)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: spend: %w", err)
	}
	return usd, nil
}

// today returns the current day as an integer count of days since the Unix
// epoch in UTC, so the budget resets at midnight UTC.
func today() int64 {
	return time.Now().UTC().Unix() / 86400
}
