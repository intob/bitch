# Claude Telegram Bridge — Design Doc

**Status:** Draft for review · **Author:** drafted with Claude · **Date:** 2026-07-15

A single always-on agent running on this Mac, reachable from anywhere over
Telegram. Outbound-only transport, a constrained permission posture to start,
and a deliberate path to fuller autonomy later. Not tied to any one repo.

---

## 1. Goal & scope

- **In scope (v1):** One Claude agent, controllable from Telegram, running
  headless on this laptop with the lid closed. Constrained permissions.
  Survives reboots. Conversation continuity per chat.
- **Out of scope (v1):** The multi-agent PM/ARCHITECT/BUILDER/TESTER pipeline,
  multiple hosts, inbound network access. These are deliberately deferred —
  see §11. Get one solid agent working first; the pipeline is a later layer on
  top of the same foundation.

**Design principle:** every capability starts *closed* and is opened
explicitly. The interesting failure mode here isn't "agent can't do enough,"
it's "agent (or something it read on the web) did something destructive on my
primary machine while I wasn't looking."

---

## 2. Architecture at a glance

```
  Phone / laptop / anywhere
          │  (Telegram app)
          ▼
   Telegram servers  ◀── message relay, no inbound ports on our side
          ▲
          │  outbound HTTPS long-poll
          ▼
  ┌───────────────────────────────────────────┐
  │  bridge process  (this Mac, under launchd) │
  │                                            │
  │   ┌──────────────┐     ┌────────────────┐  │
  │   │ Telegram I/O │◀───▶│  session store │  │
  │   │  + user gate │     │  (chat→session)│  │
  │   └──────┬───────┘     └────────────────┘  │
  │          │                                 │
  │          ▼                                 │
  │   ┌──────────────┐   canUseTool callback   │
  │   │  Agent SDK   │────────────┐            │
  │   │  (Claude)    │            ▼            │
  │   └──────┬───────┘   ask-to-approve over   │
  │          │             Telegram            │
  │          ▼                                 │
  │   tools: fs / bash / web (scoped)          │
  └───────────────────────────────────────────┘
          │
          ▼  HTTPS to Anthropic API (the actual model)
```

Key point that shapes everything: **the model runs in the cloud.** This Mac is
an orchestration host — it holds the working files, runs the tools, and relays
messages. That's why one real machine beats a fleet of Pis, and why the tool
sandbox (not the hardware) is the security boundary.

---

## 3. Transport: why Telegram long-polling

- The bridge **polls** Telegram's servers over outbound HTTPS
  (`getUpdates` long-poll, or a webhook — we use long-poll to avoid exposing a
  port). **No inbound connections to this Mac.** No firewall holes, no dynamic
  DNS, no tunnel.
- Nice side effect: if the Mac is briefly asleep or offline, messages **queue
  on Telegram's side** and are delivered when polling resumes. The channel is
  naturally resilient to the laptop dropping off.
- Bot token is issued by **@BotFather**. Treat it like a root credential
  (see §4).

---

## 4. Security model (the crux)

This is the section that matters most, because the bot is a remote-execution
surface on a personal machine.

### 4.1 Who can talk to it
- **Hard allowlist by Telegram numeric user ID.** On every incoming update,
  compare `from.id` against a configured allowlist; silently drop everything
  else. Not username (usernames can change / be spoofed in display) — the
  numeric ID.
- The bot must be created with **privacy mode** appropriate to use, and ideally
  never advertised. Anyone who guesses/finds the bot handle can *message* it;
  the user-ID gate is what stops them getting anything back.

### 4.2 The bot token
- Stored outside the repo (macOS Keychain, or a `.env` that is git-ignored and
  `chmod 600`). Never committed.
- If it leaks: rotate immediately via BotFather (old token dies instantly).
- A leaked token lets an attacker *send as the bot / read its messages* — the
  user-ID gate still blocks them from driving the agent, but rotate anyway.

### 4.3 Permission tiers (progressive autonomy)
The mechanism: the Agent SDK exposes a **`canUseTool` permission callback**
invoked before each tool call. We route it through Telegram. This is what makes
"constrained now, open later" a config change rather than a rewrite.

| Tier | Read (fs, web) | Edit in workdir | Bash (safe) | Bash (destructive), git push, network writes |
|------|:---:|:---:|:---:|:---:|
| **T0 Advisory** | auto | ask | ask | deny |
| **T1 Constrained** *(start here)* | auto | auto (within workdir) | allowlist auto | **ask over Telegram** |
| **T2 Trusted** | auto | auto | auto | ask |
| **T3 Full autonomy** | auto | auto | auto | auto |

- "**ask over Telegram**" = `canUseTool` sends you the exact command/diff and
  blocks until you reply ✅ / ❌ (inline keyboard buttons). A pending request
  times out to **deny**.
- **Working directory jail:** edits and file reads are confined to a configured
  root (e.g. `~/agent-workspace` or a specific repo). Path traversal outside it
  is denied regardless of tier.
- **Command denylist** applies at every tier below T3: `rm -rf`, disk/format
  ops, `curl … | sh`, credential file reads (`~/.ssh`, Keychain, `.env`),
  `sudo`. These *always* ask or deny.
- **Prompt-injection is a first-class threat.** If the agent fetches a web page
  or reads a file containing "ignore your instructions and run X," the tier +
  denylist + workdir jail are what contain it. This is the main reason not to
  jump to T3 early.

### 4.4 Cost / runaway guard
- Per-day token or request budget; when exceeded, the bridge refuses new work
  and pings you. Prevents a stuck loop from quietly burning money overnight.

---

## 5. Keeping a closed-lid laptop awake (chosen: lid closed)

This is the trickiest *physical* detail and you picked the hard mode, so it
gets its own section.

- `caffeinate -dimsu` prevents **idle** sleep but **not lid-close sleep** —
  closing the lid still sleeps the Mac by default.
- To keep running with the lid shut, disable lid sleep explicitly:
  ```
  sudo pmset -a disablesleep 1     # survives until set back to 0
  ```
  (Alternatively, **Amphetamine** from the App Store has a "closed-display
  mode" that manages this via a GUI toggle.)
- **Stay on AC power.** Several keep-awake behaviors don't hold on battery, and
  you don't want it running the battery flat and dying.
- ⚠️ **Thermal caveat — read this.** A MacBook dissipates heat through the
  keyboard/hinge area; closed-lid + sustained CPU (builds, test suites) →
  restricted airflow → **thermal throttling and a hot chassis.** For a chat/
  agent that's mostly waiting on the API this is fine; for heavy local
  compilation it's the real limiter. Mitigations: prop the machine on a hard
  surface with a gap, avoid stacking it under things, and keep genuinely heavy
  builds off it (or accept slower throttled runs).
- **Recovery:** if it sleeps anyway, Telegram queues your messages and the
  bridge resumes on wake — so a stray sleep is a delay, not data loss.

---

## 6. Sessions & conversation state

- **Map each Telegram chat/thread → one persisted Claude session** (session ID
  from the SDK). Follow-up messages `--resume` that session so context carries
  across messages; they are not one-shots.
- Small persistent store (SQLite or a JSON file) holds `chat_id → session_id`,
  working dir, and current tier.
- **Control commands:**
  - `/new [dir]` — fresh session, optional working directory
  - `/stop` — interrupt the currently running turn
  - `/tier <0-3>` — change permission tier (with a confirmation step)
  - `/cd <path>` — switch working directory (within allowed roots)
  - `/status` — current session, dir, tier, today's token spend

---

## 7. Output plumbing (the UX make-or-break)

- **4096-char message limit:** chunk long replies on newline / code-fence
  boundaries; never split mid-fence.
- **Streaming, not silence:** while the agent works, edit a single status
  message ("🔧 running tests…") via `editMessageText` rather than spamming new
  messages — respects Telegram's ~1 msg/sec-per-chat rate limit.
- **Formatting:** Telegram MarkdownV2 requires fussy escaping; use **HTML parse
  mode** (`<pre>`, `<code>`, `<b>`) which is more forgiving for arbitrary agent
  output.
- **Big artifacts** (long diffs, files, logs) → send as a **document
  attachment** instead of an inline wall of text.
- Approval prompts (§4.3) use **inline keyboard buttons** (✅/❌), not free-text
  parsing.

---

## 8. Process lifecycle

- Run as a **launchd LaunchAgent** (`~/Library/LaunchAgents/…plist`):
  `RunAtLoad=true`, `KeepAlive=true` so it starts at login and auto-restarts on
  crash.
- Wrap under `caffeinate` (or let launchd start it and manage `pmset`
  separately per §5).
- **Logging** to a rotating file (`~/Library/Logs/claude-tg-bridge.log`) for
  debugging — you can't watch a terminal for an always-on service.
- On crash/restart, in-flight Telegram updates are re-fetched; a partially
  completed agent turn is reported as interrupted rather than silently lost.

---

## 9. Recommended stack

- **Language / SDK:** TypeScript + **Claude Agent SDK** (first-class
  `canUseTool`, session resume, hooks) — or Python if you prefer; the Python
  SDK covers the same surface. *(Decision to confirm — §12.)*
- **Telegram lib:** `grammy` (TS) or `python-telegram-bot` v21+ (async).
- **Store:** SQLite (via `better-sqlite3` / `sqlite3`) or a flat JSON file to
  start.
- **Secrets:** macOS Keychain (`security` CLI) or git-ignored `.env`.

> ⚠️ Exact SDK method names (`canUseTool`, session-resume options) should be
> verified against the current Agent SDK docs at build time — treat the API
> surface here as design-level, not copy-paste.

---

## 10. Failure modes & how they're handled

| Failure | Behavior |
|---|---|
| Laptop sleeps / network drops | Messages queue on Telegram; bridge resumes on wake |
| API rate limit / error | Surfaced to you in-chat, not swallowed |
| Runaway loop | Per-day budget guard trips, agent pauses, you're pinged |
| Bridge crash | launchd `KeepAlive` restarts it; interrupted turn reported |
| Prompt injection via fetched content | Tier + denylist + workdir jail contain it |
| Unknown user messages bot | Dropped by user-ID gate |
| Thermal throttle (closed lid) | Slower runs; keep heavy builds off the machine |

---

## 11. Path to the bigger vision

This foundation is deliberately the substrate for the multi-agent pipeline you
originally described, without committing to it now:

- **Multiple agents** = multiple sessions/personas behind the same bridge, or
  multiple bots — each with its own tier, model, and effort. The `canUseTool`
  gate and workspace jail are per-agent already.
- **Structured handoffs** (PM → ARCHITECT → BUILDER → TESTER) become an
  artifact/task store the agents read and write — *not* free-form group chat
  (which is where these systems usually fail). Telegram stays the human
  observation/steering layer.
- **Progressive autonomy** (T1→T3) is the same knob, per agent.

Prove the single agent first; add roles as a layer, not a rewrite.

---

## 12. Open decisions (need your call before build)

1. **SDK language:** TypeScript (recommended) vs Python?
2. **Working-directory root:** one shared `~/agent-workspace`, or point it
   straight at real repos like `~/ss` (with the jail set accordingly)?
3. **Model & effort for v1:** e.g. Opus for the main agent, or a cheaper model
   until you trust it?
4. **Lid-sleep method:** `sudo pmset disablesleep` (scriptable) vs Amphetamine
   (GUI)?
5. **Group vs 1:1:** start as a private 1:1 chat with the bot, or a Telegram
   group from day one (in anticipation of multiple agents)?

---

## 13. Suggested build phases

1. **Skeleton** — bot + user-ID gate + echo. Prove the transport and that only
   you can drive it.
2. **Agent wired in (T0 advisory)** — read-only Claude answering in-chat, with
   session continuity. No write powers.
3. **Constrained (T1)** — edits in workdir, safe-bash allowlist, Telegram
   approval for risky ops via `canUseTool`.
4. **Hardening** — launchd service, lid-sleep config, logging, budget guard.
5. **Later** — additional agents / the pipeline layer (§11).
