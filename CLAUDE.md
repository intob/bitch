# CLAUDE.md — working on bitch

bitch is a Go bridge between Telegram and the Claude Code CLI: one always-on
agent on a Hetzner VPS, driven from a phone, with a tiered permission gate that
asks the human over Telegram before risky operations. `DESIGN.md` is the
authority on intent; `DEPLOY.md` is the provisioning runbook; `CODE-REVIEW.md`
records the 2026-07-16 review and what was fixed.

## Commands

```sh
go build ./cmd/bitch     # build the single binary (serve + hook + mcp subcommands)
go test ./...            # unit tests — must stay green
go vet ./...             # clean
./deploy/redeploy.sh     # ship a code change to the live box (build+scp+restart+verify+rollback)
```

Redeploy is binary-copy, not `git pull`: the box's `/root/bitch` is a plain
source tree, so `redeploy.sh` cross-compiles here and ships `/opt/bitch/bitch`.
Re-run `deploy/setup.sh` instead when the systemd unit, env schema, or CLI pin
changes. Deployment specifics (host, key) live in `DEPLOY.md`. Don't log individual
deploys anywhere — no "deployed commit" pins, no redeploy history entries.

There is no linter config beyond `go vet`. The module path is
`github.com/intob/bitch`. The user's global `~/go.work` includes this repo;
there is no repo-local go.work.

Live testing needs a real bot token + allowlisted user ID (see
`deploy/bitch.env.example`); unit tests cover everything that doesn't need
Telegram. The Ask/approval path can only be exercised live: send a risky
prompt at T1 (e.g. ask it to `git push`) and confirm the ✅/❌ card resolves.

## Architecture (one turn, end to end)

```
Telegram user → bridge poll loop (bridge.Run)
  → handleAgentTurn (own goroutine, one per chat via b.runs)
    → agent.Runner.Run spawns: claude -p --output-format stream-json
                               --resume <id> --settings <generated settings.json>
                               --mcp-config <generated mcp.json> --strict-mcp-config
      → every tool call fires PreToolUse hook → `bitch hook` subprocess
        → POST 127.0.0.1:8765/approve (shared secret)
      → capture tools (save_secret/remember_fact) via `bitch mcp` subprocess
        → POST 127.0.0.1:8765/store (shared secret) after the gate approves
          → bridge.decide → policy.Evaluate → Allow / Deny / Ask
            → Ask: approval card in Telegram, blocks until ✅/❌ or timeout(deny)
    → stream events → status message edits + chunked HTML replies
    → result event → cost recorded (budget guard), session id persisted
```

Packages: `config` (env + validation), `store` (SQLite, pure-Go driver),
`policy` (the permission gate — pure, heavily tested), `agent` (CLI
orchestration + stream parsing), `tg` (rendering/chunking + senders),
`bridge` (glue + loopback approval server), `cmd/bitch` (main + hook).

## Invariants — do not break these

**Fail closed, everywhere.** Unknown session → deny. Hook can't reach the
bridge → deny. Approval timeout → deny. Unknown tool or unparseable command →
ask. Config missing a security-critical value → refuse to start. Any change
that makes an error path default to "allow" is wrong.

**The poll loop must never block.** Agent turns run in their own goroutine
(`go b.handleAgentTurn(…)`); approval button callbacks arrive on the same
updates channel the loop drains. If the loop blocks on a turn, approvals
deadlock into timeout-denies — this was the worst bug in the original v1
(CODE-REVIEW.md §3.1). Commands/callbacks stay synchronous; they're fast.

**The policy allowlist stays small.** The agent runs as root; `internal/policy`
is the primary boundary (DESIGN.md §4.6). The rule for `safeCommands` /
`safeSubcommands`: nothing that can (a) interpret inline code (`python -c`,
`node -e`, `awk`), (b) fetch-and-execute from the network (`npx`,
`pip/npm install`, `go run`), (c) write arbitrary paths (`sed -i`, `>`
redirects), or (d) print secrets (`env`). Running **workdir** code
(`go test`, `npm test`, `make`) is allowed by design — that residual is
carried by the egress firewall, not the gate. Deny patterns scan the whole
command string on purpose (they catch badness inside arguments); remember
`/root/…` variants — the agent's HOME is /root, not `~`.

**The jail compares physical paths.** Roots are canonicalized in
`config.Load`; `policy.resolve` and `bridge.resolveDir` resolve symlinks
(including for not-yet-existing files, via deepest-existing-ancestor). Never
compare unresolved paths against roots.

**The claude child env is built from scratch** (`agent.childEnv`): base vars +
`ANTHROPIC_*`/`CLAUDE_*` + the two hook vars + the credential vault (§4.7). Never
pass `os.Environ()` — the bridge env holds `BITCH_TELEGRAM_TOKEN`, and a
safe-listed command must never be able to print it (there's a test asserting
this). Vault secrets are the one *curated* addition: every injected name is
re-checked through `store.ValidSecretName` at injection time, because a name
becomes an env var in the root child — a saved `LD_PRELOAD`/`BASH_ENV`/`PATH`
would be code-exec or a hijack. That validator (denylist of code-loading and
reserved names) is security-critical; keep it strict, and keep the re-check in
`childEnv` even though save-time also validates (defense in depth).

**The credential vault is a convenience store, not a confidential one** (§4.7).
Stored secrets are injected as env vars so a later task doesn't re-ask — but an
env var is readable by any shell the agent runs (`echo $X`), so treat every
stored credential as agent-readable and workdir-code-reachable; the disposable
box is the backstop, not the gate. It lives in a **separate** SQLite DB at the
fixed, non-configurable `/etc/bitch/secrets.db` (a movable path would escape the
static `/etc/bitch` deny and fail open). Never add a path that returns a value
back to the agent (no `secret_get`).

**Recall has two stores; don't conflate them** (§4.7). The **vault** (secrets,
above) is for sensitive values — env-injected, values never in the manifest,
strict `ValidSecretName`, separate deny-listed DB. **Facts**
(`internal/store/facts.go`, `/remember`/`/forget`) are for non-secret recall
(email, usernames) — plaintext values injected straight into the prompt preamble,
stored in the ordinary session DB, only sanity key-validation. If a value would
be harmful to print into a transcript, it's a secret, not a fact. The per-turn
preamble is `joinPreamble(factsBlock, secretsManifest)`; both are fail-open on a
read error (the agent just re-asks) — never abort a turn over recall.

**The CLI is pinned and version-coupled.** `CLAUDE_VERSION` in
`deploy/setup.sh` (currently 2.1.210) must match what the code was tested
against. The coupled surfaces: stream-json event shapes (`agent.streamEvent`),
the PreToolUse stdin payload + `hookSpecificOutput.permissionDecision` output
(`cmd/bitch/hook.go`), the stdio **MCP** JSON-RPC contract (`cmd/bitch/mcp.go`:
`initialize`/`tools/list`/`tools/call`) + the `--mcp-config` file shape
(`agent.writeMCPConfig`), and flags
`--session-id/--resume/--effort/--settings/--mcp-config/--strict-mcp-config`.
When bumping the pin: rerun `TestScan`, then a live turn with an approval, **and**
confirm the agent-capture path still gates — the whole safety of agent-driven
`save_secret`/`remember_fact` rests on the PreToolUse hook firing for MCP tools
(so the save asks over Telegram before it runs). If a CLI bump stops firing hooks
for MCP tools, agent-driven saves would become unconfirmed — treat that as a
release blocker, not a nicety.
The bridge preflights `claude --version` at boot — keep that.

**Process lifecycle:** claude runs in its own process group; cancel kills the
whole group (`-pgid`) so `/stop` can't orphan tool children running as root.
`WaitDelay` stays set so a straggler holding the pipe can't hang `Wait`.

**Telegram discipline:** HTML parse mode only; every reply goes through
`tg.Chunk` (never split a code fence, budget 3900); `Status` self-throttles
edits (editGap); sends retry once before the plain-text fallback; replies
>12 kB (`docThreshold`) go as document attachments. `tgbotapi` v5.5.1 is
unmaintained and does not retry 429s — the retry/throttle layers compensate;
don't remove them. Forum topics aren't supported by this lib version, so
`ThreadID` is always 0 (keying is already group-ready).

**One turn per chat** via the `b.runs` map; a second prompt while busy gets
"still working". Session continuity: `/new` and `/cd` clear
`ClaudeSessionID` (Claude Code sessions are project-scoped); the id persisted
on init routes hook approvals back to the right chat — a fresh session's id is
generated bridge-side and stored *before* the first tool call can fire.

**Model/effort are per-session** (stored on `store.Session`, empty ⇒ config
default). `/model` and `/effort` set them; `agent.Run` resolves the fallback to
`cfg.Model`/`cfg.Effort` — the bridge just passes `sess.Model`/`sess.Effort`
through, so the default lives in exactly one place. `/model` accepts friendly
aliases (`normalizeModel`) or any `claude-…` id (passed through — the CLI is the
final validator); `/effort` reuses `config.NormalizeEffort`. Neither needs a
confirm (unlike `/tier`, they don't change the security posture). When adding a
Session field, thread it through all three store queries (`Get`,
`GetByClaudeSession`, `Upsert`) and the `CREATE TABLE` **and** an idempotent
`ALTER TABLE` in `migrate` (existing DBs on the box won't re-run CREATE).

## Conventions

- Comments cite design sections (`§4.3`) — keep doing this; it's what makes
  DESIGN.md and the code stay honest against each other. Update DESIGN.md in
  the same commit when behavior diverges from it.
- `internal/policy` stays dependency-free and testable (its only impurity is
  symlink resolution); the bridge passes `config.WithinRoots` in as a closure.
- Secrets come from the environment (`config.Load`) and, at runtime, the
  credential vault (`internal/store/secrets.go`, §4.7); nothing else reads
  `os.Getenv`.
- Table-driven tests; security behavior gets a test per rule (see
  `policy_test.go` for the pattern).

## Known limitations / deferred

- Phase 1 (Hetzner provisioning) is planned but not executed — DEPLOY.md is
  the runbook and is blocked on user-provided secrets.
- Multi-agent pipeline (DESIGN.md §11) is a future layer; don't preempt it,
  but don't break group-readiness (session keying, `/cmd@bot` stripping).
- Two chats can race the daily budget check (single-user; accepted).
- `stripTags` plain-text fallback is lossy for legitimate `<` in text
  (accepted; it's a last-resort path).
- `captureNudge` (`internal/bridge/facts.go`) is prepended to **every** turn's
  prompt so the agent knows the deferred `save_secret`/`remember_fact` MCP tools
  exist (they're hidden behind `ToolSearch` by the server-side
  `tengu_deferred_stub_tool` gate; see the constant's comment). It's ~110 words
  on the hot path for all turns. Future: scope it so it's not paid every turn —
  e.g. only prepend when the pinned CLI actually defers MCP tools (detect once at
  boot, cache), only on fresh sessions (session-resume keeps earlier context), or
  only until the session has used a capture tool once. Left unconditional for now
  because capture correctness matters more than a small per-turn prompt cost.
