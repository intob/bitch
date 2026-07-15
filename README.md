# bitch

A single always-on Claude agent running on a **Hetzner VPS**, controllable from
anywhere over Telegram. Outbound-only transport, constrained permissions to
start, with a deliberate path toward fuller autonomy and (later) a multi-agent
dev pipeline. Hosted in the cloud (not the laptop) so it stays reachable from
your phone even while the laptop travels and goes offline.

**Status:** design phase — see [`DESIGN.md`](./DESIGN.md).

## What it is

- One Claude agent, headless, hosted on an always-on Hetzner VPS (systemd).
- Reachable via a Telegram bot; only an allowlisted user ID can drive it.
- No inbound ports — the bridge long-polls Telegram (outbound HTTPS only).
- Permission tiers from advisory → constrained → full autonomy (§4 of the design).

## Status / roadmap

Build phases are tracked in [`DESIGN.md`](./DESIGN.md) §13:

1. Provision & harden the Hetzner box (SSH key-only, firewall, non-root)
2. Skeleton — bot + user-ID gate + echo
3. Agent wired in (advisory / read-only)
4. Constrained tier — edits in workdir + Telegram approvals
5. Hardening — systemd service, logging, budget guard
6. Later — multi-agent pipeline

## Security note

The bot token and all secrets live outside git (see `.gitignore`). Treat the
token like a root credential; rotate via @BotFather if it ever leaks.
