package store

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Facts are non-secret recall values the user provides once (email, usernames,
// preferences) so a later task doesn't re-ask (PLAN-CREDENTIALS.md sibling; the
// facts half of the recall feature). Unlike secrets they carry no confidentiality
// weight: values are injected as plaintext into the prompt preamble, and they
// live in the ordinary session DB, not the deny-listed vault. The key is a short
// label; there is no env-var/code-exec concern, so validation is only sanity.

// Fact is one stored recall value.
type Fact struct {
	Key       string
	Value     string
	UpdatedAt time.Time
}

// factKeyRe is a short label: a letter/digit start, then word-ish chars. No
// spaces (the key is a single token so `/remember KEY the rest` parses cleanly),
// no control characters (they would corrupt the injected preamble).
var factKeyRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

const maxFactValue = 4096

// ValidFactKey reports whether key is an acceptable fact label. Sanity only —
// facts are non-secret and never become env vars, so this is far looser than
// ValidSecretName; it just keeps the key a clean single token.
func ValidFactKey(key string) error {
	switch {
	case key == "":
		return errors.New("key is empty")
	case len(key) > 64:
		return errors.New("key is too long (max 64)")
	case !factKeyRe.MatchString(key):
		return errors.New("key must be a single token (letters, digits, . _ -), no spaces")
	}
	return nil
}

// SaveFact stores or replaces a recall value under key.
func (s *Store) SaveFact(key, value string) error {
	if err := ValidFactKey(key); err != nil {
		return fmt.Errorf("store: invalid fact key: %w", err)
	}
	if value == "" {
		return errors.New("store: fact value is empty")
	}
	if len(value) > maxFactValue {
		return fmt.Errorf("store: fact value too long (max %d)", maxFactValue)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	_, err := s.db.Exec(
		`INSERT INTO facts (key, value, created_at, updated_at)
         VALUES (?,?,?,?)
         ON CONFLICT(key) DO UPDATE SET
             value=excluded.value,
             updated_at=excluded.updated_at`,
		key, value, now, now)
	if err != nil {
		return fmt.Errorf("store: save fact: %w", err)
	}
	return nil
}

// DeleteFact removes a fact. Returns (false, nil) if none existed.
func (s *Store) DeleteFact(key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM facts WHERE key=?`, key)
	if err != nil {
		return false, fmt.Errorf("store: delete fact: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListFacts returns all facts (key + value), ordered by key. Values are included
// because facts are non-secret and meant to be shown/injected.
func (s *Store) ListFacts() ([]Fact, error) {
	rows, err := s.db.Query(`SELECT key, value, updated_at FROM facts ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("store: list facts: %w", err)
	}
	defer rows.Close()
	var out []Fact
	for rows.Next() {
		var (
			f  Fact
			ts int64
		)
		if err := rows.Scan(&f.Key, &f.Value, &ts); err != nil {
			return nil, fmt.Errorf("store: scan fact: %w", err)
		}
		f.UpdatedAt = time.Unix(ts, 0)
		out = append(out, f)
	}
	return out, rows.Err()
}
