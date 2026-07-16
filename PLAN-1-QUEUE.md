# PLAN 1 — queue messages during a turn

Status: **phase 1 done (2026-07-16).** Phase 2 (§3) remains a proposal and is
deliberately not started. Build order: **1 of 6** (smallest, no new deps,
immediate daily-use win). Authority for intent is `DESIGN.md`; read alongside
`CLAUDE.md` (invariants).

Landed as designed: `activeRun.queue`, `claimOrQueue` (gate + append in one
critical section), `nextTurn` (pop-or-release in one critical section), the
drain loop in `handleAgentTurn`, `runTurn` extracted for PLAN-3/PLAN-4 to build
on, `/stop` clearing the queue. `DESIGN.md` §6 records the behavior; tests in
`internal/bridge/queue_test.go`.

## 1. The ask

A prompt sent while a turn is running is refused and **lost**
(`bridge.go:196-198` replies "⏳ still working" and drops the text). On a
phone, where turns run minutes and thoughts arrive as they occur, retyping
"also check the config file" after the turn ends is real friction. Queue it
instead; nothing the user types should ever be discarded.

## 2. Phase 1 — queue-and-drain (this plan)

### Behavior

- A message arriving while the chat's turn is running is **queued**, ack'd
  with "📥 queued — runs after the current turn (/stop cancels both)."
- When the running turn finishes, all queued messages are drained, joined
  with blank lines, and run as the **next turn** in the same session.
  Messages arriving during the drain keep appending.
- `/stop` cancels the running turn **and clears the queue**. A user hitting
  stop wants the work stream stopped, not a surprise follow-up turn.
- Bounds: max 10 queued entries / 8 kB total per chat. Overflow gets the old
  "still working" refusal — bounded memory, and past that point the user is
  fighting the tool anyway.

### Design

- `activeRun` (`bridge.go:41`) gains `queue []string`, guarded by the
  existing `b.mu`.
- `handleMessage`'s busy path appends instead of refusing.
- `handleAgentTurn` grows a drain loop around the current body: after a turn
  completes, under `b.mu`, pop the whole queue; if non-empty, **keep the
  `b.runs` entry** and loop with the combined prompt; if empty, delete the
  entry as today. The budget guard (§4.4) runs per iteration. Each iteration
  gets a fresh `runCtx`/cancel stored back into the entry so `/stop` hits the
  currently running turn.
- **Ownership rule (the race to not have):** the `b.runs[key]` entry must be
  held continuously from first turn start to final drain-empty deletion.
  There is no window where the queue is non-empty but no goroutine owns the
  key — otherwise a message landing in that window would start a rival
  goroutine. Pop-or-delete happens in one critical section.
- Cancellation: `/stop` clears `queue` under `b.mu` before calling `cancel()`,
  so the drain loop observes an empty queue and exits. The "🛑 stopped" path
  in the turn body already reports; the loop just terminates.
- Media (PLAN-2-MEDIA-IN.md) composes naturally: by the time a prompt is
  queued it is already a plain string (attachment note included).

### Changes

- `internal/bridge/bridge.go` — `activeRun.queue`; busy-path append; drain
  loop; while here, extract the message-independent core as
  `runTurn(ctx, key store.Key, chatID int64, prompt string)` (the body
  already only uses `msg.Chat.ID` and the text). PLAN-3-JOBS.md and
  PLAN-4-SESSION-HISTORY.md build on `runTurn`; land the refactor here.
- `internal/bridge/commands.go` — `cmdStop` clears the queue; `helpText`
  notes the queueing behavior under `/stop`.

### Tests

- Queue append / drain-all / cap-overflow as small bridge methods with a
  stubbed runner (table-driven).
- `/stop` clears the queue (no follow-up turn runs).
- No-rival-goroutine: a message appended between turn-end and drain lands in
  the next combined prompt, not a second concurrent run.

### Invariants check

- **Poll loop never blocks** — unchanged: append + ack are fast and
  synchronous; the drain loop lives in the existing turn goroutine.
- **One turn per chat** — strengthened, not weakened: the busy-gate still
  admits exactly one goroutine per key; queueing changes what happens to the
  loser, not the gate.
- Fail closed — n/a (no security surface touched).

## 3. Phase 2 — true mid-turn injection (separate, do not start yet)

The CLI supports `--input-format stream-json`: stdin stays open and accepts
additional user messages during a run. That reshapes `agent.Run` — stdin
becomes an `io.Pipe` the bridge writes user-message JSON lines into, and turn
termination becomes protocol-level instead of process-exit. This lands on the
pinned-CLI contract (the CLAUDE.md version-coupling list would grow the
input-side event shapes), so treat it like a CLI bump: prototype against the
pinned 2.1.210, rerun `TestScan`, live approval turn, MCP-hook check.

**Only build phase 2 if queue-then-run feels too laggy in practice.**
Queueing removes the data loss, which is the actual pain.
