package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestMCP returns an mcpServer writing into buf and pointed at a stub bridge
// that records the last /store payload.
func newTestMCP(t *testing.T, buf *bytes.Buffer) (*mcpServer, *storePayload) {
	t.Helper()
	var last storePayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Bitch-Secret") != "hs" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&last)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	return &mcpServer{addr: addr, secret: "hs", sessionID: "sess-abc", w: bufio.NewWriter(buf)}, &last
}

// decodeReplies splits the server's newline-delimited JSON-RPC output.
func decodeReplies(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad JSON-RPC line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestMCPInitializeAndList(t *testing.T) {
	var buf bytes.Buffer
	srv, _ := newTestMCP(t, &buf)

	srv.handle([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`))
	srv.handle([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)) // no reply expected
	srv.handle([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	srv.w.Flush()

	replies := decodeReplies(t, &buf)
	if len(replies) != 2 {
		t.Fatalf("got %d replies, want 2 (notification must not reply)", len(replies))
	}
	// initialize echoes the requested protocol version.
	res0, _ := replies[0]["result"].(map[string]any)
	if res0["protocolVersion"] != "2025-06-18" {
		t.Errorf("initialize protocolVersion = %v", res0["protocolVersion"])
	}
	// tools/list advertises every tool the agent is expected to reach for.
	for _, tool := range []string{"remember_fact", "save_secret", "send_file"} {
		if !strings.Contains(buf.String(), tool) {
			t.Errorf("tools/list missing %s: %s", tool, buf.String())
		}
	}
}

func TestMCPToolCallStores(t *testing.T) {
	var buf bytes.Buffer
	srv, last := newTestMCP(t, &buf)

	srv.handle([]byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"save_secret","arguments":{"name":"STRIPE_KEY","value":"sk_live_x","description":"payments"}}}`))
	srv.w.Flush()

	if last.Kind != "secret" || last.Name != "STRIPE_KEY" || last.Value != "sk_live_x" || last.Description != "payments" {
		t.Fatalf("store payload = %+v", *last)
	}
	replies := decodeReplies(t, &buf)
	if len(replies) != 1 {
		t.Fatalf("got %d replies, want 1", len(replies))
	}
	res, _ := replies[0]["result"].(map[string]any)
	if res["isError"] != false {
		t.Errorf("tool call reported error: %+v", res)
	}
	// The success text must not echo the secret value back to the model.
	if strings.Contains(buf.String(), "sk_live_x") {
		t.Errorf("secret value echoed in tool result: %s", buf.String())
	}
}

func TestMCPFactCall(t *testing.T) {
	var buf bytes.Buffer
	srv, last := newTestMCP(t, &buf)

	srv.handle([]byte(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"remember_fact","arguments":{"key":"email","value":"joey@rmrf.uk"}}}`))
	srv.w.Flush()

	if last.Kind != "fact" || last.Key != "email" || last.Value != "joey@rmrf.uk" {
		t.Fatalf("store payload = %+v", *last)
	}
}

func TestMCPUnknownMethodErrors(t *testing.T) {
	var buf bytes.Buffer
	srv, _ := newTestMCP(t, &buf)

	srv.handle([]byte(`{"jsonrpc":"2.0","id":3,"method":"does/not/exist"}`))
	srv.w.Flush()

	replies := decodeReplies(t, &buf)
	if len(replies) != 1 {
		t.Fatalf("got %d replies, want 1", len(replies))
	}
	if _, hasErr := replies[0]["error"]; !hasErr {
		t.Errorf("unknown method should return an error object: %+v", replies[0])
	}
}

// TestMCPSendFileCall proves send_file posts the path, the caption, and the
// session id the server was registered with — the id is what routes the file to
// a chat, so a call without one must not reach the bridge at all.
func TestMCPSendFileCall(t *testing.T) {
	var buf bytes.Buffer
	var last sendFilePayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sendfile" {
			t.Errorf("posted to %s, want /sendfile", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&last)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	s := &mcpServer{
		addr:      strings.TrimPrefix(srv.URL, "http://"),
		secret:    "hs",
		sessionID: "sess-abc",
		w:         bufio.NewWriter(&buf),
	}

	s.handle([]byte(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"send_file","arguments":{"path":"out.png","caption":"the chart"}}}`))
	s.w.Flush()

	if last.SessionID != "sess-abc" || last.Path != "out.png" || last.Caption != "the chart" {
		t.Fatalf("sendfile payload = %+v", last)
	}
	res, _ := decodeReplies(t, &buf)[0]["result"].(map[string]any)
	if res["isError"] != false {
		t.Errorf("send_file reported error: %+v", res)
	}

	// No session id: fail closed, and don't post.
	last = sendFilePayload{}
	buf.Reset()
	s.sessionID = ""
	s.handle([]byte(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"send_file","arguments":{"path":"out.png"}}}`))
	s.w.Flush()
	if last.Path != "" {
		t.Errorf("posted despite having no session id: %+v", last)
	}
	res, _ = decodeReplies(t, &buf)[0]["result"].(map[string]any)
	if res["isError"] != true {
		t.Errorf("send_file without a session should error: %+v", res)
	}
}

// TestMCPBridgeRefusalReachesAgent: the bridge's validation message is what the
// agent needs in order to adapt (compress, or pick an in-workspace path).
func TestMCPBridgeRefusalReachesAgent(t *testing.T) {
	var buf bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"/etc/shadow is outside the allowed roots"}`))
	}))
	defer srv.Close()
	s := &mcpServer{addr: strings.TrimPrefix(srv.URL, "http://"), secret: "hs", sessionID: "s1", w: bufio.NewWriter(&buf)}

	s.handle([]byte(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"send_file","arguments":{"path":"/etc/shadow"}}}`))
	s.w.Flush()

	res, _ := decodeReplies(t, &buf)[0]["result"].(map[string]any)
	if res["isError"] != true {
		t.Fatalf("refusal should be an error result: %+v", res)
	}
	if !strings.Contains(buf.String(), "outside the allowed roots") {
		t.Errorf("bridge refusal did not reach the agent: %s", buf.String())
	}
}
