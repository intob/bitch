# PLAN 3 — scheduled / proactive turns (jobs)

Status: **implemented, 2026-07-16** — see DESIGN.md §6.1, which is now the
authority; this doc is kept as the rationale record. Build order: **3 of 6**.
Per-job spend cap was **decided: yes** (user, 2026-07-16).

Shipped as planned, with two deltas: **quiet mode stayed deferred and its
`quiet` column was not created** (an unused column is speculative — phase 2 can
`ALTER TABLE`), and `runTurn` gained a `label` (activity-card tag) and returns a
short outcome word for the job's `last_status`.

## 1. The ask

The box is always-on, but the agent only reacts: it does nothing between
messages. Wanted: "every night at 02:00, pull main, run the tests, and
message me only if something broke." Scheduled turns are where the always-on
agent stops being SSH-with-extra-steps and starts being an assistant.

## 2. Design

### Jobs are sessions with a synthetic thread

Reuse the existing machinery instead of building a parallel one: a job's
turns run under `store.Key{ChatID, ThreadID: -jobID, BotID}`. That gives
each job, for free:

- its own persisted `ClaudeSessionID` — continuity across runs, **without
  clobbering the interactive session's context**;
- its own session row (workdir, tier, model, effort);
- approval routing via the existing `GetByClaudeSession` path (§4.3) — the
  hook doesn't know or care that the turn was scheduled;
- the one-turn-per-key busy gate.

`ThreadID` is always 0 for real chats today, so negative IDs cannot collide;
if forum topics ever land, real thread IDs are positive. Document this in
the §11 group-readiness note — jobs are the first concrete consumer of the
group-ready keying.

### Approvals while asleep fail closed

A scheduled turn at T1 that hits a risky op sends the normal approval card;
timeout ⇒ deny; the turn proceeds or fails accordingly. That is correct
behavior — no special casing. Jobs inherit the chat's tier at creation time
(copied into the job's session row, shown in the `/jobs add` confirmation so
there are no surprises); changing it later is `/tier` in… no — the job's
session has no chat to type into, so add `/jobs tier <id> <0-3>` reusing the
same confirm-button flow as `/tier`.

### Per-job spend cap (decided: required)

New failure mode: a runaway job burning the daily budget overnight before
the user wakes up. Every job has `cap_usd REAL NOT NULL` — default **$2.00**
per run, settable at add time (`/jobs add … cap=5`). Enforcement:

- Before each scheduled run: if `SpendToday() + cap_usd` would exceed the
  daily budget, skip the run and post "⏭️ job #N skipped — would risk the
  daily budget" (fail closed on money).
- The cap itself is a *pre-flight reservation*, not a mid-turn kill: the CLI
  reports cost only at the result event, so a hard mid-turn abort isn't
  available at this layer. The daily budget guard remains the backstop. Be
  honest about this in `/jobs` help text.

### Scheduler

- `robfig/cron/v3`, parse + next-time only (accepts both cron specs and
  `@every 4h`). Hand-rolling cron is how bugs happen; the dep is small and
  maintained. All times UTC, stated in `/jobs` output.
- Own goroutine started from `bridge.Run` — never touches the poll loop.
  Every 30 s: `store.DueJobs(now)` → for each, `go b.runJobTurn(job)`. If
  the job's key is busy, skip; the next tick retries.
- **Persist `next_run` before launching the turn.** Crash ⇒ a missed run,
  never a double run (fail closed applies to spending too).
- On boot: an overdue job runs once; older misses collapse into that one.

### Job turns

`runJobTurn` loads/creates the job's session row, prepends a short preamble
("This is scheduled job #N (spec `…`). Task: <prompt>"), and calls the
shared `runTurn`. Output goes to the owning chat; the activity card renders
as usual, titled with the job id.

### Quiet mode (phase 2)

"Only message me if broken" is mostly prompt-level. Mechanical assist,
deferred: a `quiet` flag — when set and the final result text is exactly
`OK` (documented convention, stated in the job preamble), suppress the
reply and just finalize the card.

## 3. Changes

- `internal/store/jobs.go` (new) — `jobs` table: `id INTEGER PRIMARY KEY,
  chat_id, bot_id, spec, prompt, workdir, cap_usd, quiet, enabled,
  next_run, last_run, last_status`. Idempotent `CREATE TABLE` in `migrate`
  (per the CLAUDE.md migration rule). CRUD + `DueJobs(now)`.
- `internal/bridge/bridge.go` — scheduler goroutine; `runJobTurn`; both sit
  on the `runTurn` extraction from PLAN-1-QUEUE.md.
- `internal/bridge/commands.go` —
  - `/jobs` — list: id, spec, next run (UTC), cap, last status.
  - `/jobs add <spec> :: <prompt>` — the `::` separator avoids parsing
    ambiguity between spec and prompt; optional `cap=<usd>` before `::`.
    Reply confirms spec, tier inherited, cap, next run.
  - `/jobs rm <id>` — confirm-button flow (same pattern as `/secrets rm`).
  - `/jobs run <id>` — manual trigger (respects busy gate + budget).
  - `/jobs pause <id>` / `/jobs resume <id>`.
  - `/jobs tier <id> <0-3>` — confirm-button flow (same as `/tier`).
  - `helpText` updated.
- `go.mod` — `robfig/cron/v3`.
- `DESIGN.md` — new §6.1 (jobs) + a line in §11; same commit.

## 4. Tests

- Store: CRUD, `DueJobs` boundary conditions, idempotent migration on an
  existing DB file.
- `next_run` computation table tests (cron spec + `@every`, UTC).
- Scheduler with an injected clock: fires when due, skips when busy, never
  double-fires across a simulated crash/restart (next_run persisted first).
- Spend-cap: run skipped when `SpendToday + cap` exceeds budget; cap
  parsing in `/jobs add`.

## 5. Invariants check

- **Poll loop never blocks** — scheduler is its own goroutine; job turns
  are `go`-spawned like interactive ones.
- **Fail closed** — approval timeout denies (unchanged); crash loses a run
  rather than doubling it; budget-risk skips the run.
- **One turn per key** — job keys are distinct from chat keys, so a job and
  an interactive turn may run concurrently in the same chat. That is
  intended (the job must not block the human), and each stream serializes
  within itself. The known daily-budget read race (CLAUDE.md "Known
  limitations") widens slightly; still accepted for single-user.
- **Session-field rule** — no new Session fields; jobs reuse the existing
  row shape keyed by synthetic thread.
