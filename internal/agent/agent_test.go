package agent

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/intob/bitch/internal/config"
)

// TestNewSettingsPathAbsolute guards the deploy bug where a relative StateDir
// produced a relative --settings path that the claude child (whose cwd is the
// session workspace, not the bridge's) could not resolve.
func TestNewSettingsPathAbsolute(t *testing.T) {
	t.Chdir(t.TempDir())
	r, err := New(&config.Config{StateDir: "data/state"}, "/opt/bitch/bitch")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(r.settingsPath) {
		t.Errorf("settingsPath = %q, want absolute", r.settingsPath)
	}
	if _, err := os.Stat(r.settingsPath); err != nil {
		t.Errorf("settings.json not written: %v", err)
	}
}

// TestScan feeds a canned stream-json transcript through the parser and checks
// that events dispatch to the right callbacks and the terminal result event is
// captured. This is the seam most exposed to CLI version drift (§9.2).
func TestScan(t *testing.T) {
	transcript := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"sess-1"}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"working on it"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./...\necho done"}}]}}`,
		`not json — must be ignored`,
		``,
		`{"type":"result","subtype":"success","is_error":false,"num_turns":3,"total_cost_usd":0.42,"session_id":"sess-1","result":"final answer"}`,
	}, "\n")

	var (
		inits, thinks int
		texts, tools  []string
	)
	cb := Callbacks{
		OnInit: func(id string) {
			inits++
			if id != "sess-1" {
				t.Errorf("init id = %q", id)
			}
		},
		OnText:     func(s string) { texts = append(texts, s) },
		OnToolUse:  func(name, summary string) { tools = append(tools, name+":"+summary) },
		OnThinking: func() { thinks++ },
	}

	r := &Runner{}
	result := &Result{}
	if err := r.scan(strings.NewReader(transcript), cb, result); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if inits != 1 || thinks != 1 {
		t.Errorf("inits=%d thinks=%d, want 1 and 1", inits, thinks)
	}
	if len(texts) != 1 || texts[0] != "working on it" {
		t.Errorf("texts = %q", texts)
	}
	// Multi-line bash commands are summarized to their first line.
	if len(tools) != 1 || tools[0] != "Bash:go test ./... …" {
		t.Errorf("tools = %q", tools)
	}
	if result.SessionID != "sess-1" || result.Text != "final answer" ||
		result.CostUSD != 0.42 || result.NumTurns != 3 || result.IsError {
		t.Errorf("result = %+v", result)
	}
}

// TestChildEnv proves the bridge's secrets do not leak into the claude process
// environment — only base vars, ANTHROPIC_*/CLAUDE_*, and the hook endpoint
// (§4.6: a safe-listed command must never be able to print the bot token).
func TestChildEnv(t *testing.T) {
	t.Setenv("BITCH_TELEGRAM_TOKEN", "super-secret")
	t.Setenv("BITCH_ALLOWED_USERS", "42")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	t.Setenv("CLAUDE_CODE_SOMETHING", "yes")

	r := &Runner{cfg: &config.Config{HookAddr: "127.0.0.1:1", HookSecret: "hs"}}
	env := r.childEnv()

	for _, want := range []string{
		"ANTHROPIC_API_KEY=sk-ant-test",
		"CLAUDE_CODE_SOMETHING=yes",
		"BITCH_HOOK_ADDR=127.0.0.1:1",
		"BITCH_HOOK_SECRET=hs",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("child env missing %q", want)
		}
	}
	for _, e := range env {
		if strings.HasPrefix(e, "BITCH_TELEGRAM_TOKEN=") || strings.HasPrefix(e, "BITCH_ALLOWED_USERS=") {
			t.Errorf("bridge secret leaked into child env: %s", e)
		}
	}
}
