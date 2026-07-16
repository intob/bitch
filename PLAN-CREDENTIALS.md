# PLAN — persistent credential store ("provide it once")

Status: **implemented, 2026-07-16** — see DESIGN.md §4.7, which is now the
authority; this doc is kept as the rationale record. Landed as two commits: the
vault itself (`1e51046`) and the non-secret facts sibling (`94215d0`) that keeps
ordinary recall out of it. Read alongside `CLAUDE.md` (invariants) and the
git-PAT precedent (commit `4489786`).

Shipped with the three non-negotiables below intact: the store is framed as
agent-readable convenience, `ValidSecretName` is an exec-affecting-var denylist,
and the DB path is the hardcoded `/etc/bitch/secrets.db`. `secret_get` does not
exist and must not.

> **Revised after a Fable high-effort security review.** Three corrections are
> baked into the sections below and are non-negotiable for v1:
> 1. **Env-injected secrets are agent-readable — full stop.** A shell that can run
>    `echo $X`, `printf`, or `cat /proc/self/environ` (all auto-allowed at T1+)
>    can read any env var. The earlier "value never re-enters model context" claim
>    was wrong and is deleted. This is a **convenience credential store, readable
>    by the agent and by any workdir code it runs.** Frame it that way everywhere.
> 2. **Name validation must be an exec-affecting-var denylist**, not just a
>    charset + reserved-prefix rule — otherwise a saved `LD_PRELOAD` / `BASH_ENV` /
>    `GIT_SSH_COMMAND` / `NODE_OPTIONS` runs attacker code in the next root tool
>    subprocess. See §4.3.
> 3. **The vault path is hardcoded `/etc/bitch/secrets.db`, not configurable** —
>    `internal/policy` is pure and can't read config, so a config-driven path can't
>    be covered by the static deny regex and would fail open. See §4.2.
>
> The real backstop for the widened blast radius is the **disposable box**, not the
> egress firewall (DEPLOY.md provisions egress as 443→`0.0.0.0/0`, so HTTPS
> exfil is not contained). `secret_get` must never exist. See §6.

## 1. The ask

> When I ask the agent to do something and it asks for a username, API key or
> another credential, that credential should be stored on the box so I don't have
> to provide it again in future for another (possibly unrelated) task.

So: the agent hits a task that needs a credential it doesn't have, asks for it
over Telegram, I paste it, it uses it — **and** it's kept, so a later task in a
fresh session (`/new`, `/cd`, a rebuilt session id) doesn't ask again.

## 2. Is it possible? Yes — and there's already a precedent

This is a generalization of what commit `4489786` did by hand for GitHub. The git
PAT lives on disk at `/root/.git-credentials`; `git` reads it transparently
because the child env is built from scratch and can't carry a token
(`agent.childEnv`, `agent.go:252`); and the policy gate denies the agent reading
it back (`policy.go:177`, the `.git-credentials` term). That triad —
**store on the box / use transparently / deny direct read** — is exactly the
shape this feature needs, just made general and driven at runtime instead of at
provisioning time.

What's *missing* today, and what this plan adds:

- The value the user pastes flows into the current Claude session's context and is
  used there, but is **lost the moment the session resets** (`/new` and `/cd`
  clear `ClaudeSessionID`; a failed resume starts fresh — `bridge.go:241`). There
  is no persistence across sessions/tasks.
- There is no way for the agent, at the *start* of an unrelated task, to know a
  credential already exists, so it re-asks.

## 3. The three sub-problems

Any design has to answer all three:

1. **Capture** — how does bitch learn that a given message is a credential, and
   under what *name* to file it? (The agent asks in free text; the user's reply is
   currently just the next prompt.)
2. **Storage** — where does the value live, encrypted/permissioned how, and how is
   it kept from leaking back into a transcript?
3. **Reuse** — how does a future turn get the value *without re-asking*, ideally
   without re-exposing it to the model?

A useful observation that shapes the whole design: **at capture time the secret is
already in model context** — the user pasted it in reply to the agent's question,
into the resumed session. So capture doesn't *worsen* exposure. Note the Fable
correction: reuse via the environment does **not** hide the value from the model
either (a shell reads env freely). The environment is the *general* reuse
mechanism, chosen for breadth, not confidentiality; the value stays readable by the
agent. If you need a credential kept *from* the model, the only option is a
deny-listed tool-native file (like the git PAT), which the gate blocks the agent
from `cat`-ing — but that doesn't generalize to arbitrary services.

## 4. Recommended design

A **box-wide credential vault** the bridge owns, with three moving parts:

### 4.1 Capture — an MCP "secrets" tool the agent calls

Wire a small stdio MCP server into the claude child (same pattern as the `bitch
hook` subcommand: a new `bitch mcp-secrets` subcommand in `cmd/bitch`, registered
in the generated `settings.json`, `agent/settings.go`). It exposes:

- `secret_save(name, value, description)` — persist a credential. The agent calls
  this when the user provides one ("I'll remember your `GITHUB_TOKEN` for next
  time"). Value is already in context, so nothing new leaks.
- `secret_list()` → `[{name, description}]` — **names and descriptions only, never
  values.** Lets the agent check what it already has before asking.

The MCP server talks to the bridge over the **existing loopback approval channel**
(shared-secret HTTP on `127.0.0.1:8765`, `bridge/approve.go`) or a sibling
endpoint — reuse the auth that's already there rather than inventing a second one.
`secret_save` should route through an **Ask** confirmation card ("Save credential
`GITHUB_TOKEN`? ✅/❌") so persisting a secret is a human-gated act, consistent
with the fail-closed posture — the human is already right there having just pasted
it.

Why agent-driven and not a `/secret` command as the primary path: the agent asks
mid-turn; forcing the user to break out to `/secret set NAME` and re-paste is two
manual steps and loses the flow. The agent naming the secret (from the very task
context that needed it) is the seamless path. A `/secrets` command still exists —
see §4.4 — but for *management*, not capture.

### 4.2 Storage — a dedicated, deny-listed vault

- A **separate DB file at a hardcoded `/etc/bitch/secrets.db`** — *not* a
  configurable path, and *not* mixed into the session `bitch.sqlite`. Rationale
  (from the review): `internal/policy` is dependency-free and its `denyPatterns` are
  static literals (`policy.go:162`), so a config-driven vault path could not be
  reflected in the deny regex without threading a closure like `WithinRoots`
  (`policy.go:51`) — and any mismatch **fails open** (the vault becomes `cat`-able).
  `/etc/bitch` is *already* deny-covered (`policy.go:178`) and sits outside both
  `/opt/bitch` and the `Roots` jail, so the Read tool denies it too. One fixed path,
  one clean deny term, no fail-open surface. The store constructor still takes a path
  arg so tests can point at a temp dir; production wiring passes the constant.
- Schema: `secrets(name TEXT PRIMARY KEY, value TEXT NOT NULL, description TEXT,
  created_at INTEGER, updated_at INTEGER)`.
- **Encryption at rest buys little under the full-root posture** (§4.6 — root reads
  everything anyway) and is deferred. The deny pattern protects the file *at rest*;
  per the review it does **nothing** for the value once it's in a child env (§6).
- **What the deny closes:** `cat` is a `safeCommand` and bash safe-commands are
  **not** jail-path-checked (only the Read *tool* is, `policy.go:57`), so an
  absolute `cat /etc/bitch/secrets.db` would otherwise dump the vault. The existing
  `/etc/bitch` term stops it; add a `policy_test.go` case asserting so, per the
  "test per rule" convention.

### 4.3 Reuse — env injection + a names-only manifest

- On every turn, `agent.childEnv` (`agent.go:252`) additionally injects each saved
  secret as an env var, so tools the agent runs (`gh`, a `curl`/SDK it writes,
  test suites needing an API key) pick them up. Note (review finding 5): `git`
  does **not** read an arbitrary `GITHUB_TOKEN` env var — it authenticates via the
  on-disk `.git-credentials`/helper, which is why the PAT uses a file. Env covers
  the genuinely arbitrary "API key for service X" case; keep git on its existing
  on-disk store.
- So the agent *knows* the vars exist and doesn't re-ask, inject a **names +
  descriptions manifest** (no values) into the session — appended to the turn
  prompt (simple, no version-coupled flag needed; `--append-system-prompt` is a
  candidate but is CLI-version-coupled, so don't depend on it): "Stored credentials
  are available as environment variables: `GITHUB_TOKEN` (GitHub PAT for org X),
  `OPENAI_API_KEY` (…). Reference them by variable; do not ask the user for these."
  Inject each turn a secret exists (cheap; also fixes the review's staleness note —
  a secret added via `/secrets set` mid-session is advertised on the very next turn).
- **Name validation is security-critical and must be a denylist, not a charset
  rule.** A saved name becomes an env var in a **root** child, so beyond the
  charset (`^[A-Z][A-Z0-9_]*$`) and base/reserved-prefix rejects (`PATH`, `HOME`,
  …; `BITCH_`, `ANTHROPIC_`, `CLAUDE_`), it must **reject exec-affecting names**
  (review finding 2 — otherwise a saved `LD_PRELOAD=/tmp/evil.so`, `BASH_ENV`,
  `ENV`, `IFS`, `PS4`, `PROMPT_COMMAND`, `GIT_SSH_COMMAND`, `NODE_OPTIONS`,
  `PYTHONPATH`, `PERL5OPT`, `RUBYOPT`, or anything matching `LD_*` / `*_PRELOAD` /
  `*_OPTIONS` loads attacker code in the next root tool subprocess). Enforce the
  full ruleset at `secret_save`/`/secrets set` **and** again at injection time
  (defense in depth — a value smuggled into the DB out-of-band still can't inject a
  dangerous var). This is a hard code-exec boundary, not a nicety.

## 5. Alternatives considered

| Axis | Recommended | Alternative | Why not (for v1) |
|---|---|---|---|
| Capture | Agent MCP `secret_save` | `/secret set NAME` command | Breaks the mid-turn flow; two manual steps. Kept for *management* only. |
| Capture | " | Agent `request_secret()` that prompts TG and intercepts the reply out-of-band | Most "magic", most complex: needs per-chat pending-input state racing the poll loop. Over-built for a single-user box; revisit later. |
| Reuse | Env injection (value never re-enters context) | MCP `secret_get(name)` into context | Re-exposes the value every use; only worth it if you want per-secret Ask-gating. Could be added later for high-value secrets. |
| Reuse | " | Tool-native files (`.git-credentials`, `.netrc`, `gh hosts.yml`) | Most transparent but per-tool; doesn't generalize to "API key for arbitrary service X". The PAT already uses this — keep it; env injection covers the rest. |
| Storage | Separate deny-listed DB file | New table in `bitch.sqlite` | Mixing secrets into the session DB widens what one `cat` exposes and needs a path-specific deny anyway. Separate file = one clean deny term. |

## 6. Security analysis — what this touches

Invariants from `CLAUDE.md` this feature bumps against, and the resolution:

- **"Secrets only ever come from the environment (`config.Load`)."** This adds a
  second secret source (the vault → `childEnv`). Deliberate, documented exception;
  the vault is bridge-owned and injected only into the scrubbed child, never read
  via `os.Getenv` elsewhere. Update the invariant text.
- **"The claude child env is built from scratch; never pass `os.Environ()`."** Still
  honored — we add *curated* keys, we don't inherit. The name-validation rule (§4.3)
  is what keeps an injected key from shadowing a base/hook var. The existing test
  asserting `BITCH_TELEGRAM_TOKEN` never reaches the child must still pass and
  should be extended to assert a saved secret can't be named to leak it.
- **Injected secrets are agent-readable (review finding 1 — the load-bearing
  correction).** Once a secret is in the child env, the Bash tool's shell inherits
  it and the *auto-allowed* allowlist reads it back: `echo $X`, `printf`,
  `cat /proc/self/environ`, `set`, `${!x}` — `echo`/`printf`/`cat` are all
  `safeCommands` and none of these match a deny pattern, so at T1+ they run without
  approval and can stream every secret into the transcript → Telegram. This is
  reachable by prompt-injection (first-class threat, §4.3), not just the operator.
  **You cannot keep an env var confidential from a shell.** The deny pattern
  protects the file at rest, not the value. Posture, stated plainly: **v1 is a
  convenience store whose contents are readable by the agent.**
- **Blast radius, and what actually backstops it (finding 3).** Today one narrow,
  revocable credential (the git PAT) is workdir-reachable; §4.6 accepts that.
  Blanket env injection makes **every** stored secret readable by any `go test` /
  `npm test` / `make` — or a malicious repo's test suite via prompt-injection — and
  DEPLOY.md provisions egress as **443→`0.0.0.0/0`**, so a `POST https://attacker/…`
  exfiltrates them. The egress firewall — "the one control root can't defeat"
  (§4.6) — does **not** contain HTTPS exfil. So the box becomes an all-secrets
  honeypot, and the **disposable box is the real backstop**, not the firewall. This
  is a qualitative shift and gets the loudest callout in DESIGN.md. It's why we
  ship with the honest framing and recommend the user store only what they can
  tolerate on an agent-readable box (§9).
- **`secret_get` must never exist (finding 6).** `BITCH_HOOK_SECRET` is in the
  child env (`agent.go:255`), so workdir code running as root can already call the
  loopback secrets endpoint directly, bypassing any MCP tool. That's tolerable for
  `secret_save` (Ask-gated) and `secret_list` (names only) but means a value-
  returning endpoint/tool would be an unauthenticated read of the whole vault from
  inside the child. No `secret_get`, ever.
- **Exposure is tier-independent.** Secrets are injected at T0/T1 too, which sits in
  tension with "keep T1 the operating tier longer" (§4.6). Accepted for v1 (the
  feature is useless if credentials vanish at low tiers); noted as a future
  scoping knob (§9).
- **Telegram residency.** `/secrets set NAME value` and the mid-turn paste leave
  plaintext in Telegram's cloud history. v1 should `deleteMessage` on the
  `/secrets set` command message right after ingest; document that prior pastes
  persist server-side regardless.
- **Fail-closed:** unknown/failed vault read → inject nothing (task proceeds, agent
  falls back to asking). Save failure → tell the user it wasn't saved. A name that
  fails validation → refuse to save/inject. Never silently continue as if saved.

## 7. Implementation plan (phased)

Each phase builds + tests green (`go build ./cmd/bitch`, `go test ./...`,
`go vet ./...`).

Building **1–3 + 5 + 6 now** (the tested core); **4 deferred**.

1. **Store layer** (new file `internal/store/secrets.go`, same `*Store`/mutex): a
   `secrets` table + idempotent `CREATE TABLE IF NOT EXISTS` in `migrate`;
   `SaveSecret`, `ListSecrets` (names+descriptions), `AllSecrets` (name→value, for
   injection), `DeleteSecret`. **`validName` denylist helper** (charset +
   base/reserved-prefix + exec-affecting-var rejects, §4.3) with exhaustive tests —
   this is the security-critical unit. Save and inject both call it.
2. **Policy hardening.** Add a `policy_test.go` case asserting `cat`/`Read` of
   `/etc/bitch/secrets.db` is denied at every tier. The existing `/etc/bitch` term
   already covers it (`policy.go:178`) — the test locks that in and is done first.
3. **Env injection + manifest.** Add `Secrets map[string]string` (+ a built
   `Manifest string`) to `agent.RunRequest`; the bridge fills them from the store
   before `Run`. `childEnv` appends the validated secrets; `Run` appends the
   manifest to the prompt. Re-validate names in `childEnv` (defense in depth).
   Extend the childEnv scrubbing test: `BITCH_TELEGRAM_TOKEN` still absent; a secret
   named `LD_PRELOAD`/`PATH`/`BASH_ENV` is **not** injected.
4. **[DONE] MCP capture server** — `bitch mcp` subcommand (`cmd/bitch/mcp.go`),
   registered via `agent.writeMCPConfig` + `--mcp-config`/`--strict-mcp-config`.
   Exposes `save_secret` and `remember_fact`; persists via the loopback `/store`
   endpoint. Confirmation reuses the PreToolUse gate (the tool call is Ask at
   T1/T2) rather than a new blocking flow, with the secret value redacted from the
   card (`policy.isSecretCaptureTool`). No value-returning tool — §6 holds.
5. **`/secrets` management command** (`bridge/commands.go`): `/secrets` lists
   names+descriptions; `/secrets set NAME value` saves (validates the name; on
   success `deleteMessage` to scrub the plaintext from Telegram, §6); `/secrets rm
   NAME` deletes with a confirm card. Add to `helpText`. This is the v1 capture path.
6. **Docs.** New `DESIGN.md` §4.7 (credential vault) extending §4.5 with the honest
   posture (§6 here); update the §12 open decision; update `CLAUDE.md` invariants
   (secret sources now include the vault; childEnv gains curated keys; name denylist);
   note in `DEPLOY.md` that the PAT is the first instance of a general mechanism and
   that stored secrets widen the disposable-box rationale.

## 8. Test plan

- Unit: name validation (accept `GITHUB_TOKEN`; reject `path`, `PATH`, `BITCH_X`,
  `1ABC`, `A-B`); store round-trip; `List` never returns values.
- Policy: `cat`/`Read` of the vault path denied at every tier (test-per-rule).
- childEnv: saved secrets present as vars; `BITCH_TELEGRAM_TOKEN` still absent; a
  secret named to collide with a base var is rejected/ignored, not injected.
- Live (the only way to exercise capture/Ask, per `CLAUDE.md`): at T1, prompt a
  task needing a credential → agent asks → paste → confirm the "Save credential?"
  card → `/new` → new task in the same domain → agent uses it **without asking**.
  Confirm `secret_list` shows it and the value never appears in any reply.

## 9. Decisions taken for v1

The user said "do what you think is best." Recorded here so DESIGN.md can absorb them:

1. **Blanket env injection** (all stored secrets present each turn). Scoped/gated
   injection reduces blast radius but breaks the core promise ("don't re-ask") — the
   agent can't reliably predict from the prompt which secret a task will need. Ship
   blanket with the honest "agent-readable convenience store" framing (§6); keep
   *scoped-by-referenced-name* documented as a future hardening knob for users who
   want to trade the no-re-ask guarantee for a smaller radius.
2. **Injection is not tier-gated** — credentials must work at T1 where the user
   operates. Noted as a knob.
3. **Box-wide scope**, not per-chat (single-user box; cross-task reuse is the point).
   Keying stays group-ready internally (§11) so a per-scope split stays additive.
4. **No encryption at rest** for v1 (root reads it anyway; the deny pattern is the
   real file-at-rest control). Passphrase-derived key can be added later.
5. **MCP auto-capture deferred** (version-coupled, only live-testable). v1 capture is
   `/secrets set NAME value`; the manifest tells the agent to instruct the user to
   run it. The reuse half — the actual "don't re-ask" win — is fully delivered.

Still genuinely the user's call (surfaced, not blocking the build): whether a
`secret_save`/`/secrets set` should require an **Ask confirmation card**. Building it
*with* the confirm (fail-closed, human already present); trivially removable.
