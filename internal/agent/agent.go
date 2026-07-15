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

	"github.com/intob/bitch/internal/config"
)

// Runner spawns Claude Code processes. One Runner is shared across all chats.
type Runner struct {
	cfg          *config.Config
	exe          string // path to this bitch binary (for the hook subcommand)
	settingsPath string // pre-written settings.json wiring the PreToolUse hook
}

// New creates a Runner and writes the shared settings.json that installs the
// permission hook (§4.3). exe must be the absolute path to the bitch binary.
func New(cfg *config.Config, exe string) (*Runner, error) {
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("agent: state dir: %w", err)
	}
	settingsPath := filepath.Join(cfg.StateDir, "settings.json")
	if err := writeSettings(settingsPath, exe); err != nil {
		return nil, err
	}
	return &Runner{cfg: cfg, exe: exe, settingsPath: settingsPath}, nil
}

// RunRequest describes one agent turn.
type RunRequest struct {
	Prompt          string
	WorkDir         string
	ClaudeSessionID string // empty ⇒ start a fresh session with a new UUID
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
	OnInit     func(sessionID string) // fired once on the init event
	OnText     func(text string)      // an assistant text block completed
	OnToolUse  func(name, summary string)
	OnThinking func()
}

// Run executes one turn, streaming events to cb, and returns the final Result.
// The context governs process lifetime — cancel it to stop the run (§6 /stop).
func (r *Runner) Run(ctx context.Context, req RunRequest, cb Callbacks) (*Result, error) {
	sessionID := req.ClaudeSessionID
	fresh := sessionID == ""
	if fresh {
		sessionID = newUUID()
	}

	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose", // required for stream-json in print mode
		"--model", r.cfg.Model,
		"--permission-mode", "default", // the hook is authoritative (§4.6)
		"--settings", r.settingsPath,
	}
	if r.cfg.EffortFlag() != "" {
		args = append(args, "--effort", r.cfg.EffortFlag())
	}
	if fresh {
		args = append(args, "--session-id", sessionID)
	} else {
		args = append(args, "--resume", sessionID)
	}
	// Confine tool access to the session workspace (§4.3). The hook enforces the
	// jail per-call; --add-dir keeps Claude Code from ranging wider on its own.
	args = append(args, "--add-dir", req.WorkDir)
	args = append(args, r.cfg.ClaudeExtraArgs...)
	args = append(args, req.Prompt)

	cmd := exec.CommandContext(ctx, r.cfg.ClaudeBin, args...)
	cmd.Dir = req.WorkDir
	// Propagate the loopback approval endpoint + secret into the child so the
	// PreToolUse hook (which inherits this env) can call back into the bridge.
	cmd.Env = append(os.Environ(),
		"BITCH_HOOK_ADDR="+r.cfg.HookAddr,
		"BITCH_HOOK_SECRET="+r.cfg.HookSecret,
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("agent: stdout pipe: %w", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

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
					cb.OnToolUse(block.Name, summarizeToolUse(block.Name, block.Input))
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
}

type contentBlock struct {
	Type  string         `json:"type"`
	Text  string         `json:"text"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
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
