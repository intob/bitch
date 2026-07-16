package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// runMCP implements the stdio MCP server that gives the child Claude process
// tools to capture credentials and facts the user provides in chat, so a later
// task doesn't have to re-ask (PLAN-CREDENTIALS.md, agent-driven capture). It is
// spawned by Claude Code (registered via --mcp-config; see agent.settings) and
// speaks newline-delimited JSON-RPC 2.0 over stdin/stdout.
//
// The tools persist by POSTing to the bridge's loopback /store endpoint (the
// same shared-secret channel the hook uses). Confirmation happens at the
// PreToolUse gate before the tool is allowed to run — not here.
func runMCP() int {
	addr := os.Getenv("BITCH_HOOK_ADDR")
	secret := os.Getenv("BITCH_HOOK_SECRET")

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 1<<20)
	out := bufio.NewWriter(os.Stdout)
	srv := &mcpServer{addr: addr, secret: secret, w: out}

	for in.Scan() {
		line := bytes.TrimSpace(in.Bytes())
		if len(line) == 0 {
			continue
		}
		srv.handle(line)
		out.Flush()
	}
	return 0
}

type mcpServer struct {
	addr, secret string
	w            *bufio.Writer
}

// --- JSON-RPC envelopes ---------------------------------------------------

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"` // absent for notifications
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (s *mcpServer) handle(line []byte) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return // ignore unparseable noise
	}
	// Notifications (no id) get no response.
	notification := len(req.ID) == 0 || string(req.ID) == "null"

	switch req.Method {
	case "initialize":
		s.reply(req.ID, s.initializeResult(req.Params))
	case "tools/list":
		s.reply(req.ID, s.toolsListResult())
	case "tools/call":
		s.reply(req.ID, s.toolsCallResult(req.Params))
	case "ping":
		s.reply(req.ID, map[string]any{})
	default:
		if !notification {
			s.replyError(req.ID, -32601, "method not found: "+req.Method)
		}
	}
}

func (s *mcpServer) initializeResult(params json.RawMessage) map[string]any {
	// Echo the client's protocol version when given one, else a known default.
	version := "2024-11-05"
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(params, &p) == nil && p.ProtocolVersion != "" {
		version = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "bitch", "version": "1.0.0"},
	}
}

func (s *mcpServer) toolsListResult() map[string]any {
	return map[string]any{"tools": []any{
		map[string]any{
			"name": "remember_fact",
			"description": "Store a NON-SECRET fact the user has provided (email address, " +
				"username, handle, preference) so future tasks don't have to ask again. " +
				"Do NOT use this for passwords, API keys, or tokens — use save_secret for those. " +
				"Call this whenever the user gives you a durable personal detail.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"key":   map[string]any{"type": "string", "description": "short label, e.g. \"email\" or \"github_username\""},
					"value": map[string]any{"type": "string", "description": "the value to remember"},
				},
				"required": []any{"key", "value"},
			},
		},
		map[string]any{
			"name": "save_secret",
			"description": "Store a SECRET credential the user provided (API key, token, password). " +
				"It becomes available to future tasks as the environment variable NAME — reference it " +
				"as $NAME in commands. The value is never shown back to you. Use this instead of " +
				"remembering the value in the conversation, so it survives into unrelated later tasks.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":        map[string]any{"type": "string", "description": "env-var name, UPPER_SNAKE_CASE, e.g. STRIPE_SECRET_KEY"},
					"value":       map[string]any{"type": "string", "description": "the secret value"},
					"description": map[string]any{"type": "string", "description": "optional short note on what it's for"},
				},
				"required": []any{"name", "value"},
			},
		},
	}}
}

func (s *mcpServer) toolsCallResult(params json.RawMessage) map[string]any {
	var p struct {
		Name      string `json:"name"`
		Arguments struct {
			Key         string `json:"key"`
			Name        string `json:"name"`
			Value       string `json:"value"`
			Description string `json:"description"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return toolText("invalid arguments", true)
	}

	var body storePayload
	switch p.Name {
	case "remember_fact":
		body = storePayload{Kind: "fact", Key: p.Arguments.Key, Value: p.Arguments.Value}
	case "save_secret":
		body = storePayload{Kind: "secret", Name: p.Arguments.Name, Value: p.Arguments.Value, Description: p.Arguments.Description}
	default:
		return toolText("unknown tool: "+p.Name, true)
	}

	if err := s.postStore(body); err != nil {
		return toolText("could not store: "+err.Error(), true)
	}
	if body.Kind == "secret" {
		return toolText("Saved. It's available to future tasks as $"+body.Name+".", false)
	}
	return toolText("Remembered "+body.Key+". I'll recall it in future tasks.", false)
}

// storePayload mirrors bridge.storeRequest.
type storePayload struct {
	Kind        string `json:"kind"`
	Key         string `json:"key"`
	Name        string `json:"name"`
	Value       string `json:"value"`
	Description string `json:"description"`
}

func (s *mcpServer) postStore(body storePayload) error {
	if s.addr == "" || s.secret == "" {
		return fmt.Errorf("bridge address/secret not set")
	}
	data, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, "http://"+s.addr+"/store", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Bitch-Secret", s.secret)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("bridge unreachable")
	}
	defer resp.Body.Close()
	var res struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return fmt.Errorf("bad bridge response")
	}
	if !res.OK {
		return fmt.Errorf("%s", orDefault(res.Error, "rejected"))
	}
	return nil
}

// --- JSON-RPC writers -----------------------------------------------------

func (s *mcpServer) reply(id json.RawMessage, result any) {
	if len(id) == 0 {
		return // notification: no response
	}
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (s *mcpServer) replyError(id json.RawMessage, code int, message string) {
	s.write(map[string]any{"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": message}})
}

func (s *mcpServer) write(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.w.Write(data)
	s.w.WriteByte('\n')
}

// toolText builds an MCP tools/call result carrying a single text block.
func toolText(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": isError,
	}
}
