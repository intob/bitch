# bitch

A single always-on Claude agent running on this Mac, controllable from anywhere
over Telegram. Outbound-only transport, constrained permissions to start, with a
deliberate path toward fuller autonomy and (later) a multi-agent dev pipeline.

**Status:** design phase — see [`DESIGN.md`](./DESIGN.md).

## What it is

- One Claude agent, headless, hosted on this laptop (lid closed, under launchd).
- Reachable via a Telegram bot; only an allowlisted user ID can drive it.
- No inbound ports — the bridge long-polls Telegram (outbound HTTPS only).
- Permission tiers from advisory → constrained → full autonomy (§4 of the design).

## Status / roadmap

Build phases are tracked in [`DESIGN.md`](./DESIGN.md) §13:

1. Skeleton — bot + user-ID gate + echo
2. Agent wired in (advisory / read-only)
3. Constrained tier — edits in workdir + Telegram approvals
4. Hardening — launchd, lid-sleep config, logging, budget guard
5. Later — multi-agent pipeline

## Security note

The bot token and all secrets live outside git (see `.gitignore`). Treat the
token like a root credential; rotate via @BotFather if it ever leaks.
