# bitch — Deployment Plan (Hetzner provisioning)

**Status: DEPLOYED & VERIFIED — 2026-07-16.** The bot (@<bot>) is live
on the box and passed end-to-end verification (answers, `/status`, and the T1
approval gate resolving instantly both ways). See the deployment record in §0.
**Mode:** Full auto — Claude provisioned everything via the Hetzner Cloud API,
then SSHed in to harden + deploy. Decided with the user 2026-07-16.

This doc is self-contained so a fresh session can pick up mid-flow. It planned
build **phase 1** (§13 of [`DESIGN.md`](./DESIGN.md)) plus the deploy of the v1
bridge implemented in this repo.

---

## 0. Deployment record (2026-07-16) — what actually happened

| | |
|---|---|
| **Box** | `bitch` · **cx33** (Intel x86, 4 vCPU / 8 GB / 80 GB) · **nbg1** (Nuremberg) · Ubuntu 24.04 |
| **IP** | `<box-ip>` (IPv6 `<box-ipv6>`) |
| **SSH** | `ssh -i ~/.ssh/bitch_hetzner admin@<box-ip>` (root login disabled; admin has passwordless sudo) |
| **Auth** | **Max-plan subscription** via `CLAUDE_CODE_OAUTH_TOKEN` (from `claude setup-token`); budget guard **off** (`BITCH_DAILY_BUDGET_USD=0`) |
| **Bot** | @<bot> (id <bot-id>); allowlist = user <user-id> |
| **Deployed commit** | `a24d87f` on `main` |

**Deviations from the original plan (all deliberate):**
- **x86 `cx33`, not ARM `cax21`.** The entire ARM CAX line was sold out across
  all three EU regions at deploy time. `cx33` is the exact-spec x86 equivalent;
  the workload is arch-agnostic (`setup.sh` now auto-detects arch).
- **Region `nbg1`, not `fsn1`** — same reason (capacity), still EU.
- **Max-plan auth, not an Anthropic API key** — user's choice. Consequence: the
  $/day budget guard is inert (no per-token cost reported), so the real ceiling
  is the Max usage limit. `BITCH_DAILY_BUDGET_USD=0`.
- **Firewall egress adds TCP 80** (beyond the planned 443/53/22) — Ubuntu `apt`
  and unattended security upgrades fetch over HTTP.

**Bugs found during the live rollout and fixed** (commit `a24d87f`; all were
invisible to unit tests because they only manifest with a real spawned `claude`
child): relative `--settings` path, prompt swallowed by variadic `--add-dir`
(now sent on stdin), and stale-session recovery. Details in the commit message.

**Still outstanding for the user — see the checklist in §4.**

---

## 1. Decisions already made

| Thing | Choice | Why / ref |
|---|---|---|
| Approach | **Full auto** — Claude creates server, firewall, hardens, deploys | user picked, 2026-07-16 |
| Box | **CAX21** (4 vCPU / 8 GB ARM, ~€7/mo) | §5; resizing up is easy/reversible |
| Region | **Falkenstein `fsn1`** (EU; CAX is EU-only) | §5 |
| OS image | **Ubuntu 24.04 LTS** (ARM64) | §5 |
| Starting tier | **T1 constrained** | §4.3 — start closed |
| Budget guard | **$20/day** (`BITCH_DAILY_BUDGET_USD=20`) | §4.4 |
| Model / effort | **claude-opus-4-8 / high** | §9.1 |
| Firewall inbound | **SSH (22) from the user's current public IP only**; no app ports | §3, §4.5 |
| Firewall egress | allow outbound **443 (HTTPS)**, **53 (DNS)**, **22 (SSH/git)**; deny rest | §4.5/§4.6 — the control root can't flush |
| SSH keypair | dedicated `~/.ssh/bitch_hetzner`, generated on the laptop; public half uploaded to Hetzner | §4.5 |
| Repo module path | `github.com/intob/bitch` (private repo already pushed) | — |

**Deferred / can widen later via env + `/tier`:** egress rules, tier, budget,
model. All live in `/etc/bitch/bitch.env` on the box.

---

## 2. Secrets to collect from the user (blocking)

Claude cannot proceed with provisioning until it has these. They will be pasted
into the chat (full-auto mode) → they land in the transcript → **rotate the
Hetzner token afterward** (most powerful); the rest live only on the box.

1. **Hetzner Cloud API token** — project-scoped, **Read & Write**.
   Console → project → Security → API Tokens → Generate.
2. **Telegram bot token** — user creates via **@BotFather** (`/newbot`). Format
   `123456:ABC-...`. (Interactive in the Telegram app — Claude can't do this.)
3. **User's numeric Telegram user ID** — from **@userinfobot** → `BITCH_ALLOWED_USERS`.
4. **Anthropic API key** — `sk-ant-...` → `ANTHROPIC_API_KEY` for the `claude` CLI.
5. *(Optional)* **Git PAT** (fine-grained) + which repos to clone under
   `/srv/work`. If skipped, no repos are cloned yet.

The user's **public IP** for the SSH firewall rule is auto-detected (Claude runs
from the user's laptop, so SSH originates from that IP) — no need to provide it.

---

## 3. Execution steps (what Claude runs)

Tooling: `curl` against the Hetzner REST API (`https://api.hetzner.cloud/v1`), or
`brew install hcloud` first — either works. `ssh`/`ssh-keygen` already present.

1. **Preflight**
   - `brew install hcloud` (or use curl); export the token.
   - Detect public IP: `curl -s https://ipv4.icanhazip.com`.
2. **SSH key**
   - `ssh-keygen -t ed25519 -f ~/.ssh/bitch_hetzner -N "" -C bitch-hetzner`.
   - Upload public key to Hetzner (`POST /ssh_keys`).
3. **Firewall** (`POST /firewalls`)
   - inbound: TCP 22 from `<user-ip>/32` only.
   - egress: TCP 443, TCP/UDP 53, TCP 22 to `0.0.0.0/0`; deny the rest.
4. **Server** (`POST /servers`)
   - name `bitch`, type `cax21`, image `ubuntu-24.04`, location `fsn1`,
     ssh_keys = the uploaded key, firewalls = the created firewall.
   - (Root login stays enabled only until step 6 hardens it; setup.sh creates an
     `admin` user and disables root SSH login — §4.5/§4.6.)
5. **Wait for boot**, grab the public IPv4, wait for SSH to answer.
6. **Provision + harden** — copy the repo to the box and run `deploy/setup.sh`
   (installs Go/Node/Claude CLI, ufw, fail2ban, unattended-upgrades, admin user,
   SSH key-only + no root login, builds bitch, installs the systemd unit,
   creates `/srv/work` and `/etc/bitch/`). Reachable at `git@github.com:intob/bitch`
   (private) — clone via the Git PAT, or `scp` the working tree.
7. **Configure secrets** — write `/etc/bitch/bitch.env` (chmod 600) from
   `deploy/bitch.env.example` with the real token, user ID, API key, budget,
   model. Never committed.
8. *(Optional)* **Wire Git credentials + clone repos** into `/srv/work`, per §4.5.
   Fine-grained PAT scoped to the target repos (never a full-access token), stored
   on disk so the from-scratch child env doesn't need to carry it:
   `git config --global credential.helper store` then write
   `/root/.git-credentials` (mode 600) as
   `https://x-access-token:<PAT>@github.com`. `contents:read` clones/pulls;
   add `contents:write` only for pushes (still gated). The policy denylist blocks
   the agent from reading `.git-credentials`.
9. **Start + verify**
   - `systemctl enable --now bitch`; `journalctl -u bitch -f`.
   - Confirm the startup log line ("bitch online as @… ; N allowed user(s)…").
   - User sends the bot a message → confirm it answers and that `/status` works.
   - Sanity-check the gate: an approval prompt appears for a risky op at T1.

---

## 4. Post-setup checklist

Done during the 2026-07-16 rollout:
- [x] Bot answers the user (verified: read `/srv/work`, correct answer).
- [x] `/status` works (dir `/srv/work`, tier T1, model/effort defaults, budget off).
- [x] A risky op triggers a Telegram ✅/❌ approval at T1 — **both ✅ and ❌ resolve instantly** (the concurrency-fix path).
- [x] `systemctl is-active bitch` = active.
- [x] **Survives reboot** — verified: box rebooted, service auto-started ~9s after boot with no intervention.
- [x] **Hetzner Cloud Firewall** attached — inbound SSH from the user's IP only; egress 443/53/22/80.

**Still for the user to do:**
- [ ] **Rotate/delete the Hetzner API token** used for provisioning (it was pasted into the chat transcript — most powerful credential; do this first).
- [ ] Consider rotating the **Telegram bot token** (@BotFather) and **revoking the `claude setup-token` OAuth token** (Anthropic account settings) — both were in the transcript. The agent keeps working; you'd just re-issue and update `/etc/bitch/bitch.env`.
- [ ] Back up the private key `~/.ssh/bitch_hetzner`; note the box IP `<box-ip>`.
- [ ] Set a **Max usage cap** if you want a spend ceiling (the $/day guard is off under subscription auth).
- [ ] Optional: wire a scoped Git PAT (fine-grained, never a full-access token) via `git config --global credential.helper store` + `/root/.git-credentials` (mode 600, `https://x-access-token:<PAT>@github.com`), then clone repos under `/srv/work` so the agent has real projects. `contents:read` clones/pulls; `contents:write` for gated pushes. See §4.5 / setup.sh step 2.

---

## 5. Notes for the resuming session

- The v1 bridge is already built, tested, and pushed to
  **https://github.com/intob/bitch** (`main`). Deploy artifacts live in `deploy/`.
- `~/go.work` (the user's global workspace) includes `./bitch`; there is no
  repo-local `go.work`.
- Full-root, internet-exposed posture is deliberate (§4.6); the **permission gate**
  (`internal/policy`) and the **Cloud Firewall egress** are the load-bearing controls.
- If the user preferred not to paste secrets, the fallback modes discussed were:
  (b) Claude provisions, user SSHes in to inject secrets; (c) Claude only installs
  hcloud + a provisioning script the user runs. User chose full auto.
- Telegram forum-topic routing is not wired (lib v5.5.1 predates it); 1:1 works,
  keying is group-ready (§11).
