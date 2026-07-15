# bitch

A single always-on Claude agent running on a **Hetzner VPS**, controllable from
anywhere over Telegram. Outbound-only transport, constrained permissions to
start, with a deliberate path toward fuller autonomy and (later) a multi-agent
dev pipeline. Hosted in the cloud (not the laptop) so it stays reachable from
your phone even while the laptop travels and goes offline.

**Status:** v1 bridge implemented, reviewed, and hardened — see
[`DESIGN.md`](./DESIGN.md) for the design, [`CODE-REVIEW.md`](./CODE-REVIEW.md)
for the review and its resolution log, and [`DEPLOY.md`](./DEPLOY.md) for the
provisioning plan. Working on this repo? Read [`CLAUDE.md`](./CLAUDE.md) first.

## What it is

- One Claude agent, headless, written in **Go**, hosted on an always-on Hetzner
  VPS (systemd, `Restart=always`).
- Reachable via a Telegram bot; only allowlisted numeric user IDs can drive it —
  everyone else is silently dropped.
- No inbound ports — the bridge long-polls Telegram (outbound HTTPS only).
- **Path B** (DESIGN.md §9.2): the Go bridge spawns the **Claude Code CLI**
  (`claude -p --output-format stream-json`) per turn and parses its event
  stream. The CLI version is **pinned** in `deploy/setup.sh` and preflighted at
  boot — the stream shapes and hook contract are version-coupled.
- Permission tiers T0–T3 (advisory → constrained → trusted → full autonomy).
  Every tool call fires a `PreToolUse` hook that calls back into the bridge;
  the tier policy (`internal/policy`) answers allow / deny / **ask over
  Telegram** (✅/❌ inline buttons, timeout ⇒ deny). Fail-closed at every edge.
- Sessions persist per chat (SQLite): follow-ups `--resume` the same Claude
  session. Keyed `(chat, thread, bot)` — group-ready for the multi-agent layer.
- Daily budget guard ($20 default): new work pauses when the ceiling is hit.

## Layout

```
cmd/bitch/            entrypoint — `serve` (bridge) and `hook` (PreToolUse callback)
internal/config/      env config + validation (allowlist, jail roots, budget)
internal/store/       SQLite session store, keyed (chat, thread, bot)
internal/policy/      the permission gate: tiers, bash gate, workdir jail
internal/agent/       Claude Code CLI orchestration + stream-json parsing
internal/tg/          Telegram output: HTML rendering, chunking, status, approvals
internal/bridge/      glue: poll loop, user gate, commands, loopback approval server
deploy/               systemd unit, provisioning/hardening script, env template
```

## Build & run

```sh
go build ./cmd/bitch          # build
go test ./...                 # tests

# locally: export the env vars from deploy/bitch.env.example, then
./bitch serve
```

Provision a fresh Hetzner box with `deploy/setup.sh` (installs Go, Node, the
pinned Claude Code CLI, hardens SSH/firewall, builds bitch, installs the
systemd unit). Full provisioning flow: [`DEPLOY.md`](./DEPLOY.md).

## Commands (in Telegram)

| Command | Effect |
|---|---|
| `/new [dir]` | start a fresh session (optionally in `dir`) |
| `/cd <path>` | switch working directory (starts a fresh session) |
| `/tier <0-3>` | set permission tier (button-confirmed) |
| `/model <name>` | set model for this chat — `opus`/`sonnet`/`haiku`/`fable`, a full `claude-…` id, or `default` to reset |
| `/effort <level>` | set effort for this chat — `low`/`medium`/`high`/`xhigh`/`max`, or `default` |
| `/stop` | cancel the current turn (kills the whole process group) |
| `/status` | session, dir, tier, model, effort, today's spend |

Anything else is sent to the agent. `/model` and `/effort` are per-session
(they override the `BITCH_MODEL`/`BITCH_EFFORT` defaults) and take effect on the
next turn. Long replies (>12 kB) arrive as a document attachment instead of a
stream of messages.

## Security posture (the short version)

The agent runs as **root on a disposable box** — a deliberate choice
(DESIGN.md §4.6). What holds the line:

- **The permission gate** (`internal/policy`): a whole-string denylist
  (destructive ops, credential paths incl. `/root/.ssh`, network writes), a
  small allowlist of inert commands and read-only/test subcommands, and
  ask-by-default for everything else — including interpreters with inline
  code, network installers (`npx`, `pip install`), redirects, and command
  substitution. The jail compares **physical paths** (symlinks resolved).
- **A scrubbed child environment** — the claude process never sees the
  Telegram token or the bridge's secrets.
- **What the gate doesn't claim to stop:** code the agent writes into the
  workdir and then runs (`go test`, `npm test`). That's inherent to a coding
  agent; the **Hetzner Cloud Firewall egress rules** (enforced outside the VM,
  unflushable from inside) and the rebuild-from-script box bound that residual.

Secrets live in `/etc/bitch/bitch.env` (root-owned, `chmod 600`), injected via
the systemd `EnvironmentFile` — never in git. Rotate the bot token via
@BotFather if it ever leaks.

## Roadmap

Phases 2–5 of DESIGN.md §13 (skeleton, agent, constrained tier + approvals,
hardening) are implemented and reviewed. Next: phase 1 execution (provision the
box, DEPLOY.md), then the multi-agent pipeline layer (§11) on this foundation.
