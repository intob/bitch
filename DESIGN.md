# bitch — Design Doc

**Status:** Draft for review · **Author:** drafted with Claude · **Date:** 2026-07-15

A single always-on agent running on a **Hetzner VPS**, reachable from anywhere
over Telegram, written in **Go**. Outbound-only transport, a constrained
permission posture to start, and a deliberate path to fuller autonomy later. Not
tied to any one repo.

---

## 1. Goal & scope

- **In scope (v1):** One Claude agent, controllable from Telegram, running
  headless on an always-on **Hetzner VPS**. Go bridge. Constrained permissions.
  Survives reboots. Conversation continuity per chat.
- **Out of scope (v1):** The multi-agent PM/ARCHITECT/BUILDER/TESTER pipeline,
  multiple hosts, inbound network access. Deferred — see §11. Get one solid
  agent working first; the pipeline is a later layer on the same foundation.

**Design principle:** every capability starts *closed* and is opened
explicitly. The interesting failure mode isn't "agent can't do enough," it's
"agent (or something it read on the web) did something destructive to the code,
credentials, or box it can reach while I wasn't looking." Hosting on a cloud VPS
shrinks that blast radius to a disposable box — a real improvement, but not
zero, since repos and Git credentials live on it.

**Why a VPS (not the laptop):** the laptop travels and goes offline; the phone
stays online over mobile data. An always-on cloud box is reachable regardless of
where the laptop is, and it deletes the whole closed-lid / sleep / thermal
problem. The laptop becomes just another client, same as the phone.

---

## 2. Architecture at a glance

```
  Phone (mobile data) / laptop / anywhere
          │  (Telegram app)
          ▼
   Telegram servers  ◀── message relay, no inbound ports on our side
          ▲
          │  outbound HTTPS long-poll
          ▼
  ┌───────────────────────────────────────────┐
  │  bitch bridge (Go)  (Hetzner VPS · systemd) │
  │                                            │
  │   ┌──────────────┐     ┌────────────────┐  │
  │   │ Telegram I/O │◀───▶│  session store │  │
  │   │  + user gate │     │  (chat→session)│  │
  │   └──────┬───────┘     └────────────────┘  │
  │          │                                 │
  │          ▼                                 │
  │   ┌──────────────┐    permission gate      │
  │   │  agent core  │────────────┐            │
  │   │  (Claude)    │            ▼            │
  │   └──────┬───────┘   ask-to-approve over   │
  │          │             Telegram            │
  │          ▼                                 │
  │   tools: fs / bash / web (scoped)          │
  └───────────────────────────────────────────┘
          │
          ▼  HTTPS to Anthropic API (the actual model)
```

Key point that shapes everything: **the model runs in the cloud.** The **VPS**
is an orchestration host — it holds the working files, runs the tools, and
relays messages. That's why one real always-on box beats both a fleet of Pis and
a laptop that travels, and why the tool sandbox (not the hardware) is the
security boundary.

---

## 3. Transport: why Telegram long-polling

- The bridge **polls** Telegram's servers over outbound HTTPS
  (`getUpdates` long-poll). Because the VPS has a public IP a **webhook** is now
  also viable, but long-poll keeps zero inbound app ports open — simpler and
  one less thing to secure. **No inbound connections needed.**
- If the VPS reboots or the network blips, messages **queue on Telegram's side**
  and are delivered when polling resumes.
- Bot token is issued by **@BotFather**. Treat it like a root credential (§4).

---

## 4. Security model (the crux)

The bot is a remote-execution surface, now on an internet-exposed cloud host.

### 4.1 Who can talk to it
- **Hard allowlist by Telegram numeric user ID.** On every incoming update,
  compare `from.id` against a configured allowlist; silently drop everything
  else. Not username (spoofable in display) — the numeric ID.
- Create the bot with appropriate privacy mode and don't advertise the handle.

### 4.2 The bot token & API key
- Stored outside the repo (systemd `EnvironmentFile`, `chmod 600`). Never
  committed.
- Bot token leaks → rotate via BotFather (old token dies instantly). Anthropic
  key leaks → rotate in the console.

### 4.3 Permission tiers (progressive autonomy)
The tiers below are identical regardless of implementation; only the *mechanism*
that enforces them differs by stack (see §9):

- **Go + Claude Code CLI (recommended):** each tier is a Claude Code permission
  profile (`settings.json` allow/deny + `--allowed-tools`). Risky ops fire a
  **`PreToolUse` hook** that calls back into the bridge, which asks you over
  Telegram and returns allow/deny.
- **Go + `anthropic-sdk-go`:** you define the tools (bash/read/write/edit) as Go
  functions and gate *inside* each — the risky ones send a Telegram approval and
  block on your reply.

Either way, "constrained now, open later" is a config change, not a rewrite.

| Tier | Read (fs, web) | Edit in workdir | Bash (safe) | Bash (destructive), git push, network writes |
|------|:---:|:---:|:---:|:---:|
| **T0 Advisory** | auto | ask | ask | deny |
| **T1 Constrained** *(start here)* | auto | auto (within workdir) | allowlist auto | **ask over Telegram** |
| **T2 Trusted** | auto | auto | auto | ask |
| **T3 Full autonomy** | auto | auto | auto | auto |

- "**ask over Telegram**" sends you the exact command/diff and blocks until you
  reply ✅ / ❌ (inline keyboard buttons). A pending request times out to **deny**.
- **Working directory jail:** edits/reads confined to a configured root. Path
  traversal outside it is denied regardless of tier.
- **Command denylist** at every tier below T3: `rm -rf`, disk ops, `curl … | sh`,
  reads of credential files (`~/.ssh`, token env), `sudo`. Always ask or deny.
- **Prompt-injection is a first-class threat.** Tier + denylist + workdir jail
  contain it; the disposable-box isolation (§4.5) is the backstop.

### 4.4 Cost / runaway guard
- Per-day token or request budget; when exceeded, the bridge refuses new work
  and pings you. On Opus 4.8 you can also set an API-native **task budget** (a
  token ceiling the model paces itself against) per agentic run (§9.1).

### 4.5 Host hardening (it's a public cloud box)
- SSH **key-only**, password auth disabled, root login disabled; run the bridge
  as a **non-root user** (see §4.6).
- **Firewall** (Hetzner Cloud Firewall + `ufw`): inbound limited to SSH from
  your IP(s); no inbound app ports (long-poll is outbound). Everything else denied.
- `fail2ban` + unattended security upgrades.
- **Git credentials for all repos + future self-hosted Git.** Decided: credentials
  are provisioned explicitly on the box. Recommended shape — a dedicated
  **machine user** (a bot GitHub account added to your repos) or a **fine-grained
  PAT** scoped to the repos you clone; the future self-hosted server is just
  another remote + a deploy key. Never the full-access personal GitHub token.
- **Treat the box as reproducible:** capture setup as a script / cloud-init, so a
  suspected-compromised box is *rebuilt*, not cleaned.
- **Blast-radius upside:** no Keychain, no personal `~/.ssh`, no personal files —
  worst case is a rebuildable VM. This is why T2/T3 become reasonable sooner here.

### 4.6 Root vs non-root (decision pending)
Running the agent **as root collapses several containment layers at once** — the
workdir jail, the command denylist, and secret isolation all rely on normal file
permissions, and root walks through all of them (it can read the token
`EnvironmentFile`, rewrite the systemd unit, disable the firewall). The
disposable-VPS argument softens the *cost* of a compromise but does not justify
making one trivial and total.

**Recommended default:** run as a dedicated **non-root user** with a curated
`NOPASSWD` **sudo allowlist** for the few operations that genuinely need it
(package installs, service restarts). Keeps the convenience of root for the
handful of privileged tasks while leaving every containment layer meaningful.
Full root remains an option you can choose with eyes open — see §12.

---

## 5. The host: Hetzner VPS (always-on)

The agent lives on a cloud box, not the laptop — reachable whether the laptop is
asleep, offline, or in a bag on a train. Phone on mobile data → Telegram → VPS
works regardless of where you (or the laptop) are.

- **Box: start on ARM `CAX21` (4 vCPU / 8 GB, ~€7/mo).** The model runs via API,
  so RAM/CPU is only for git, builds, and test suites. `CAX11` (2/4, ~€4) works
  for light repos but gets tight on `npm install`, Docker, or parallel tests;
  since you're cloning *all* your repos and eyeing multi-agent later, CAX21 is
  the "don't think about it" pick. **Resizing CPU/RAM up is easy and reversible**
  (disk only grows), so starting modest is low-risk.
- **Region: EU — ARM `CAX` instances are EU-only on Hetzner** (Falkenstein /
  Nuremberg / Helsinki). Perfect for a UK user; latency is irrelevant (API calls
  go to Anthropic regardless). If you ever need x86 (rare x86-only toolchain),
  the `CX`/`CPX` line is available in the US too.
- **OS:** Ubuntu LTS (or Debian), plus Go, git, and the Node/Python toolchains
  your repos build with. ARM64 runs Go, Node, Python, and Claude Code fine.
- **No sleep, no thermal limits, no AC-power worry.**
- **Data residency:** repos cloned to the box live on Hetzner (EU, GDPR-friendly).

---

## 6. Sessions & conversation state

- **Map each Telegram chat/thread → one persisted Claude session.** Follow-ups
  resume that session so context carries across messages; not one-shots.
- Small persistent store (SQLite or a JSON file) holds
  `(chat_id, thread_id, bot_id) → session_id`, working dir, and current tier —
  keyed group-ready from day one (§11) so the multi-agent move is additive.
- **Control commands:** `/new [dir]`, `/stop`, `/tier <0-3>` (with confirm),
  `/cd <path>` (within allowed roots), `/status` (session, dir, tier, spend).

---

## 7. Output plumbing (the UX make-or-break)

- **4096-char message limit:** chunk long replies on newline / code-fence
  boundaries; never split mid-fence.
- **Streaming, not silence:** parse Claude Code's `stream-json` output (or the
  SDK stream) and edit a single status message ("🔧 running tests…") via
  `editMessageText` rather than spamming — respects Telegram's ~1 msg/sec/chat
  rate limit.
- **Formatting:** use **HTML parse mode** (`<pre>`, `<code>`, `<b>`) — more
  forgiving for arbitrary agent output than MarkdownV2.
- **Big artifacts** (long diffs, files, logs) → send as a **document attachment**.
- Approval prompts (§4.3) use **inline keyboard buttons** (✅/❌).

---

## 8. Process lifecycle

- Run as a **systemd service** (`/etc/systemd/system/bitch.service`):
  `Restart=always`, `WantedBy=multi-user.target`. Dedicated non-root user.
- Secrets injected via `EnvironmentFile=` (root-owned, `chmod 600`).
- **Logging** to `journald` (`journalctl -u bitch`).
- On crash/restart, in-flight Telegram updates are re-fetched; a partially
  completed agent turn is reported as interrupted rather than silently lost.

---

## 9. Stack

Go for the bridge. **There is no Claude Agent SDK for Go** — that library (the
batteries-included Claude Code harness with `canUseTool`, built-in tools, session
resume) is TypeScript/Python only. Go has `anthropic-sdk-go` (raw Messages API +
a `toolrunner` helper). So the agent core is one of two paths (§9.2).

- **Telegram lib:** `go-telegram-bot-api` or `telebot` (`tucnak/telebot`).
- **Store:** SQLite (`modernc.org/sqlite`, pure-Go, no cgo) or a flat JSON file.
- **Secrets:** systemd `EnvironmentFile` — never in git.

### 9.1 Model & effort
- **Model: Claude Opus 4.8** (`claude-opus-4-8`) for real work — most capable
  Opus tier, adaptive thinking on. Cost is $5/$25 per MTok.
- **Effort: `high`** as the default (the recommended starting point for 4.8;
  `xhigh` for heavy coding/agentic tasks, `max` when correctness must beat cost).
- **Develop the plumbing on a cheaper model** (Haiku 4.5 / Sonnet 5) — validating
  transport, gating, and sessions doesn't need Opus. Model + effort are
  per-session config, so switching is trivial.
- Pair Opus with the budget guard (§4.4) — Opus + agentic loops burn tokens.

### 9.2 Go implementation paths (decision pending — §12)
- **Path B — Go bridge orchestrates the Claude Code CLI (recommended).** Go owns
  Telegram + process lifecycle; it spawns
  `claude -p --output-format stream-json --resume …`. Claude Code brings the
  maintained harness: built-in tools, sessions, MCP, and a real permission
  system. Tiers = permission profiles; approvals = a `PreToolUse` hook that calls
  back into the bridge → Telegram. Least reinvention; reuses Anthropic's
  security-critical tooling. Bridge logic is still all Go.
- **Path A — pure Go with `anthropic-sdk-go`.** Define bash/read/write/edit as Go
  functions, run the loop with the `toolrunner` helper, gate inside each tool
  function. Maximum control, minimal deps, naturally small tool surface — but
  **you own the security-critical bash/file plumbing** (path jail, denylist).

---

## 10. Failure modes & how they're handled

| Failure | Behavior |
|---|---|
| Laptop offline while travelling | Irrelevant — agent runs on the VPS; phone-over-mobile reaches it |
| VPS reboot / network drop | Messages queue on Telegram; systemd restarts the bridge, which resumes |
| API rate limit / error | Surfaced to you in-chat, not swallowed |
| Runaway loop | Per-day budget guard trips, agent pauses, you're pinged |
| Bridge crash | systemd `Restart=always`; interrupted turn reported |
| Prompt injection via fetched content | Tier + denylist + workdir jail; disposable-box isolation as backstop |
| Unknown user messages bot | Dropped by user-ID gate |
| Box compromised | Rebuild from setup script; non-root + scoped creds limit reach |

---

## 11. Path to the bigger vision

The foundation is deliberately the substrate for the multi-agent pipeline:

- **Multiple agents** = multiple sessions/personas behind the same bridge, or
  multiple bots — each with its own tier, model, and effort. The permission gate
  and workspace jail are per-agent already.
- **Structured handoffs** (PM → ARCHITECT → BUILDER → TESTER) become an
  artifact/task store the agents read and write — *not* free-form group chat.
  Telegram stays the human observation/steering layer.
- **1:1 now, group-ready by design.** Start as a private 1:1 chat (simplest,
  no @mention/privacy-mode friction). But key sessions by
  `(chat_id, thread_id, bot_id)` and handle @mention addressing from day one — a
  Telegram **supergroup with topics** is the natural home for the multi-agent
  "team standup," and building group-ready now makes that move additive, not a
  refactor. (Group `chat_id`s are negative; topics add `thread_id`.)
- **Scaling out** is easy from a VPS: bigger box, or several boxes.

Prove the single agent first; add roles as a layer, not a rewrite.

---

## 12. Open decisions

**Resolved:**
- **Language:** Go.
- **Model & effort:** Opus 4.8 at `high` (cheaper model for plumbing dev).
- **VPS:** ARM CAX21, EU region (CAX is EU-only).
- **Repos:** all GitHub repos cloned to the box + future self-hosted Git;
  credentials provisioned explicitly (machine user or fine-grained PAT).
- **Working-dir root:** clone real repos onto the box under a jailed root.

**Still to confirm:**
1. **Go implementation path:** B (orchestrate Claude Code CLI — recommended) vs
   A (pure `anthropic-sdk-go` with self-built tools)? §9.2
2. **Privilege posture:** non-root + scoped `NOPASSWD` sudo (recommended) vs full
   root? §4.6
3. **Telegram surface:** start 1:1 (recommended, group-ready) vs a group from day
   one? §11

---

## 13. Suggested build phases

1. **Provision & harden** — spin up the Hetzner CAX21 box, SSH key-only,
   firewall, non-root user, Go + toolchains. (§4.5, §4.6, §5)
2. **Skeleton** — Go bot + user-ID gate + echo. Prove the transport and that only
   you can drive it.
3. **Agent wired in (T0 advisory)** — read-only Claude answering in-chat, with
   session continuity. No write powers. (Chosen path from §9.2.)
4. **Constrained (T1)** — edits in workdir, safe-bash allowlist, Telegram approval
   for risky ops.
5. **Hardening** — systemd service, logging, budget guard, secrets in place.
6. **Later** — additional agents / the pipeline layer (§11).
