// Package config loads and validates the bitch bridge configuration from the
// environment. Every secret and every knob lives here so the rest of the code
// never touches os.Getenv directly. Secrets are injected via the systemd
// EnvironmentFile (DESIGN.md §4.2, §8) and never committed.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Config is the fully-resolved runtime configuration.
type Config struct {
	// Telegram
	TelegramToken string  // BITCH_TELEGRAM_TOKEN — bot token from @BotFather.
	AllowedUsers  []int64 // BITCH_ALLOWED_USERS — hard allowlist of numeric user IDs (§4.1).

	// Workspace jail
	Roots      []string // BITCH_ROOTS — absolute roots edits/reads are confined to (§4.3).
	DefaultDir string   // BITCH_DEFAULT_DIR — initial working directory for a new session.

	// Model
	Model           string   // BITCH_MODEL — e.g. claude-opus-4-8 (§9.1).
	Effort          string   // BITCH_EFFORT — low|medium|high|xhigh|max (§9.1); empty ⇒ CLI default.
	ClaudeBin       string   // BITCH_CLAUDE_BIN — path to the claude CLI (default: "claude").
	ClaudeExtraArgs []string // BITCH_CLAUDE_EXTRA_ARGS — extra flags passed through to claude.

	// Policy
	DefaultTier     int           // BITCH_DEFAULT_TIER — starting permission tier (§4.3), default 1.
	ApprovalTimeout time.Duration // BITCH_APPROVAL_TIMEOUT — how long an approval waits before deny (§4.3).

	// Budget guard (§4.4)
	DailyBudgetUSD float64 // BITCH_DAILY_BUDGET_USD — per-day spend ceiling; 0 disables.

	// Internals
	HookAddr   string // BITCH_HOOK_ADDR — loopback addr for the approval callback server (§4.3).
	HookSecret string // BITCH_HOOK_SECRET — shared secret gating the loopback server; auto-generated if empty.
	DBPath     string // BITCH_DB_PATH — SQLite session store path (§6).
	StateDir   string // BITCH_STATE_DIR — scratch dir for per-run settings.json files.

	// SecretsPath is the credential vault DB (PLAN-CREDENTIALS.md §4.2). It is
	// deliberately NOT env-configurable: the policy deny that keeps the agent from
	// reading it is a static /etc/bitch literal (policy.go), so a movable path
	// would fail open. Overridable only in tests, via SecretsPathForTest.
	SecretsPath string
}

// DefaultSecretsPath is the fixed vault location. It sits under /etc/bitch, which
// the policy gate already denies the agent from reading (§4.5/§4.7).
const DefaultSecretsPath = "/etc/bitch/secrets.db"

// Load reads configuration from the environment, applies defaults, and
// validates. It fails fast: a misconfigured security-critical value should stop
// the process, not silently open a hole.
func Load() (*Config, error) {
	c := &Config{
		TelegramToken:   os.Getenv("BITCH_TELEGRAM_TOKEN"),
		DefaultDir:      os.Getenv("BITCH_DEFAULT_DIR"),
		Model:           envOr("BITCH_MODEL", "claude-opus-4-8"),
		Effort:          envOr("BITCH_EFFORT", "high"),
		ClaudeBin:       envOr("BITCH_CLAUDE_BIN", "claude"),
		HookAddr:        envOr("BITCH_HOOK_ADDR", "127.0.0.1:8765"),
		HookSecret:      os.Getenv("BITCH_HOOK_SECRET"),
		DBPath:          envOr("BITCH_DB_PATH", "data/bitch.sqlite"),
		StateDir:        envOr("BITCH_STATE_DIR", "data/state"),
		SecretsPath:     DefaultSecretsPath, // fixed by design; see the field comment
		DailyBudgetUSD:  envFloat("BITCH_DAILY_BUDGET_USD", 20),
		DefaultTier:     int(envInt("BITCH_DEFAULT_TIER", 1)),
		ApprovalTimeout: time.Duration(envInt("BITCH_APPROVAL_TIMEOUT", 300)) * time.Second,
	}

	if extra := strings.TrimSpace(os.Getenv("BITCH_CLAUDE_EXTRA_ARGS")); extra != "" {
		c.ClaudeExtraArgs = strings.Fields(extra)
	}

	// Allowlist — the single most security-critical value. Refuse to run open.
	for _, f := range splitList(os.Getenv("BITCH_ALLOWED_USERS")) {
		id, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("BITCH_ALLOWED_USERS: %q is not a numeric user ID", f)
		}
		c.AllowedUsers = append(c.AllowedUsers, id)
	}

	// Roots — the workdir jail. Canonicalized (symlinks resolved) because the
	// policy jail check compares physical paths (§4.3).
	for _, r := range splitList(os.Getenv("BITCH_ROOTS")) {
		abs, err := filepath.Abs(r)
		if err != nil {
			return nil, fmt.Errorf("BITCH_ROOTS: %q: %w", r, err)
		}
		abs = filepath.Clean(abs)
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			abs = resolved
		}
		c.Roots = append(c.Roots, abs)
	}

	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) validate() error {
	if c.TelegramToken == "" {
		return fmt.Errorf("BITCH_TELEGRAM_TOKEN is required")
	}
	if len(c.AllowedUsers) == 0 {
		return fmt.Errorf("BITCH_ALLOWED_USERS is required and must be non-empty (an open bot is a remote-execution hole, §4.1)")
	}
	if len(c.Roots) == 0 {
		return fmt.Errorf("BITCH_ROOTS is required (the workdir jail root, §4.3)")
	}
	if c.DefaultDir == "" {
		c.DefaultDir = c.Roots[0]
	}
	abs, err := filepath.Abs(c.DefaultDir)
	if err != nil {
		return fmt.Errorf("BITCH_DEFAULT_DIR: %w", err)
	}
	c.DefaultDir = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(c.DefaultDir); err == nil {
		c.DefaultDir = resolved
	}
	if !c.WithinRoots(c.DefaultDir) {
		return fmt.Errorf("BITCH_DEFAULT_DIR %q is not within any BITCH_ROOTS", c.DefaultDir)
	}
	if c.DefaultTier < 0 || c.DefaultTier > 3 {
		return fmt.Errorf("BITCH_DEFAULT_TIER must be 0..3, got %d", c.DefaultTier)
	}
	return nil
}

// NormalizeEffort returns effort if it is a valid `claude --effort` level, else
// "" (let the CLI default stand). Shared by config validation and the /effort
// command (§9.1).
func NormalizeEffort(effort string) string {
	switch effort {
	case "low", "medium", "high", "xhigh", "max":
		return effort
	default:
		return ""
	}
}

// EffortFlag returns the validated configured effort level, or "" (§9.1
// defaults to "high").
func (c *Config) EffortFlag() string { return NormalizeEffort(c.Effort) }

// WithinRoots reports whether an absolute, cleaned path is inside a configured
// root. This is the load-bearing check behind the workdir jail (§4.3); callers
// must pass an already-resolved absolute path.
func (c *Config) WithinRoots(path string) bool {
	for _, root := range c.Roots {
		if path == root {
			return true
		}
		if strings.HasPrefix(path, root+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// AllowedUser reports whether a numeric Telegram user ID may drive the bot.
func (c *Config) AllowedUser(id int64) bool {
	return slices.Contains(c.AllowedUsers, id)
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int64) int64 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

// splitList splits a comma- or whitespace-separated list, dropping empties.
func splitList(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}
