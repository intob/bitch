package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// runHook implements the Claude Code PreToolUse hook (§4.3). Claude Code pipes a
// JSON event on stdin before every tool call; we forward it to the running
// bridge's loopback approval server, which applies the tier policy and (if
// needed) asks over Telegram, then translate the decision into Claude Code's
// PreToolUse contract on stdout.
//
// It fails closed: any error — bridge unreachable, bad config, timeout — emits a
// deny. The permission gate is the primary boundary under full root (§4.6), so a
// broken hook must never default to allow.
func runHook() int {
	addr := os.Getenv("BITCH_HOOK_ADDR")
	secret := os.Getenv("BITCH_HOOK_SECRET")

	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return emit("deny", "hook: could not read stdin")
	}

	// The bridge understands the raw Claude Code payload (session_id, tool_name,
	// tool_input, cwd) directly, so forward it verbatim.
	if addr == "" || secret == "" {
		return emit("deny", "hook: bridge address/secret not set")
	}

	url := "http://" + addr + "/approve"
	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(input))
	if err != nil {
		return emit("deny", "hook: build request failed")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Bitch-Secret", secret)

	// Generous timeout: the bridge may block waiting for a human ✅/❌.
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(httpReq)
	if err != nil {
		return emit("deny", "hook: bridge unreachable")
	}
	defer resp.Body.Close()

	var decision struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decision); err != nil {
		return emit("deny", "hook: bad bridge response")
	}
	if decision.Decision != "allow" {
		return emit("deny", orDefault(decision.Reason, "denied"))
	}
	return emit("allow", orDefault(decision.Reason, "approved"))
}

// emit prints the PreToolUse hook decision JSON and returns exit code 0. Claude
// Code reads permissionDecision from stdout; a "allow" bypasses its own
// permission prompt, "deny" blocks the tool and feeds the reason to the model.
func emit(decision, reason string) int {
	out := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       decision,
			"permissionDecisionReason": reason,
		},
	}
	data, _ := json.Marshal(out)
	fmt.Println(string(data))
	return 0
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
