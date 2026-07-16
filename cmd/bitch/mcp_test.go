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
	return &mcpServer{addr: addr, secret: "hs", w: bufio.NewWriter(buf)}, &last
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
	// tools/list advertises both capture tools.
	if !strings.Contains(buf.String(), "remember_fact") || !strings.Contains(buf.String(), "save_secret") {
		t.Errorf("tools/list missing a tool: %s", buf.String())
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
