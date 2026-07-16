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
- **Working directory jail:** edits/reads confined to a configured root,
  compared on **physical paths** — symlinks are resolved on both the roots and
  the target, so a link inside the jail can't launder a target outside it.
  Path traversal outside is denied regardless of tier.
- **The bash gate** (`internal/policy`) below T3 is three layers, each failing
  toward *ask*:
  1. a **denylist** of destructive / credential / network-write patterns
     (`rm -rf`, disk ops, `curl … | sh`, `git push`, `~/.ssh` **and**
     `/root/.ssh` — the agent's HOME is /root — `/etc/bitch`, firewall and
     service tools, `find -delete/-exec`) that scans the **whole command
     string**, so badness embedded in the arguments of a safe leading command
     is still caught;
  2. a deliberately small **allowlist**: inert utilities plus read-only VCS /
     toolchain **subcommands** (`git status|diff|log|fetch…`,
     `go build|test|vet…`, `npm test|run`, `pytest`, `make`). Interpreters
     with inline code (`python -c`, `node -e`, `awk`), network installers
     (`npx`, `pip install`, `npm install`), `env` (would print secrets),
     `sed` (`-i` writes arbitrary paths), and any command containing **file
     redirection** (`>`) or **command substitution** (`$(…)`, backticks) all
     go to ask;
  3. everything unrecognized **asks**.
- **What the gate deliberately does not stop:** running code that lives in the
  workdir (`go test`, `npm test`, `make`). An agent allowed to edit files and
  run tests can always smuggle behavior into the code it runs — that is
  inherent to a useful coding agent, not a hole. The gate's job is to close
  the **zero-footprint escapes** (inline interpreter code,
  network-fetch-and-run), so that going off-script requires either a human ✅
  or a visible artifact in the diff. The residual risk is carried by the
  egress firewall and the disposable box (§4.5).
- **Prompt-injection is a first-class threat.** Tier + gate + workdir jail
  contain it; the disposable-box isolation (§4.5) is the backstop.

### 4.4 Cost / runaway guard
- Per-day token or request budget; when exceeded, the bridge refuses new work
  and pings you. On Opus 4.8 you can also set an API-native **task budget** (a
  token ceiling the model paces itself against) per agentic run (§9.1).

### 4.5 Host hardening (it's a public cloud box)
- SSH **key-only**, password auth disabled. SSH root *login* stays disabled even
  though the agent runs as root locally (§4.6) — you SSH in as a separate admin
  user.
- **Firewall — lean on the Hetzner Cloud Firewall, not just `ufw`.** It's
  enforced *outside* the VM, so a root-level compromise inside the box can flush
  `ufw` but cannot touch it. Inbound limited to SSH from your IP(s); no inbound
  app ports (long-poll is outbound). Set **egress limits here too** — with the
  agent running as root, this is your best remaining control on where a
  compromised process can reach out.
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

### 4.6 Privilege posture — **decided: full root**
Running as root **collapses the OS-level containment layers** — the workdir jail,
the command denylist, and secret isolation all rely on file permissions, and root
walks through all of them (it can read the token `EnvironmentFile`, rewrite the
systemd unit, flush `ufw`). This is a deliberate, eyes-open choice for
convenience; the disposable-VPS model bounds the *cost* of a compromise to a
rebuildable box.

Because the OS layers no longer contain anything, the **compensating controls
carry the weight** — treat these as load-bearing, not optional:
- **The permission gate is now the primary boundary.** Under Path B (§9.2) Claude
  Code evaluates its permission profile *before* executing a tool, independent of
  the OS user — so tiers still work. Keep **T1 (constrained)** the operating tier
  longer than you otherwise would; only the Telegram approval hook stands between
  a risky tool call and root execution. Be honest about what "boundary" means
  here: the gate closes the zero-footprint escapes (§4.3) — it cannot stop an
  agent that is allowed to write and run workdir code from putting anything it
  likes *in* that code. That residual is bounded by the two controls below.
- **The agent's environment is scrubbed.** The claude process (and every tool
  it runs) gets a from-scratch env: base vars, `ANTHROPIC_*`/`CLAUDE_*`, and
  the hook endpoint — never the Telegram token or the bridge's own config.
- **Hetzner Cloud Firewall for egress** (§4.5) — enforced outside the VM, so root
  can't disable it. This is the one control root can't defeat.
- **Disposable box** (§4.5) — rebuild on any suspicion; keep nothing on it that
  isn't reproducible from a script.

### 4.7 Credential vault — provide a credential once, reuse it later
When a task needs a credential the agent doesn't have (API key, service token), it
asks over Telegram; storing it means a *later, unrelated* task in a fresh session
doesn't have to ask again. This generalizes the git-PAT pattern (§4.5) from a
provisioning-time file to a runtime store. Design (PLAN-CREDENTIALS.md is the
detailed plan; `internal/store/secrets.go`, `internal/bridge/secrets.go`):

- **Storage:** a *separate* SQLite DB at the fixed path `/etc/bitch/secrets.db`.
  The path is **not** configurable: the policy deny that stops the agent reading it
  is a static `/etc/bitch` literal, so a movable path would fail open. `/etc/bitch`
  is outside both the workdir jail and `/opt/bitch`, so the Read tool denies it and
  `cat /etc/bitch/secrets.db` hits the deny pattern.
- **Reuse:** stored secrets are injected as env vars into the scrubbed claude child
  (`agent.childEnv`), and a names+descriptions *manifest* (never values) is
  prepended to the prompt so the agent knows which vars exist and stops re-asking.
- **Name safety is a hard boundary.** A stored name becomes an env var in a **root**
  child, so `store.ValidSecretName` refuses anything that can redirect code loading
  (`LD_*`, `BASH_ENV`, `*PATH`, `*_OPTIONS`, `GIT_SSH_COMMAND`, …) or shadow a
  base/hook/`ANTHROPIC_*`/`CLAUDE_*` var. Enforced at save **and** at injection.
- **Honest posture — it is a convenience store, not a confidential one.** Once a
  value is in the child env, any shell the agent runs reads it (`echo $X`,
  `cat /proc/self/environ`) — you cannot keep an env var secret from a shell, and
  those reads auto-allow at T1+. So every stored credential is agent-readable and
  reachable by workdir code, and the egress firewall does **not** contain HTTPS
  exfil (egress allows 443 outbound). The **disposable box** is the real backstop:
  store only what you can tolerate on an agent-readable box, and rebuild on
  suspicion. There is deliberately **no** value-returning path back to the agent
  (no `secret_get`) — the loopback shared secret is in the child env, so such a
  path would be an unauthenticated vault read from inside the sandbox.
- **v1 capture is manual** (`/secrets set NAME value`, which self-deletes the
  plaintext message); a seamless agent-driven MCP capture tool is deferred because
  it is CLI-version-coupled and only live-testable.

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
- Small persistent store (SQLite) holds
  `(chat_id, thread_id, bot_id) → session_id`, working dir, current tier, and
  per-session model/effort overrides — keyed group-ready from day one (§11) so
  the multi-agent move is additive.
- **Control commands:** `/new [dir]`, `/stop`, `/tier <0-3>` (with confirm),
  `/model <name>`, `/effort <level>`, `/cd <path>` (within allowed roots),
  `/status` (session, dir, tier, model, effort, spend). `/model` and `/effort`
  set per-session overrides (empty ⇒ the config default) and take effect on the
  next turn.

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
  `Restart=always`, `WantedBy=multi-user.target`. Runs as **root** (§4.6).
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
  per-session state: `BITCH_MODEL`/`BITCH_EFFORT` set the default and `/model`
  `/effort` override it live per chat, so switching is trivial. *(Implemented.)*
- Pair Opus with the budget guard (§4.4) — Opus + agentic loops burn tokens.

### 9.2 Go implementation path — **chosen: Path B**
- **Path B — Go bridge orchestrates the Claude Code CLI.** Go owns Telegram +
  process lifecycle; it spawns `claude -p --output-format stream-json --resume …`.
  Claude Code brings the maintained harness: built-in tools, sessions, MCP, and a
  real permission system. Tiers = permission profiles; approvals = a `PreToolUse`
  hook that calls back into the bridge → Telegram. Reuses Anthropic's
  security-critical tooling; bridge logic is all Go. This path's permission gate
  is also what makes the full-root posture (§4.6) tolerable — it's the boundary
  that survives root.
- **The CLI is a version-coupled dependency, so it is pinned.** The bridge
  depends on the CLI's `stream-json` event shapes, the PreToolUse stdin/stdout
  hook contract, and specific flags (`--session-id`, `--resume`, `--effort`,
  `--settings`). `deploy/setup.sh` installs a **pinned version**
  (`CLAUDE_VERSION`); upgrades are deliberate: bump the pin, rebuild, retest.
  The bridge **preflights** `claude --version` at boot so a missing or drifted
  binary fails startup loudly, not mid-turn.
- *(Not chosen)* **Path A — pure Go with `anthropic-sdk-go`.** Self-defined tools
  + gating; more control but you'd own the bash/file plumbing and its security.

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
| Box compromised | In-box reach is total (root); cloud firewall bounds egress, scoped creds bound Git reach, rebuild from script |

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

- **Go implementation path:** Path B — orchestrate the Claude Code CLI. §9.2
- **Privilege posture:** full root, with the permission gate + Hetzner Cloud
  Firewall + disposable-box model as the compensating controls. §4.6
- **Telegram surface:** start 1:1 private chat; routing built group-ready. §11

All primary decisions are resolved — the doc is ready to build against.

---

## 13. Suggested build phases

1. **Provision & harden** — spin up the Hetzner CAX21 box, SSH key-only (admin
   user for SSH), Hetzner Cloud Firewall with egress limits, Go + toolchains.
   (§4.5, §4.6, §5) — *planned in DEPLOY.md, pending secrets.*
2. **Skeleton** — Go bot + user-ID gate + echo. Prove the transport and that only
   you can drive it. — **done**
3. **Agent wired in (T0 advisory)** — read-only Claude answering in-chat, with
   session continuity. No write powers. (Chosen path from §9.2.) — **done**
4. **Constrained (T1)** — edits in workdir, safe-bash allowlist, Telegram approval
   for risky ops. — **done** (reviewed and hardened; see CODE-REVIEW.md)
5. **Hardening** — systemd service, logging, budget guard, secrets in place.
   — **done** (deploy artifacts in `deploy/`)
6. **Later** — additional agents / the pipeline layer (§11).
