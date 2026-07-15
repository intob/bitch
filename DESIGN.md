# Claude Telegram Bridge — Design Doc

**Status:** Draft for review · **Author:** drafted with Claude · **Date:** 2026-07-15

A single always-on agent running on a **Hetzner VPS**, reachable from anywhere
over Telegram. Outbound-only transport, a constrained permission posture to
start, and a deliberate path to fuller autonomy later. Not tied to any one repo.

---

## 1. Goal & scope

- **In scope (v1):** One Claude agent, controllable from Telegram, running
  headless on an always-on **Hetzner VPS**. Constrained permissions. Survives
  reboots. Conversation continuity per chat.
- **Out of scope (v1):** The multi-agent PM/ARCHITECT/BUILDER/TESTER pipeline,
  multiple hosts, inbound network access. These are deliberately deferred —
  see §11. Get one solid agent working first; the pipeline is a later layer on
  top of the same foundation.

**Design principle:** every capability starts *closed* and is opened
explicitly. The interesting failure mode here isn't "agent can't do enough,"
it's "agent (or something it read on the web) did something destructive to the
code, credentials, or box it can reach while I wasn't looking." Hosting on a
cloud VPS instead of the laptop shrinks that blast radius to a disposable box —
a real improvement, but not zero, since repos and Git credentials still live on
it.

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
  │  bridge process  (Hetzner VPS · systemd)   │
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
- Nice side effect: if the VPS reboots or the network blips, messages **queue on
  Telegram's side** and are delivered when polling resumes.
- Bot token is issued by **@BotFather**. Treat it like a root credential
  (see §4).

---

## 4. Security model (the crux)

This is the section that matters most: the bot is a remote-execution surface,
now on an internet-exposed cloud host.

### 4.1 Who can talk to it
- **Hard allowlist by Telegram numeric user ID.** On every incoming update,
  compare `from.id` against a configured allowlist; silently drop everything
  else. Not username (usernames can change / be spoofed in display) — the
  numeric ID.
- Create the bot with appropriate privacy mode and don't advertise the handle.
  Anyone who finds it can *message* it; the user-ID gate is what stops them
  getting anything back.

### 4.2 The bot token & API key
- Stored outside the repo (systemd `EnvironmentFile` with `chmod 600`, or a
  secrets manager). Never committed.
- If the bot token leaks: rotate immediately via BotFather (old token dies
  instantly). If the Anthropic key leaks: rotate in the console.
- A leaked bot token lets an attacker *send as the bot / read its messages* —
  the user-ID gate still blocks them from driving the agent, but rotate anyway.

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
  root (e.g. `/srv/agent/workspace`). Path traversal outside it is denied
  regardless of tier.
- **Command denylist** applies at every tier below T3: `rm -rf`, disk ops,
  `curl … | sh`, reads of credential files (`~/.ssh`, token env), `sudo`. These
  *always* ask or deny.
- **Prompt-injection is a first-class threat.** If the agent fetches a web page
  or reads a file containing "ignore your instructions and run X," the tier +
  denylist + workdir jail are what contain it. The disposable-box isolation
  (§4.5) is the backstop.

### 4.4 Cost / runaway guard
- Per-day token or request budget; when exceeded, the bridge refuses new work
  and pings you. Prevents a stuck loop from quietly burning money overnight.

### 4.5 Host hardening (new — it's now a public cloud box)
The box is internet-exposed, so standard VPS hygiene applies *in addition* to
the app-level controls above:
- SSH **key-only**, password auth disabled, root login disabled; run the bridge
  as a **non-root user**.
- **Firewall** (Hetzner Cloud Firewall + `ufw`): inbound limited to SSH from
  your IP(s); no inbound app ports (long-poll is outbound). Everything else
  denied.
- `fail2ban` + unattended security upgrades.
- **Scope the credentials that live on the box.** It needs the Anthropic key,
  the Telegram token, and — if it touches repos — Git access. Use **per-repo
  deploy keys** or a **fine-grained PAT** limited to the specific repos, never
  your full-access personal GitHub token.
- **Treat the box as reproducible:** capture setup as a small script (or
  cloud-init), so a suspected-compromised box is *rebuilt*, not cleaned.
- **Blast-radius upside:** unlike the laptop, this box has no Keychain, no
  personal `~/.ssh`, no personal files — a worst-case agent action is contained
  to a rebuildable VM. This is why T2/T3 become reasonable sooner here.

---

## 5. The host: Hetzner VPS (always-on)

The agent lives on a cloud box, not the laptop — so it stays reachable whether
the laptop is asleep, offline, or in a bag on a train. Your phone on mobile data
→ Telegram → the VPS works regardless of where you (or the laptop) are.

- **Box:** Hetzner Cloud. Start with an ARM **CAX11** (2 vCPU / 4 GB, ~€4/mo) —
  ample, since the model runs via API and the box only orchestrates + runs
  tools. Step up to **CAX21** (4 vCPU / 8 GB) if local builds/test suites are
  heavy. ARM64 (Ampere) runs Node / Python / Claude Code fine; watch for the
  rare x86-only toolchain, in which case use an x86 CX line box instead.
- **OS:** Ubuntu LTS (or Debian), plus the Node/Python toolchain + git.
- **Region:** EU (Falkenstein / Nuremberg / Helsinki) for a UK user; US
  (Ashburn) if preferred. Latency is not critical for a chat agent.
- **No sleep, no thermal limits, no AC-power worry.** These laptop-only concerns
  are gone entirely.
- **Data residency:** repos cloned to the box live on Hetzner (EU, GDPR-
  friendly). Worth a thought before cloning sensitive work repos — see §4.5 for
  credential scoping.

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

- Run as a **systemd service** (`/etc/systemd/system/bitch.service`):
  `Restart=always`, `WantedBy=multi-user.target` so it starts at boot and
  auto-restarts on crash. Runs as a dedicated non-root user.
- Secrets injected via `EnvironmentFile=` (root-owned, `chmod 600`).
- **Logging** to `journald` (`journalctl -u bitch`) — you can't watch a terminal
  for an always-on service.
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
- **Secrets:** systemd `EnvironmentFile` (or a secrets manager) — never in git.

> ⚠️ Exact SDK method names (`canUseTool`, session-resume options) should be
> verified against the current Agent SDK docs at build time — treat the API
> surface here as design-level, not copy-paste.

---

## 10. Failure modes & how they're handled

| Failure | Behavior |
|---|---|
| Laptop offline while travelling | Irrelevant — agent runs on the VPS; phone-over-mobile reaches it fine |
| VPS reboot / network drop | Messages queue on Telegram; systemd restarts the bridge, which resumes on reconnect |
| API rate limit / error | Surfaced to you in-chat, not swallowed |
| Runaway loop | Per-day budget guard trips, agent pauses, you're pinged |
| Bridge crash | systemd `Restart=always`; interrupted turn reported |
| Prompt injection via fetched content | Tier + denylist + workdir jail; disposable-box isolation as backstop |
| Unknown user messages bot | Dropped by user-ID gate |
| Box compromised | Rebuild from setup script; scoped credentials limit reach |

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
- **Scaling out** is easy from a VPS: bigger box, or several boxes, without ever
  touching your personal machine.

Prove the single agent first; add roles as a layer, not a rewrite.

---

## 12. Open decisions (need your call before build)

1. **SDK language:** TypeScript (recommended) vs Python?
2. **Working-directory root:** one shared `/srv/agent/workspace`, or clone real
   repos like `~/ss` onto the box (with the jail set accordingly)?
3. **Model & effort for v1:** e.g. Opus for the main agent, or a cheaper model
   until you trust it?
4. **VPS size & region:** ARM CAX11 (2 vCPU/4 GB) vs CAX21 (4 vCPU/8 GB);
   EU (Falkenstein/Helsinki) vs US (Ashburn)?
5. **Group vs 1:1:** start as a private 1:1 chat with the bot, or a Telegram
   group from day one (in anticipation of multiple agents)?
6. **Repos on the box:** which repos get cloned to the VPS, and with what Git
   credential scope (per-repo deploy key vs fine-grained PAT)? Note that source
   then lands on a cloud box (Hetzner, EU).

---

## 13. Suggested build phases

1. **Provision & harden** — spin up the Hetzner box, SSH key-only, firewall,
   non-root user, toolchain. (§4.5, §5)
2. **Skeleton** — bot + user-ID gate + echo. Prove the transport and that only
   you can drive it.
3. **Agent wired in (T0 advisory)** — read-only Claude answering in-chat, with
   session continuity. No write powers.
4. **Constrained (T1)** — edits in workdir, safe-bash allowlist, Telegram
   approval for risky ops via `canUseTool`.
5. **Hardening** — systemd service, logging, budget guard, secrets in place.
6. **Later** — additional agents / the pipeline layer (§11).
