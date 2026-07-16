package store

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// The credential vault (PLAN-CREDENTIALS.md). Secrets are stored on the box and
// injected as environment variables into the scrubbed claude child so a later
// task doesn't have to re-ask for them. The vault DB lives at a hardcoded,
// deny-listed path (/etc/bitch/secrets.db) so the agent can't `cat` it back
// (policy denyPatterns cover /etc/bitch) — but be honest: once injected, a value
// is readable by any shell the agent runs (`echo $X`, `cat /proc/self/environ`).
// This is a convenience store, not a confidential one (DESIGN.md §4.7).

// Secret is one stored credential. Value is omitted from listings.
type Secret struct {
	Name        string
	Value       string
	Description string
	UpdatedAt   time.Time
}

// nameRe is the base charset: an uppercase env-var identifier.
var nameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// baseVars are the process vars childEnv sets itself; a secret must not shadow
// them (agent.childEnv). PATH is also caught by the PATH suffix rule below.
var baseVars = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true,
	"SHELL": true, "TMPDIR": true, "LANG": true, "LC_ALL": true,
	"TERM": true, "TZ": true,
}

// execVars are exact env-var names that change how a subsequently-spawned
// process loads or runs code. A saved secret bearing one of these names would
// run attacker-controlled code in the next root tool subprocess, so they are
// refused. The prefix/suffix rules below (LD_*, *PATH, *_PRELOAD, *_OPTIONS)
// cover the families; this set catches the singletons those miss.
var execVars = map[string]bool{
	"IFS": true, "ENV": true, "BASH_ENV": true, "BASHOPTS": true, "SHELLOPTS": true,
	"PS1": true, "PS2": true, "PS3": true, "PS4": true, "PROMPT_COMMAND": true,
	"GIT_SSH": true, "GIT_SSH_COMMAND": true, "GIT_EXTERNAL_DIFF": true,
	"GIT_PAGER": true, "GIT_EDITOR": true, "GIT_ASKPASS": true, "SSH_ASKPASS": true,
	"PAGER": true, "MANPAGER": true, "EDITOR": true, "VISUAL": true,
	"PYTHONSTARTUP": true, "PYTHONINSPECT": true, "RUBYOPT": true, "RUBYLIB": true,
	"PERL5OPT": true, "PERL5LIB": true, "NODE_OPTIONS": true,
	"DYLD_INSERT_LIBRARIES": true, // macOS analogue of LD_PRELOAD
}

// reservedPrefixes are namespaces the child env owns or that inject code.
var reservedPrefixes = []string{"BITCH_", "ANTHROPIC_", "CLAUDE_", "LD_", "DYLD_"}

// reservedSuffixes catch the code-loading families by shape rather than by an
// exhaustive list: anything that ends up a search path (*PATH), a preloaded
// library (*_PRELOAD), or interpreter options (*_OPTIONS/*_OPTS).
var reservedSuffixes = []string{"PATH", "_PRELOAD", "_OPTIONS", "_OPTS"}

// ValidSecretName reports whether name is safe to store and to inject as an env
// var into the root claude child. It is deliberately a denylist of dangerous
// shapes, not merely a charset check: the name becomes a variable in a
// privileged process, so anything that can redirect code loading is refused
// (PLAN-CREDENTIALS.md §4.3, review finding 2). Enforced at save AND at
// injection time (defense in depth). Returns a human-facing error or nil.
func ValidSecretName(name string) error {
	switch {
	case name == "":
		return errors.New("name is empty")
	case len(name) > 64:
		return errors.New("name is too long (max 64)")
	case !nameRe.MatchString(name):
		return errors.New("name must be UPPER_SNAKE_CASE (start with a letter; A-Z, 0-9, _)")
	case baseVars[name]:
		return fmt.Errorf("%s is a reserved process variable", name)
	case execVars[name]:
		return fmt.Errorf("%s can alter code execution and cannot be stored", name)
	}
	for _, p := range reservedPrefixes {
		if strings.HasPrefix(name, p) {
			return fmt.Errorf("names starting with %s are reserved", p)
		}
	}
	for _, s := range reservedSuffixes {
		if strings.HasSuffix(name, s) {
			return fmt.Errorf("names ending in %s are reserved (code-loading risk)", s)
		}
	}
	return nil
}

// SaveSecret stores or replaces a credential. The name is validated; the value
// is stored verbatim. An empty value is rejected (use DeleteSecret to remove).
func (s *Store) SaveSecret(name, value, description string) error {
	if err := ValidSecretName(name); err != nil {
		return fmt.Errorf("store: invalid secret name: %w", err)
	}
	if value == "" {
		return errors.New("store: secret value is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	_, err := s.db.Exec(
		`INSERT INTO secrets (name, value, description, created_at, updated_at)
         VALUES (?,?,?,?,?)
         ON CONFLICT(name) DO UPDATE SET
             value=excluded.value,
             description=excluded.description,
             updated_at=excluded.updated_at`,
		name, value, description, now, now)
	if err != nil {
		return fmt.Errorf("store: save secret: %w", err)
	}
	return nil
}

// DeleteSecret removes a credential. Returns (false, nil) if none existed.
func (s *Store) DeleteSecret(name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM secrets WHERE name=?`, name)
	if err != nil {
		return false, fmt.Errorf("store: delete secret: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListSecrets returns stored credentials WITHOUT values (name + description),
// ordered by name. Safe to render to the user or hand to the agent as a manifest.
func (s *Store) ListSecrets() ([]Secret, error) {
	rows, err := s.db.Query(
		`SELECT name, description, updated_at FROM secrets ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list secrets: %w", err)
	}
	defer rows.Close()
	var out []Secret
	for rows.Next() {
		var (
			sec Secret
			ts  int64
		)
		if err := rows.Scan(&sec.Name, &sec.Description, &ts); err != nil {
			return nil, fmt.Errorf("store: scan secret: %w", err)
		}
		sec.UpdatedAt = time.Unix(ts, 0)
		out = append(out, sec)
	}
	return out, rows.Err()
}

// AllSecrets returns name→value for every stored credential, for env injection
// into the claude child. Names that fail validation are dropped (defense in
// depth: a value smuggled into the DB out-of-band must not inject a dangerous
// var). Callers must treat the result as sensitive.
func (s *Store) AllSecrets() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT name, value FROM secrets`)
	if err != nil {
		return nil, fmt.Errorf("store: all secrets: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return nil, fmt.Errorf("store: scan secret: %w", err)
		}
		if ValidSecretName(name) != nil {
			continue // never inject a name that isn't provably safe
		}
		out[name] = value
	}
	return out, rows.Err()
}

// GetSecret returns a single secret's value. Intended for bridge-side management
// only; there is deliberately no path that returns a value back to the agent
// (PLAN-CREDENTIALS.md §6 — no secret_get). Returns ("", false, nil) if absent.
func (s *Store) GetSecret(name string) (string, bool, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM secrets WHERE name=?`, name).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: get secret: %w", err)
	}
	return value, true, nil
}
