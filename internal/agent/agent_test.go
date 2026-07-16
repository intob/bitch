package agent

import (
	"encoding/json"
	"fmt"
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
		`{"type":"assistant","message":{"content":[{"type":"thinking"}],"usage":{"input_tokens":100,"output_tokens":20,"cache_creation_input_tokens":1000,"cache_read_input_tokens":5000}}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"working on it"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"go test ./...\necho done"}}],"usage":{"input_tokens":50,"output_tokens":30}}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":true}]}}`,
		`not json — must be ignored`,
		``,
		`{"type":"result","subtype":"success","is_error":false,"num_turns":3,"total_cost_usd":0.42,"session_id":"sess-1","result":"final answer"}`,
	}, "\n")

	var (
		inits, thinks int
		texts, tools  []string
		results       []string
		used          Usage
	)
	cb := Callbacks{
		OnInit: func(id string) {
			inits++
			if id != "sess-1" {
				t.Errorf("init id = %q", id)
			}
		},
		OnText:    func(s string) { texts = append(texts, s) },
		OnToolUse: func(id, name, summary string) { tools = append(tools, id+":"+name+":"+summary) },
		OnToolResult: func(id string, isError bool) {
			results = append(results, fmt.Sprintf("%s:%t", id, isError))
		},
		OnThinking: func() { thinks++ },
		OnUsage: func(u Usage) {
			used.InputTokens += u.InputTokens
			used.OutputTokens += u.OutputTokens
			used.CacheCreationInputTokens += u.CacheCreationInputTokens
			used.CacheReadInputTokens += u.CacheReadInputTokens
		},
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
	if len(tools) != 1 || tools[0] != "toolu_1:Bash:go test ./... …" {
		t.Errorf("tools = %q", tools)
	}
	if len(results) != 1 || results[0] != "toolu_1:true" {
		t.Errorf("tool results = %q", results)
	}
	// Usage sums across assistant events; the second text-only event carries
	// no usage object and must not fire the callback with zeros.
	if used != (Usage{InputTokens: 150, OutputTokens: 50, CacheCreationInputTokens: 1000, CacheReadInputTokens: 5000}) {
		t.Errorf("usage = %+v", used)
	}
	if used.Total() != 6200 {
		t.Errorf("usage total = %d, want 6200", used.Total())
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
	// Vault secrets (PLAN-CREDENTIALS.md §4.3): a valid one is injected; names
	// that would shadow a base var or load code are refused at injection time
	// even if they somehow reached the map.
	env := r.childEnv(map[string]string{
		"GITHUB_TOKEN": "ghp_abc",
		"LD_PRELOAD":   "/tmp/evil.so",
		"PATH":         "/pwn",
		"BASH_ENV":     "/tmp/rc",
	})

	for _, want := range []string{
		"ANTHROPIC_API_KEY=sk-ant-test",
		"CLAUDE_CODE_SOMETHING=yes",
		"BITCH_HOOK_ADDR=127.0.0.1:1",
		"BITCH_HOOK_SECRET=hs",
		"GITHUB_TOKEN=ghp_abc",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("child env missing %q", want)
		}
	}
	for _, e := range env {
		if strings.HasPrefix(e, "BITCH_TELEGRAM_TOKEN=") || strings.HasPrefix(e, "BITCH_ALLOWED_USERS=") {
			t.Errorf("bridge secret leaked into child env: %s", e)
		}
		if strings.HasPrefix(e, "LD_PRELOAD=") || strings.HasPrefix(e, "BASH_ENV=") || e == "PATH=/pwn" {
			t.Errorf("dangerous secret name reached child env: %s", e)
		}
	}
}

// TestRunAddDirs drives a real Run against a stub CLI that records its argv.
// The media root only works if it reaches the child as its own --add-dir: the
// hook jails each call, but without the flag Claude Code won't range into the
// dir at all and a photo prompt points at a file it refuses to read (§7).
func TestRunAddDirs(t *testing.T) {
	dir := t.TempDir()
	argv := filepath.Join(dir, "argv")
	stub := filepath.Join(dir, "claude")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %s\n"+
		`echo '{"type":"result","subtype":"success","session_id":"s1","result":"ok"}'`+"\n", argv)
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	r, err := New(&config.Config{StateDir: filepath.Join(dir, "state"), ClaudeBin: stub}, "/opt/bitch/bitch")
	if err != nil {
		t.Fatal(err)
	}

	work, media := t.TempDir(), t.TempDir()
	if _, err := r.Run(t.Context(), RunRequest{Prompt: "hi", WorkDir: work, MediaDir: media}, Callbacks{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw, err := os.ReadFile(argv)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(raw)), "\n")

	for _, want := range []string{work, media} {
		i := slices.Index(args, want)
		if i < 1 || args[i-1] != "--add-dir" {
			t.Errorf("%q not passed as --add-dir; argv = %v", want, args)
		}
	}

	// Absent a media root the flag must not appear at all — an empty --add-dir
	// would be the workdir's parent as far as the CLI is concerned.
	if _, err := r.Run(t.Context(), RunRequest{Prompt: "hi", WorkDir: work}, Callbacks{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw, _ = os.ReadFile(argv)
	args = strings.Split(strings.TrimSpace(string(raw)), "\n")
	if n := strings.Count(strings.Join(args, "\n"), "--add-dir"); n != 1 {
		t.Errorf("want exactly one --add-dir without a media root, got %d; argv = %v", n, args)
	}
}

// TestWriteMCPConfig pins the --mcp-config shape (a version-coupled surface,
// §9.2) and the session id inside it: send_file routes on that id, and an MCP
// server has no other way to learn which session it serves (§7).
func TestWriteMCPConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := writeMCPConfig(path, "/opt/bitch/bitch", "127.0.0.1:8765", "hs", "sess-1"); err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("mcp config is not valid JSON: %v", err)
	}
	srv, ok := cfg.MCPServers["bitch"]
	if !ok {
		t.Fatalf("no \"bitch\" server registered: %s", data)
	}
	if srv.Type != "stdio" || srv.Command != "/opt/bitch/bitch" || !slices.Equal(srv.Args, []string{"mcp"}) {
		t.Errorf("server = %+v", srv)
	}
	want := map[string]string{
		"BITCH_HOOK_ADDR":   "127.0.0.1:8765",
		"BITCH_HOOK_SECRET": "hs",
		"BITCH_SESSION_ID":  "sess-1",
	}
	for k, v := range want {
		if srv.Env[k] != v {
			t.Errorf("env[%s] = %q, want %q", k, srv.Env[k], v)
		}
	}
}
