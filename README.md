# bitch

A single always-on Claude agent running on a **Hetzner VPS**, controllable from
anywhere over Telegram. Outbound-only transport, constrained permissions to
start, with a deliberate path toward fuller autonomy and (later) a multi-agent
dev pipeline. Hosted in the cloud (not the laptop) so it stays reachable from
your phone even while the laptop travels and goes offline.

**Status:** v1 bridge implemented — see [`DESIGN.md`](./DESIGN.md) for the full design.

## What it is

- One Claude agent, headless, written in **Go**, hosted on an always-on Hetzner VPS (systemd).
- Reachable via a Telegram bot; only an allowlisted user ID can drive it.
- No inbound ports — the bridge long-polls Telegram (outbound HTTPS only).
- Permission tiers from advisory → constrained → full autonomy (§4 of the design).
- **Path B** (§9.2): the Go bridge orchestrates the **Claude Code CLI**; risky
  tool calls fire a `PreToolUse` hook that asks you over Telegram.

## Layout

```
cmd/bitch/            entrypoint — `serve` (bridge) and `hook` (PreToolUse callback)
internal/config/      env config + validation (allowlist, roots, budget)
internal/store/       SQLite session store, keyed (chat, thread, bot) — group-ready
internal/policy/      the permission gate: tiers, bash denylist/allowlist, workdir jail
internal/agent/       Claude Code CLI orchestration + stream-json parsing
internal/tg/          Telegram output: HTML rendering, 4096-char chunking, approvals
internal/bridge/      glue: poll loop, user-ID gate, commands, loopback approval server
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
Claude Code CLI, hardens SSH/firewall, builds bitch, installs the systemd unit).

## Commands (in Telegram)

`/new [dir]` · `/cd <path>` · `/tier <0-3>` (with confirm) · `/stop` · `/status`.
Anything else is sent to the agent.

## Roadmap

Build phases in [`DESIGN.md`](./DESIGN.md) §13. Phases 2–5 (skeleton, agent,
constrained tier + approvals, hardening) are implemented; phase 6 (multi-agent
pipeline) is the deliberate next layer on this foundation (§11).

## Security note

The bot token and all secrets live outside git (see `.gitignore`) — injected via
the systemd `EnvironmentFile` (root-owned, `chmod 600`). Treat the token like a
root credential; rotate via @BotFather if it ever leaks. Under the full-root
posture (§4.6) the permission gate (`internal/policy`) and the Hetzner Cloud
Firewall are the load-bearing controls.
