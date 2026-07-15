# bitch — Code Review: concept → design → v1 implementation

**Reviewer:** Claude (Fable 5) · **Date:** 2026-07-16 · **Scope:** DESIGN.md,
DEPLOY.md, all Go source (~2.5k lines), deploy artifacts. `go build`, `go vet`,
`go test ./...` all pass clean.

---

> **Resolution log (2026-07-16, same day):** every finding below was addressed
> in the follow-up hardening pass. Summary of what changed:
>
> 1. **§3.1 (critical, approval deadlock)** — agent turns now run in their own
>    goroutine; the poll loop keeps draining approval callbacks and `/stop`.
>    The loopback listener also binds synchronously in `Run` so a bad
>    `BITCH_HOOK_ADDR` fails startup (replaces the goroutine `log.Fatalf`).
> 2. **§4 (policy tighten)** — `safeCommands` stripped to inert utilities;
>    interpreters (`python`, `node`, `awk`, `sed`), installers (`npx`, `pip`,
>    `npm install`), and `env` now Ask. New `safeSubcommands` allowlists
>    read-only/test subcommands for `git`/`go`/`npm`/`cargo`/`pip`. File
>    redirects (`>`) and command substitution (`` $() ``/backticks) demote to
>    Ask. Deny patterns extended: `/root/.ssh`+`/root/.aws`, `/etc/bitch`,
>    `find -delete/-exec`. Child env is built from scratch (`agent.childEnv`)
>    — the Telegram token is no longer visible to the agent (test-asserted).
>    The jail now compares physical paths: symlinks resolved in
>    `policy.resolve` (deepest-existing-ancestor for new files),
>    `bridge.resolveDir`, and roots canonicalized in `config.Load`
>    (test-asserted). DESIGN.md §4.3/§4.6 rewritten to state honestly what the
>    gate does and does not stop.
> 3. **CLI pinning** — `CLAUDE_VERSION=2.1.210` pinned in `deploy/setup.sh`;
>    the bridge preflights `claude --version` at boot and fails loudly.
> 4. **Process lifecycle** — claude runs in its own process group;
>    cancellation SIGKILLs the group; `WaitDelay` set.
> 5. **Telegram robustness** — `Status.Set` self-throttles (1.5s min gap,
>    deferred flush); chunk sends retry once before the plain-text fallback.
> 6. **Misc** — `KillBash`/`KillShell` reclassified as inert; stderr capped at
>    4 kB (`tailBuffer`); replies >12 kB sent as document attachments.
> 7. **Tests** — policy suite expanded (subcommands, redirects, substitution,
>    `/root` paths, symlink-jail escape), new stream-parser test, new
>    env-scrub test, new config tests (fail-closed loading, root
>    canonicalization). `go build`, `go vet`, `go test ./...` green.
>
> **Not yet done (needs a live bot):** the end-to-end approval verification —
> send a risky prompt at T1 and confirm the ✅ resolves promptly. Do this
> during the DEPLOY.md rollout (§4 post-setup checklist covers it).

## TL;DR

- **Was Go a good choice?** Yes, with one honest caveat — see §1.
- **Is shelling out to Claude Code a good solution?** Yes, it's the strongest
  decision in the design. The risks are version-coupling, not architecture — §2.
- **Will it be smooth and stable?** Not yet *(as reviewed — since fixed, see
  the resolution log above)*. There was **one critical bug** that broke the
  approval flow and `/stop` entirely (§3.1), and the bash policy had holes
  that matter a lot under the full-root posture (§4). Both were fixable
  without touching the architecture.

The overall shape — Go bridge, long-poll transport, Claude Code CLI as the agent
harness, PreToolUse hook → loopback HTTP → Telegram approval, SQLite session
store — is sound, well-documented, and unusually well-commented. The design doc
and code agree with each other, which is rarer than it should be.

---

## 1. Was Go a good choice?

**Yes for the bridge; it did force the architecture.**

What Go buys here is real and matches this workload:

- A single static ARM64 binary — deploy is `scp` + systemd, no runtime on the
  box beyond what Claude Code itself needs anyway (Node).
- The bridge is an I/O multiplexer (Telegram long-poll, child process streams,
  loopback HTTP, SQLite) — exactly Go's home turf. Goroutines + `context`
  cancellation map 1:1 onto "one turn per chat, cancellable by /stop".
- `modernc.org/sqlite` (pure Go, no cgo) keeps cross-compilation trivial.
- For an always-on unattended service, Go's boring-and-predictable runtime is a
  genuine stability feature.

The caveat, which DESIGN.md §9 states honestly: **there is no Claude Agent SDK
for Go.** The TypeScript/Python Agent SDK gives you `canUseTool` (an in-process
permission callback), typed streaming events, and session management natively.
Choosing Go is what made Path B necessary, and Path B is why the repo contains
~400 lines of plumbing the SDK would have provided: the `bitch hook`
subcommand, the loopback approval server + shared secret, generated
`settings.json`, and hand-rolled `stream-json` parsing. A TS implementation
would have been smaller and less coupled to CLI internals.

**Verdict:** the trade was taken knowingly and the price was paid in plumbing
that is now written, tested, and small. The operational advantages (deployment,
concurrency, single binary) are permanent; the SDK gap was a one-time cost.
No reason to revisit.

## 2. Is shelling out to Claude Code a good solution?

**Yes — it's the best part of the design.** Spawning
`claude -p --output-format stream-json --resume …` per turn means the bridge
inherits Anthropic's maintained tools, session persistence, MCP support, and
permission machinery instead of owning bash/file plumbing and its security
(Path A). Process-per-turn also gives free crash isolation: a wedged agent turn
dies with its process; the bridge lives on.

The hook design is genuinely good:

- **Fail-closed everywhere.** Unknown session → deny. Bridge unreachable →
  deny. Bad response → deny. Timeout → deny (`cmd/bitch/hook.go`).
- **Degrades safe, not open.** If the hook silently failed to install (e.g. a
  CLI change), `-p` mode with `--permission-mode default` cannot prompt
  interactively, so writes/bash would be *denied*, not allowed. A broken gate
  yields a read-only agent, not an ungated one. Nice property.
- The loopback server binds 127.0.0.1 with a per-boot shared secret.

The real risks are **contract-coupling**, not architecture:

1. **The CLI is an implicit, unpinned dependency.** `deploy/setup.sh` installs
   `@anthropic-ai/claude-code` at whatever version npm serves that day. The
   bridge depends on: the `stream-json` event shapes, the PreToolUse stdin
   payload and `hookSpecificOutput.permissionDecision` output contract, and the
   `--session-id` / `--resume` / `--effort` / `--settings` flags. Any of these
   can shift between CLI releases. **Recommendation:** pin the version
   (`npm install -g @anthropic-ai/claude-code@X.Y.Z`), record it in the env
   file, and upgrade deliberately. Add a startup smoke test (`claude --version`
   + a trivial `-p` run) so drift is caught at boot, not mid-turn.
2. **Tool-name coupling in `internal/policy`.** `isReadTool`/`isEditTool` match
   today's tool names. A renamed or newly added tool falls into the
   unknown→risky bucket — which asks rather than allows, so this fails safe,
   just noisily. Fine.
3. **Per-turn spawn cost** (~1s of CLI startup) is irrelevant next to model
   latency. Non-issue.

**Verdict:** right call. Pin the CLI version — that's the single cheapest
stability win available.

## 3. Will it be smooth and stable? — Correctness findings

### 3.1 CRITICAL — the update loop is single-threaded, so approvals deadlock and `/stop` never works

`bridge.Run` (`internal/bridge/bridge.go:85-90`) processes updates like this:

```go
case update, ok := <-updates:
    b.handleUpdate(ctx, update)   // ← synchronous
```

`handleUpdate → handleMessage → handleAgentTurn → runner.Run` blocks until the
entire Claude turn finishes. While a turn is running, **no other Telegram
update is processed**. Consequences:

- **Every human approval deadlocks into a timeout-deny.** The claude process
  blocks on the hook; the hook blocks on `/approve`; `askHuman` blocks waiting
  for the ✅/❌ button callback — but that callback arrives on the very updates
  channel the loop is no longer draining. The button press sits in the channel
  until the turn ends. Net effect: at T1, every risky op waits the full
  `ApprovalTimeout` (5 min default) and then denies, no matter how fast you tap
  ✅. The core UX of the whole project — "ask over Telegram" — cannot work.
- **`/stop` is unreachable while anything is running**, which is the only time
  it matters.
- One busy chat blocks all other chats (and the "⏳ still working" busy-check in
  `handleAgentTurn` is dead code — a second message can never even be read
  while the first turn runs).

**Fix:** run agent turns in their own goroutine —
`go b.handleAgentTurn(ctx, msg, key, prompt)` — keeping the existing
`b.runs`/mutex busy-gate (which already anticipates concurrency). Commands and
callbacks stay synchronous on the loop; they're fast. This is a one-line change
plus a careful look at what `handleAgentTurn` touches (it's already
mutex-guarded, and `Sender` calls are safe to make concurrently).

This must be fixed and then verified live (send a `git push`-ish prompt at T1
and tap ✅) before the Hetzner deploy is called done.

### 3.2 `/stop` kills the CLI but can orphan its children

`exec.CommandContext` cancellation SIGKILLs the `claude` process only. Tool
children it spawned (a running `npm test`, a build) are re-parented and keep
running as root. Set `cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}`
and `cmd.Cancel = func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }`
to kill the process group, and add a `cmd.WaitDelay` so a stuck stdout reader
can't hang `Wait()` forever.

### 3.3 Telegram rate limits are only half-handled

`SendMarkdown` sleeps 1.1s between chunks (good), but `Status.Set` fires an
`editMessageText` per tool-use event with no throttle — a tool-heavy turn will
trip 429s. The library (`go-telegram-bot-api` v5.5.1, effectively unmaintained)
does not retry 429s; errors are silently dropped (`sender.go:104`,
`ResolveApproval`, `AnswerCallback` all discard errors). Failure mode is
cosmetic (stale status text) except when the *final answer* send fails — that's
logged but the user just sees silence. Worth: throttling `Set` to ~1/sec, and
retrying the final-answer send. Longer-term consider `telebot` or `bot`
(go-telegram/bot), both maintained.

### 3.4 Smaller correctness notes

- `serveApprovals` calls `log.Fatalf` from a goroutine if the loopback listen
  fails (`approve.go:50`) — kills the bridge mid-flight (systemd restarts it,
  possibly into the same error loop). Return the error to `Run` instead.
- On `--resume`, the CLI mints a *new* session id; the bridge only learns it
  when the `init` event is parsed. A hook firing before that line is processed
  would fail-closed ("no matching session"). In practice init precedes tool
  calls by seconds — acceptable, but worth knowing the deny-reason exists.
- `stderr` is accumulated unbounded (`agent.go:112`); a pathological run could
  balloon memory. Cap it (you already have `tail()`).
- `SendDocument` (§7's "big artifacts as attachments") is implemented but never
  called — long diffs currently arrive as many 3900-char `<pre>` chunks at
  1.1s intervals. Wire it up for results over ~3 chunks.
- Two chats can race the budget check and both start (single-user; ignorable).
- `internal/agent`, `config`, `store` have no tests. The tests that exist
  (policy, chunker, decide-through-store) are well-chosen; `config.WithinRoots`
  and the stream parser (feed it a canned stream-json transcript) would be the
  next highest-value ones.

## 4. Security review — the policy gate vs. the full-root posture

DESIGN.md §4.6 is clear-eyed that under full root, **`internal/policy` is the
primary boundary** ("only the Telegram approval hook stands between a risky
tool call and root execution"). Measured against that job, the current gate has
holes that are individually small but collectively decisive — a
prompt-injected agent (a stated first-class threat, §4.3) escapes T1
containment through the `safeCommands` allowlist without ever triggering an
approval:

1. **Interpreters are allowlisted.** `python3`, `node`, `npx`, `awk`, `sed`
   are all "safe" (`policy.go:180-196`). But `python3 -c '…'` is arbitrary code
   as root — it can read `/root/.ssh`, rewrite the systemd unit, or exfiltrate
   over HTTPS, none of which the regex denylist sees. `npx <pkg>` and
   `pip install <pkg>` *download and execute arbitrary code* auto-allowed at
   T1. `awk 'BEGIN{system("…")}'` is a shell escape. A leading-token classifier
   cannot contain a Turing-complete interpreter; these need to move to the
   ask-tier (or be allowed only with specific argument shapes, e.g. bare
   `python3 -m pytest`).
2. **`git` is blanket-safe** with only `push`/`remote` deny-patterned.
   `git clean -fdx`, `git reset --hard`, `git checkout .` are destructive;
   `git config core.sshCommand`/`credential.helper` is an execution/credential
   primitive. Allowlist specific read subcommands (`status`, `diff`, `log`,
   `show`, `branch`, `fetch`) instead.
3. **`find` is safe** → `find / -delete` auto-runs at T1 as root. (`-exec rm
   -rf` happens to be caught by the whole-string `rm -rf` regex; `-delete` is
   not.)
4. **Redirections aren't inspected.** `echo x > /etc/systemd/system/bitch.service`
   leads with safe `echo`. Similarly `sed -i` writes any file. The bash jail
   only exists for tools with `file_path` args; Bash file writes are ungated.
5. **`(~|\$HOME)/\.ssh` misses `/root/.ssh`** — and the agent runs as root, so
   its literal home path is the one the pattern doesn't match. Same for
   `/root/.aws` etc. Add `/root/` variants (`policy.go:172-173`).
6. **`env` is safe-listed and the child env is the bridge's full env**
   (`agent.go:103`) — one auto-allowed `env` prints `BITCH_TELEGRAM_TOKEN`
   (design calls it a root credential), `ANTHROPIC_API_KEY`, and
   `BITCH_HOOK_SECRET` into the transcript. Fix both ends: build the child env
   explicitly (PATH, HOME, `ANTHROPIC_API_KEY`, the two hook vars — not the
   Telegram token), and drop `env` from the safe list.
7. **The jail is lexical, not physical.** `resolve()` uses `filepath.Clean`,
   not `EvalSymlinks` — a symlink inside `/srv/work` pointing at `/` turns
   every Read/Edit "inside" the jail. Creating the symlink needs an approved
   `ln`, so it's a two-step, but resolve symlinks before `WithinRoots` anyway.

None of this says the *architecture* is wrong — the ask-flow, fail-closed
hook, tier model, and jail are all right. It says the **allowlist is tuned for
convenience at a level the threat model doesn't support**. Two coherent ways to
respond:

- **Tighten** (keep the posture): shrink `safeCommands` to genuinely inert
  commands, gate interpreters/package-managers behind Ask, inspect redirects,
  scrub the child env, fix the `/root` patterns. Cost: more Telegram taps at
  T1 — which is what T1 is *for*.
- **Or lean on the backstops** (accept the gate is best-effort): the design's
  own compensating controls — outside-the-VM egress firewall, scoped git
  credentials, disposable box — already assume the gate can fail. That's
  honest, but then §4.6's "the permission gate is the primary boundary" should
  be rewritten, because today it's a speed bump, not a boundary.

I'd do the tighten pass; it's an afternoon of work in one pure, well-tested
file. Also worth stating in DESIGN.md: even under the strictest T1, running
unreviewed model output as root means the *real* guarantees come from the
egress firewall and disposability — which the doc already half-says.

One more deploy-time note: `DEPLOY.md` has secrets pasted into the chat
transcript by design (full-auto mode). It already says to rotate the Hetzner
token afterward — extend that to *treat the Anthropic key and bot token as
transcript-exposed too* and rotate them post-setup if the transcript persists
anywhere.

## 5. What's good (worth keeping exactly as-is)

- **Docs–code coherence.** Every package header cites the design section it
  implements, and the implementation actually matches the doc. DEPLOY.md is
  written to be resumable by a fresh session. This is exemplary.
- **Config fails fast and closed** — refuses to start with an empty allowlist
  or missing roots, with error messages that explain *why* (`config.go:99`).
- **The store** is small and right: WAL, single writer, busy timeout, group-
  ready key, `(nil, nil)` not-found convention, spend ledger keyed by UTC day.
- **The chunker** (`internal/tg/render.go`) handles the genuinely fiddly cases:
  never splits a fence, closes every `<pre>`, hard-splits oversized lines,
  protects inline code from bold rewriting, and has tests.
- **Fail-closed as a reflex**, not a slogan — unknown tools ask, unknown
  sessions deny, hook errors deny, T0 denies risky outright.
- Sensible defaults everywhere: $20/day budget on by default, T1 default tier,
  5-minute approval timeout, tier changes require button confirmation.

## 6. Prioritized action list

| # | Priority | Action | Status |
|---|---|---|---|
| 1 | **Blocking** | Run agent turns in a goroutine (§3.1) — approvals and `/stop` are broken without it. Verify live end-to-end. | ✅ done (live verify pending deploy) |
| 2 | **Blocking** | Policy tighten pass (§4): drop interpreters/`git`-blanket/`find`/`env` from safe list, add `/root/.ssh` patterns, scrub child env. | ✅ done |
| 3 | High | Pin the Claude Code CLI version in setup.sh; add a startup CLI smoke check (§2). | ✅ done (2.1.210) |
| 4 | High | Kill the process group on cancel; add `WaitDelay` (§3.2). | ✅ done |
| 5 | Medium | Throttle `Status.Set`; retry the final-answer send (§3.3). | ✅ done |
| 6 | Medium | Resolve symlinks before the jail check (§4.7); return listen errors instead of `log.Fatalf` (§3.4). | ✅ done |
| 7 | Low | Wire up `SendDocument` for long outputs; cap stderr; add stream-parser + config tests. | ✅ done |

## 7. Bottom line

Go: right tool, honestly costed. Shelling out to Claude Code: the correct
architecture — it borrows a maintained, security-critical harness instead of
rebuilding one, and its hook contract is used properly (fail-closed at every
edge). Smooth and stable: the transport/lifecycle/persistence layers are ready
for it, but not until the synchronous update loop is fixed (right now the
flagship feature — approve-over-Telegram — times out to deny every time) and
the bash allowlist is brought in line with the "primary security boundary" job
the full-root posture assigns it. Both fixes are local, neither disturbs the
design, and the codebase is easily good enough to carry the multi-agent layer
planned on top of it.
