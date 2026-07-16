// Package agent orchestrates the Claude Code CLI — Path B from DESIGN.md §9.2.
// The Go bridge owns Telegram and process lifecycle; Claude Code brings the
// maintained harness (built-in tools, sessions, MCP, and the permission system).
// We spawn `claude -p --output-format stream-json --resume …`, parse the event
// stream, and route risky tool calls back to Telegram via a PreToolUse hook.
package agent

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/intob/bitch/internal/config"
	"github.com/intob/bitch/internal/store"
)

// Runner spawns Claude Code processes. One Runner is shared across all chats.
type Runner struct {
	cfg           *config.Config
	exe           string // path to this bitch binary (for the hook + mcp subcommands)
	settingsPath  string // pre-written settings.json wiring the PreToolUse hook
	mcpConfigPath string // pre-written --mcp-config registering the `bitch mcp` server (§4.7)
}

// New creates a Runner and writes the shared settings.json that installs the
// permission hook (§4.3). exe must be the absolute path to the bitch binary.
func New(cfg *config.Config, exe string) (*Runner, error) {
	// Resolve the state dir to an absolute path: the settings path is passed to
	// the claude child via --settings, and that child runs with its cwd set to
	// the session workspace (not the bridge's cwd), so a relative path would be
	// resolved against the wrong directory and not found.
	stateDir := cfg.StateDir
	if abs, err := filepath.Abs(stateDir); err == nil {
		stateDir = abs
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("agent: state dir: %w", err)
	}
	settingsPath := filepath.Join(stateDir, "settings.json")
	if err := writeSettings(settingsPath, exe); err != nil {
		return nil, err
	}
	// Register the credential/fact capture MCP server (§4.7). The hook secret is
	// resolved by now (bridge.New generates it before constructing the Runner).
	mcpConfigPath := filepath.Join(stateDir, "mcp.json")
	if err := writeMCPConfig(mcpConfigPath, exe, cfg.HookAddr, cfg.HookSecret); err != nil {
		return nil, err
	}
	return &Runner{cfg: cfg, exe: exe, settingsPath: settingsPath, mcpConfigPath: mcpConfigPath}, nil
}

// RunRequest describes one agent turn.
type RunRequest struct {
	Prompt          string
	WorkDir         string
	ClaudeSessionID string // empty ⇒ start a fresh session with a new UUID
	Model           string // empty ⇒ config default (§9.1)
	Effort          string // empty ⇒ config default (§9.1)

	// MediaDir is the inbound-attachment root (§7); when set it gets its own
	// --add-dir so a prompt referencing a downloaded file is readable.
	MediaDir string

	// Credential vault (PLAN-CREDENTIALS.md §4.3). Secrets are injected as env
	// vars into the scrubbed child so a later task doesn't re-ask; Manifest is a
	// names+descriptions preamble (never values) prepended to the prompt so the
	// agent knows which vars exist. The bridge fills both from the store.
	Secrets  map[string]string
	Manifest string
}

// Result is the outcome of a completed turn (from the terminal `result` event).
type Result struct {
	SessionID string
	Text      string
	CostUSD   float64
	NumTurns  int
	IsError   bool
	Subtype   string
}

// Callbacks receive streaming events so the bridge can update the Telegram
// status message (§7). All are optional and called from the Run goroutine.
type Callbacks struct {
	OnInit       func(sessionID string) // fired once on the init event
	OnText       func(text string)      // an assistant text block completed
	OnToolUse    func(id, name, summary string)
	OnToolResult func(toolUseID string, isError bool) // a tool_use resolved
	OnThinking   func()
	OnUsage      func(u Usage) // per-assistant-message token accounting
}

// Usage is the token accounting attached to each assistant message event. One
// assistant event corresponds to one API call, so summing Usage across a turn
// gives the turn's running totals (§7). It feeds the live cost estimate on the
// status card only — the authoritative USD cost is the result event's
// total_cost_usd, which the budget guard uses.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// Total is the all-in token count (input + output + both cache classes).
func (u Usage) Total() int {
	return u.InputTokens + u.OutputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// Run executes one turn, streaming events to cb, and returns the final Result.
// The context governs process lifetime — cancel it to stop the run (§6 /stop).
func (r *Runner) Run(ctx context.Context, req RunRequest, cb Callbacks) (*Result, error) {
	sessionID := req.ClaudeSessionID
	fresh := sessionID == ""
	if fresh {
		sessionID = newUUID()
	}

	// Per-session model/effort override the config default when set (§9.1).
	model := req.Model
	if model == "" {
		model = r.cfg.Model
	}
	effort := req.Effort
	if effort == "" {
		effort = r.cfg.Effort
	}

	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose", // required for stream-json in print mode
		"--model", model,
		"--permission-mode", "default", // the hook is authoritative (§4.6)
		"--settings", r.settingsPath,
		// Register only our capture MCP server (§4.7); strict mode ignores any
		// ambient MCP config, including a .mcp.json inside a cloned repo.
		"--mcp-config", r.mcpConfigPath,
		"--strict-mcp-config",
	}
	if ef := config.NormalizeEffort(effort); ef != "" {
		args = append(args, "--effort", ef)
	}
	if fresh {
		args = append(args, "--session-id", sessionID)
	} else {
		args = append(args, "--resume", sessionID)
	}
	// Confine tool access to the session workspace (§4.3). The hook enforces the
	// jail per-call; --add-dir keeps Claude Code from ranging wider on its own.
	args = append(args, "--add-dir", req.WorkDir)
	if req.MediaDir != "" {
		args = append(args, "--add-dir", req.MediaDir)
	}
	args = append(args, r.cfg.ClaudeExtraArgs...)

	cmd := exec.CommandContext(ctx, r.cfg.ClaudeBin, args...)
	cmd.Dir = req.WorkDir
	cmd.Env = r.childEnv(req.Secrets)
	// Feed the prompt on stdin, not as a trailing positional arg: --add-dir is
	// variadic and would swallow a positional prompt, and stdin is also immune
	// to prompts that begin with '-'. Prepend the credential manifest (§4.3) so
	// the agent knows which env vars hold stored secrets.
	prompt := req.Prompt
	if m := strings.TrimSpace(req.Manifest); m != "" {
		prompt = m + "\n\n" + prompt
	}
	cmd.Stdin = strings.NewReader(prompt)
	// Put claude in its own process group and kill the whole group on cancel:
	// tool children (test runs, builds) must die with it, or /stop leaves them
	// running as root (§6). WaitDelay unblocks Wait if a straggler holds the
	// stdout pipe open after the kill.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 10 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("agent: stdout pipe: %w", err)
	}
	stderr := &tailBuffer{max: 4096}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("agent: start claude: %w", err)
	}

	result := &Result{SessionID: sessionID}
	if cb.OnInit != nil && fresh {
		// We already know the id (we set it); report it up front so the hook
		// can route approvals even during the very first turn.
		cb.OnInit(sessionID)
	}

	scanErr := r.scan(stdout, cb, result)

	waitErr := cmd.Wait()
	if scanErr != nil {
		return result, scanErr
	}
	if waitErr != nil {
		// A cancelled context surfaces here as "signal: killed" — report cleanly.
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, fmt.Errorf("agent: claude exited: %w: %s", waitErr, tail(stderr.String()))
	}
	return result, nil
}

// scan reads the stream-json line protocol and dispatches events.
func (r *Runner) scan(stdout interface{ Read([]byte) (int, error) }, cb Callbacks, result *Result) error {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20) // tolerate large lines (diffs, files)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue // ignore non-JSON noise
		}
		r.dispatch(ev, cb, result)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("agent: read stream: %w", err)
	}
	return nil
}

func (r *Runner) dispatch(ev streamEvent, cb Callbacks, result *Result) {
	switch ev.Type {
	case "system":
		if ev.Subtype == "init" && ev.SessionID != "" {
			result.SessionID = ev.SessionID
			if cb.OnInit != nil {
				cb.OnInit(ev.SessionID)
			}
		}
	case "assistant":
		for _, block := range ev.Message.Content {
			switch block.Type {
			case "text":
				if t := strings.TrimSpace(block.Text); t != "" && cb.OnText != nil {
					cb.OnText(block.Text)
				}
			case "thinking":
				if cb.OnThinking != nil {
					cb.OnThinking()
				}
			case "tool_use":
				if cb.OnToolUse != nil {
					cb.OnToolUse(block.ID, block.Name, summarizeToolUse(block.Name, block.Input))
				}
			}
		}
		if cb.OnUsage != nil && ev.Message.Usage != (Usage{}) {
			cb.OnUsage(ev.Message.Usage)
		}
	case "user":
		// Tool results come back as user-role events; they resolve the pending
		// tool_use lines on the status card (§7).
		if cb.OnToolResult != nil {
			for _, block := range ev.Message.Content {
				if block.Type == "tool_result" {
					cb.OnToolResult(block.ToolUseID, block.IsError)
				}
			}
		}
	case "result":
		result.Subtype = ev.Subtype
		result.IsError = ev.IsError
		result.NumTurns = ev.NumTurns
		result.CostUSD = ev.TotalCostUSD
		if ev.SessionID != "" {
			result.SessionID = ev.SessionID
		}
		if ev.Result != "" {
			result.Text = ev.Result
		}
	}
}

// Preflight verifies the claude CLI is present and answers, returning its
// version string. Run at startup so a PATH problem or a breaking CLI upgrade
// fails the boot loudly instead of failing the first turn — the stream-json
// shapes, hook contract, and flags we use are all version-coupled (§9.2).
func (r *Runner) Preflight(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, r.cfg.ClaudeBin, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("agent: claude CLI preflight (%s --version): %w: %s",
			r.cfg.ClaudeBin, err, tail(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// childEnv builds the claude process environment from scratch rather than
// inheriting the bridge's. The bridge env holds secrets the agent must not see
// — BITCH_TELEGRAM_TOKEN above all (a safe-listed `env` would print them into
// the transcript, §4.6). The child gets: base process vars, ANTHROPIC_*/
// CLAUDE_* for auth and CLI config, and the hook callback endpoint.
//
// secrets are the credential vault (PLAN-CREDENTIALS.md §4.3), injected as env
// vars so tools the agent runs can use them without re-asking. Each name is
// re-validated here (defense in depth): a value that reached the DB out-of-band
// must not inject a code-loading var (LD_PRELOAD, BASH_ENV, …) or shadow a
// base/hook var. Be honest about the posture — an injected value is readable by
// any shell the agent runs; this is a convenience store, not a confidential one.
func (r *Runner) childEnv(secrets map[string]string) []string {
	env := []string{
		"BITCH_HOOK_ADDR=" + r.cfg.HookAddr,
		"BITCH_HOOK_SECRET=" + r.cfg.HookSecret,
	}
	base := map[string]bool{
		"PATH": true, "HOME": true, "USER": true, "LOGNAME": true,
		"SHELL": true, "TMPDIR": true, "LANG": true, "LC_ALL": true,
		"TERM": true, "TZ": true,
	}
	for _, kv := range os.Environ() {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if base[k] || strings.HasPrefix(k, "ANTHROPIC_") || strings.HasPrefix(k, "CLAUDE_") {
			env = append(env, kv)
		}
	}
	for name, value := range secrets {
		if store.ValidSecretName(name) != nil {
			continue // never inject a name that isn't provably safe
		}
		env = append(env, name+"="+value)
	}
	return env
}

// tailBuffer keeps only the last max bytes written. stderr can balloon on a
// pathological run, and only the tail is ever reported.
type tailBuffer struct {
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }

// --- stream-json event shapes -------------------------------------------

type streamEvent struct {
	Type         string       `json:"type"`
	Subtype      string       `json:"subtype"`
	SessionID    string       `json:"session_id"`
	IsError      bool         `json:"is_error"`
	NumTurns     int          `json:"num_turns"`
	TotalCostUSD float64      `json:"total_cost_usd"`
	Result       string       `json:"result"`
	Message      assistantMsg `json:"message"`
}

type assistantMsg struct {
	Content []contentBlock `json:"content"`
	Usage   Usage          `json:"usage"`
}

type contentBlock struct {
	Type      string         `json:"type"`
	ID        string         `json:"id"` // tool_use id, correlates with tool_result
	Text      string         `json:"text"`
	Name      string         `json:"name"`
	Input     map[string]any `json:"input"`
	ToolUseID string         `json:"tool_use_id"` // set on tool_result blocks
	IsError   bool           `json:"is_error"`
}

// summarizeToolUse builds a compact one-liner for the status message (§7).
func summarizeToolUse(name string, input map[string]any) string {
	switch name {
	case "Bash":
		if c, ok := input["command"].(string); ok {
			return firstLine(c)
		}
	case "Read", "Edit", "Write", "MultiEdit":
		if p, ok := input["file_path"].(string); ok {
			return filepath.Base(p)
		}
	case "Grep":
		if p, ok := input["pattern"].(string); ok {
			return p
		}
	case "WebFetch", "WebSearch":
		if u, ok := input["url"].(string); ok {
			return u
		}
		if q, ok := input["query"].(string); ok {
			return q
		}
	}
	return ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	const max = 500
	if len(s) > max {
		return "…" + s[len(s)-max:]
	}
	return s
}

// newUUID returns a random RFC-4122 v4 UUID string.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
