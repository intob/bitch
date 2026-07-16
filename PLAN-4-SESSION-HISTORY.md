# PLAN 4 — session history: /sessions and /resume

Status: **proposal, not yet implemented.** Build order: **4 of 6** —
independent of the others; benefits from PLAN-1-QUEUE.md's `runTurn`
extraction for the recording hooks. Authority for intent is `DESIGN.md`;
read alongside `CLAUDE.md` (invariants).

## 1. The ask

One session per chat, and `/new` / `/cd` are destructive-in-practice: they
clear `ClaudeSessionID` and the bridge forgets the old id forever, even
though the Claude session itself survives on the box's disk. There is no way
to hop back to yesterday's debugging session. This is purely a bookkeeping
gap — a history table plus a switch command.

## 2. Design

### Recording

New `session_history` table; a row per Claude session the bridge has ever
started for a key:

- `chat_id, thread_id, bot_id, claude_session_id` (PK: key +
  claude_session_id), `workdir`, `title`, `created_at`, `last_used`.
- `title` = first ~64 chars (rune-safe) of the first prompt of that session,
  captured in `runTurn` when the turn started with an empty
  `ClaudeSessionID` (i.e. the fresh-session case — same condition that
  triggers `--session-id`).
- `last_used` touched on every completed turn.
- Pruned to the newest 20 rows per key (on insert).

Job sessions (PLAN-3-JOBS.md, negative ThreadID) are recorded too but
filtered out of the interactive `/sessions` listing — their continuity is
managed by the job, not the human.

### Listing & resuming

- `/sessions` — numbered, newest-first:
  `1. [2h ago] fix policy test flake — /root/work/bitch`.
- `/resume <n>` — sets the chat session's `ClaudeSessionID` **and**
  `WorkDir` from the history row. Claude Code sessions are project-scoped
  (the same fact that makes `/cd` clear the id, `commands.go:114`), so
  resuming without restoring the dir would feed `--resume` a session from
  another project. The restored workdir passes through the same jail check
  as `/cd` (`resolveDir`) in case roots changed since.
- Refused while a turn is running for the key ("finish or /stop first") —
  swapping the session id under a live turn would corrupt the id-routing
  the approval hook depends on.
- `/new` and `/cd` keep their semantics; they are no longer lossy because
  the id they drop is now in history.

### Expired sessions

If the resumed session is gone on disk (expired, rebuilt box), the existing
"No conversation found → clear and retry fresh" fallback
(`bridge.go:306-311`) already catches it on the next turn. After an explicit
`/resume`, silence there would be confusing — add a user-visible note in
that path: "⚠️ that session no longer exists — started fresh." Also delete
the dead history row.

## 3. Changes

- `internal/store/history.go` (new) — table (idempotent `CREATE TABLE` in
  `migrate`), `RecordSession`, `TouchSession`, `ListSessions(key, n)`,
  `DeleteSession`; prune-to-20 inside `RecordSession`.
- `internal/bridge/bridge.go` — record on fresh-session init, touch on
  completed turns (both inside `runTurn`); the fresh-fallback note +
  dead-row cleanup.
- `internal/bridge/commands.go` — `/sessions`, `/resume <n>`, `helpText`.
- `DESIGN.md` — §6 paragraph on history/resume; same commit.

## 4. Tests

- Store round-trip, prune-to-20, job-session filtering.
- Title truncation (rune-safe, multibyte input).
- `/resume` restores workdir and re-jails it; refuses while busy; rejects a
  bad index. (Command handlers with an in-memory store, table-driven.)

## 5. Invariants check

- **Fail closed** — an unresolvable/out-of-jail stored workdir refuses the
  resume rather than proceeding; expired sessions degrade to fresh with a
  notice.
- **Session id routes approvals** — the swap is refused while a turn is
  live, so `GetByClaudeSession` never sees a mid-turn id change.
- No new Session columns — history is its own table; the Session-field
  three-queries+migration rule is not triggered.
