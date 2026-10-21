# bitch

Drive a [Claude Code](https://docs.claude.com/en/docs/claude-code) agent from
Telegram. bitch is a single Go binary that runs on an always-on Linux box, takes
your messages from a Telegram bot, runs them as Claude Code turns, and streams
the work back into the chat. Before risky operations it **asks you over
Telegram** (✅/❌) instead of just doing them.

Run it on any always-on Linux machine you're willing to give an agent root
on: a VPS, a home server, a spare box. You talk to it from your phone.

## What you get

- **A Claude Code agent in a chat.** Each message is a turn, and follow-ups
  resume the same session. Long work shows up as a live activity card, and
  replies are rendered as Telegram HTML.
- **A permission gate.** Every tool call goes through a `PreToolUse` hook into
  the bridge's policy. Tiers T0–T3 run from advisory up to full autonomy. Risky
  calls become approval cards, and a timeout counts as a deny. Every error path
  fails closed.
- **Only you can use it.** Only allowlisted numeric Telegram user IDs can drive
  it. Everyone else is silently ignored.
- **No inbound ports.** The bridge long-polls Telegram over outbound HTTPS. The
  hook callback listens only on loopback.
- **Files both ways.** You can send it photos and documents, and the agent can
  send files back with a gated `send_file` tool.
- **Voice notes (optional).** A voice note is transcribed by an
  OpenAI-compatible endpoint you choose, then runs as a normal turn.
- **Scheduled jobs.** Recurring prompts run as their own sessions, each with an
  optional per-job cost cap.
- **Recall.** A credential vault stores secrets once and reuses them as env
  vars. A separate fact store keeps non-secret details like your email.
- **Cost guard.** An optional daily USD budget pauses new work once it's spent.

## How it works

```
Telegram ──long-poll──▶ bitch serve ──spawns──▶ claude -p --output-format stream-json
                           ▲                         │
                           │  127.0.0.1 (secret)     ├─ PreToolUse hook ─▶ bitch hook
                           └─────────────────────────┤
                                                     └─ MCP tools ──────▶ bitch mcp
```

`bitch serve` runs the Claude Code CLI once per turn and parses its event
stream. The hook and MCP subprocesses call back to the bridge, which decides
allow / deny / ask. Sessions live in SQLite. The CLI version is **pinned**
(`CLAUDE_VERSION` in `deploy/setup.sh`) because the stream format, hook contract
and flags are version-coupled, and the bridge checks the CLI version at boot.

## Quick start (about 10 minutes)

**1. Gather three things:**

| What | Where to get it |
|---|---|
| Telegram bot token | message [@BotFather](https://t.me/BotFather), send `/newbot` |
| Your numeric Telegram user ID | message [@userinfobot](https://t.me/userinfobot) (a number, not your @username) |
| Anthropic auth | an API key from the [Anthropic Console](https://console.anthropic.com/), **or** a Claude subscription token from running `claude setup-token` on your own computer |

**2. Get a box.** Any always-on **Debian or Ubuntu** machine (amd64 or arm64)
with your SSH key in root's `authorized_keys`. A small cloud VPS is ideal:
treat it as disposable, because the agent gets root. Use a fresh box, not one
that holds anything you care about.

**3. Run the setup script** as root on the box:

```sh
apt-get install -y git   # if missing
git clone https://github.com/intob/bitch.git /root/bitch
bash /root/bitch/deploy/setup.sh
```

It asks for the three values first, checks the bot token against Telegram, and
then runs unattended. It:

- installs Go, Node and the pinned Claude Code CLI
- creates a key-only `admin` SSH user and turns off root and password login
- turns on ufw, fail2ban and unattended upgrades
- builds bitch, writes `/etc/bitch/bitch.env` (mode 600), and installs and starts
  the systemd service

It finishes by printing `online as @yourbot`.

**4. Message your bot.** Try `/status`, then ask it something. You can tell it
to work on a repo you've cloned under `/srv/work`.

Before closing your root session, check that `ssh admin@<box>` works, since
root login is now off.

### Non-interactive setup

Pass the values as env vars instead of answering prompts. This is handy for
cloud-init or a provisioning script:

```sh
# Anthropic auth: ANTHROPIC_API_KEY, or CLAUDE_CODE_OAUTH_TOKEN for a subscription.
# GIT_TOKEN is optional.
BITCH_TELEGRAM_TOKEN=123456:ABC… \
BITCH_ALLOWED_USERS=111111111 \
ANTHROPIC_API_KEY=sk-ant-… \
GIT_TOKEN=github_pat_… \
bash /root/bitch/deploy/setup.sh
```

These optional settings can be passed the same way: `BITCH_ROOTS`,
`BITCH_MODEL`, `BITCH_EFFORT`, `BITCH_DEFAULT_TIER`, `BITCH_DAILY_BUDGET_USD`,
and the `BITCH_TRANSCRIBE_*` voice settings. For anything else in
[`deploy/bitch.env.example`](./deploy/bitch.env.example), edit the env file
afterwards.

The script is safe to re-run. It keeps your existing config and only asks for
what's missing.

### Changing settings later

Edit `/etc/bitch/bitch.env`, then run `systemctl restart bitch`. Every option
is documented in that file: model, effort, default tier, daily budget, approval
timeout and voice transcription. Logs: `journalctl -u bitch -f`.

### Recommended: an external egress firewall

The agent runs as root, so anything inside the box (ufw included) can be undone
by the agent. If your provider offers a firewall enforced outside the VM (most
clouds do), use it:

- **Inbound:** SSH from your IPs only.
- **Outbound:** only what the agent needs.

That firewall is what bounds the risk from code the agent writes and runs (see
below).

### Git access

For private repos, give the setup script a **fine-grained** GitHub token
scoped to just those repos when it asks (or as `GIT_TOKEN`). Use
`contents:read`, or `contents:write` if you want pushes, which still need your
✅. Then clone the repos under `/srv/work`. Never use a full-access token: the
agent runs as root, so it can reach the stored credential.

### Updating

Build on your workstation and ship the binary. The script runs the tests,
cross-compiles, copies, restarts, checks the service came back, and rolls back
if it didn't:

```sh
echo 'BITCH_HOST=203.0.113.7' > deploy/redeploy.local   # gitignored; also BITCH_SSH_USER, BITCH_SSH_KEY
./deploy/redeploy.sh
```

Re-run `setup.sh` on the box instead when the systemd unit, the env schema or
the CLI pin changes.

### Running without systemd

```sh
go build ./cmd/bitch
set -a; . ./my.env; set +a      # the variables from deploy/bitch.env.example
./bitch serve
```

The `claude` CLI must be on `PATH` (or set `BITCH_CLAUDE_BIN`), at the pinned
version.

## Telegram commands

| Command | Effect |
|---|---|
| `/new [dir]` | start a fresh session (optionally in `dir`) |
| `/cd <path>` | switch working directory (starts a fresh session) |
| `/sessions` | list this chat's past sessions |
| `/resume <n>` | switch back to one (restores its directory too) |
| `/tier <0-3>` | set the permission tier (button-confirmed) |
| `/model <name>` | `opus` / `sonnet` / `haiku` / `fable`, a `claude-…` id, or `default` |
| `/effort <level>` | `low` / `medium` / `high` / `xhigh` / `max`, or `default` |
| `/stop` | cancel the running turn (and job turns), dropping the queue |
| `/jobs` | scheduled turns; `/jobs add <spec> :: <prompt>` creates one |
| `/secrets` | list stored credentials; `/secrets set NAME value`, `/secrets rm NAME` |
| `/remember [KEY value]` | store or list non-secret facts |
| `/forget KEY` | drop a fact |
| `/status` | session, dir, tier, model, effort, today's spend |

Anything else goes to the agent. A message you send while a turn is running is
queued and runs next, in the same session.

## Security model

Read this before you deploy. **The agent runs as root on a box you should treat
as disposable.** That's a deliberate trade-off (DESIGN.md §4.6). These are what
hold the line:

- **The permission gate** (`internal/policy`):
  - a denylist checked against the whole command string (destructive ops,
    credential paths, network writes)
  - a small allowlist of harmless commands and read-only or test subcommands
  - **ask** for everything else, including inline interpreters (`python -c`),
    network installers (`npx`, `pip install`), redirects and command
    substitution
  - a workspace jail (`BITCH_ROOTS`) that compares symlink-resolved paths
- **A scrubbed child environment.** The `claude` process is started with an
  environment built from scratch, so it never sees the Telegram token or the
  transcription key.
- **What the gate does *not* stop:** code the agent writes into the workdir and
  then runs (`go test`, `npm test`, `make`). Every coding agent has this risk.
  The external egress firewall and a box you can rebuild from the script are
  what limit it.
- **The credential vault is for convenience, not secrecy.** Stored secrets
  become env vars the agent can read. Only store credentials scoped tightly
  enough that you'd accept the agent seeing them.
- **Voice notes leave the box.** The recording goes to whatever transcription
  endpoint you configure (a local one on 127.0.0.1 works too).

Secrets live in `/etc/bitch/bitch.env` (root-owned, mode 600) and never in
git. If the bot token leaks, rotate it with @BotFather.

## Layout

```
cmd/bitch/        entrypoint: serve (bridge), hook (PreToolUse), mcp (MCP tools)
internal/config/  env config + validation
internal/store/   SQLite: sessions, history, jobs, facts; separate secrets DB
internal/policy/  the permission gate: tiers, bash gate, path jail
internal/agent/   Claude Code CLI orchestration + stream-json parsing
internal/tg/      Telegram rendering, chunking, senders
internal/bridge/  poll loop, commands, approvals, media, voice, jobs, loopback server
deploy/           setup.sh, redeploy.sh, systemd unit, env template
```

## Docs

- [`DESIGN.md`](./DESIGN.md): design and rationale. Code comments cite its sections (`§4.3`).
- [`CLAUDE.md`](./CLAUDE.md): invariants and conventions for working on the
  code. Read it before changing anything.
- [`DEPLOY.md`](./DEPLOY.md): a worked example of one provisioning run.
- `PLAN-*.md`, [`CODE-REVIEW.md`](./CODE-REVIEW.md): feature plans and the review log.

## Development

```sh
go build ./cmd/bitch && go vet ./... && go test ./...
```

Unit tests cover everything that doesn't need Telegram. The approval path can
only be tested live: at T1, ask the agent to `git push` and check that the ✅/❌
card resolves.
