# PLAN 5 — outbound files: a send_file MCP tool

Status: **proposal, not yet implemented.** Build order: **5 of 6** — last of
the feature plans before voice, because it touches the policy gate and the
version-coupled MCP surface, so it gets the most careful review plus a live
gating test. Authority for intent is `DESIGN.md`; read alongside `CLAUDE.md`
(invariants) and PLAN-CREDENTIALS.md (the capture-tool pattern this mirrors).

## 1. The ask

The agent has no sanctioned way to deliver a binary artifact — a rendered
image, a built binary, a screenshot from a headless browser. `SendDocument`
exists (`sender.go:72`) but only the bridge uses it, for long text replies.

## 2. Design

### Mirror the capture-tool pattern exactly

The `save_secret`/`remember_fact` path is already proven end-to-end: MCP tool
in `bitch mcp` → PreToolUse hook gates it over Telegram → on approval the
tool POSTs to a loopback endpoint with the hook secret → the bridge acts.
`send_file(path, caption)` is the same shape:

- Tool schema + call in `cmd/bitch/mcp.go` (version-coupled surface — the
  CLAUDE.md list already covers the MCP JSON-RPC contract).
- POSTs to a new loopback endpoint `/sendfile` (hook secret required),
  carrying the Claude session id like the capture tools do.
- Bridge resolves session → chat via `GetByClaudeSession` and uploads.

The whole safety of this rests on the same fact as the capture tools: the
PreToolUse hook fires for MCP tools. The existing release-blocker rule for
CLI bumps ("confirm the agent-capture path still gates") now covers
`send_file` too — add it to that live checklist.

### Policy verdict

Add `mcp__bitch__send_file` to `policy.Evaluate`: **Ask at T0/T1, Allow at
T2+.** Rationale: content-wise it is equivalent to the agent printing a file
into the chat, which is already allowed — but a prompt-injected agent
auto-sending files is worth one tap at low tiers, and Ask-by-default is the
house style for anything new. The approval card shows path + size.

### Bridge-side validation is the real boundary (fail closed)

Never trust that the gate ran — re-validate in the `/sendfile` handler, same
defense-in-depth stance as `childEnv` re-checking vault names:

- Re-resolve symlinks and check `WithinRoots` **in the handler** (physical
  paths, per the jail invariant).
- Explicitly re-check the static deny paths (`/etc/bitch`) even though they
  are outside the roots anyway — belt and braces, one line.
- Size cap 50 MB (Telegram bot upload limit). Refusals return a clear error
  string through the MCP result so the agent can adapt (e.g. compress).
- Regular files only (no devices, no dirs).

### Rendering

- `.png/.jpg/.jpeg/.gif/.webp` under 10 MB → `SendPhoto` (inline preview).
- Everything else → `SendDocument`, generalized to take the artifact's
  basename instead of the hardcoded `reply.md`.
- Failure of the photo path falls back to document (Telegram rejects some
  technically-valid images).

### Discoverability

The capture tools are hidden behind ToolSearch by the server-side deferred
gate, so `captureNudge` (`internal/bridge/facts.go`) must mention
`send_file` alongside `save_secret`/`remember_fact` — otherwise the agent
won't know it exists.

## 3. Changes

- `cmd/bitch/mcp.go` — `send_file` tool schema + `tools/call` handler
  (POST to `/sendfile`).
- `internal/bridge/sendfile.go` (new) — endpoint: secret check, session →
  chat routing, path/size/type validation, upload dispatch.
- `internal/tg/sender.go` — `SendPhoto`; `SendDocument` takes a filename.
- `internal/policy/policy.go` — verdict for `mcp__bitch__send_file`.
- `internal/bridge/facts.go` — `captureNudge` mentions the tool.
- `DESIGN.md` §7 (outbound files) + the CLI-bump live checklist in
  `CLAUDE.md`; same commit.

## 4. Tests

One test per fail-closed rule, per convention:

- Policy: verdict per tier for `mcp__bitch__send_file`.
- Endpoint: rejects bad/missing secret; unknown session; out-of-jail path;
  symlinked escape from inside a root; `/etc/bitch` path; oversize file;
  directory/device. Accepts a happy-path file (upload behind an interface).
- MCP: `tools/list` includes the schema; `tools/call` marshals args
  (extend `mcp_test.go`).
- **Live (release checklist):** at T1, ask the agent to send a file →
  confirm the ✅/❌ card appears **before** the file arrives.

## 5. Invariants check

- **Fail closed** — every validation failure refuses; no path defaults to
  send.
- **Jail compares physical paths** — handler resolves symlinks before the
  root check.
- **Vault posture unchanged** — this adds no path that returns secret
  *values* to the agent; `/etc/bitch` stays unreadable and unsendable. (The
  agent can already print any env-injected secret into chat; send_file does
  not widen that.)
- **Poll loop never blocks** — `/sendfile` is served by the approval
  server's goroutine pool, and the upload happens there, not in the poll
  loop.
