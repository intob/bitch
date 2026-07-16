# PLAN 5 — outbound files: a send_file MCP tool

Status: **implemented, 2026-07-16** — see DESIGN.md §7 (outbound files), which is
now the authority; this doc is kept as the rationale record. Build order: **5 of
6**. The live gating test (§4) is still owed: it needs a real bot, and it is the
release gate for this feature.

Shipped as planned, with two deltas:

- **Session routing had to be built, not reused.** The plan assumed the capture
  tools carry a Claude session id — they don't, and don't need to: facts and
  secrets are global, so `/store` never had to name a chat. `send_file` does, and
  an MCP server (unlike the hook, which gets `session_id` on stdin) is told
  nothing about its session. So `--mcp-config` became **per-run**
  (`agent.writeMCPConfig` now takes a sessionID, `Run` writes and removes the
  file), passing `BITCH_SESSION_ID` explicitly rather than leaning on the CLI's
  env inheritance — which would couple chat routing to an undocumented detail of
  how the pinned CLI spawns MCP servers. No session id ⇒ the tool refuses before
  it posts.
- **The approval card shows path + caption, not path + size.** `policy` is pure
  (its one impurity is symlink resolution) and a size means a stat; the bridge
  checks size where it matters, at the send. Keeping the gate off the filesystem
  was worth more than the number on the card.

`SendDocument` already took a filename, so only `SendPhoto` was new.

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
