# PLAN 2 — inbound media: images & documents

Status: **done (2026-07-16).** Build order: **2 of 6** — the
biggest UX unlock for a phone-driven agent. Voice is deliberately **not**
here; it is PLAN-6-VOICE.md (sequenced last). Authority for intent is
`DESIGN.md`; read alongside `CLAUDE.md` (invariants).

Landed as designed: the derived+canonicalized `config.MediaDir` appended to the
jail roots, `internal/bridge/media.go` (download, bridge-side filenames, prompt
construction, boot prune), the `handleMessage` dispatch table, and
`RunRequest.MediaDir` as a second `--add-dir`. Two deviations worth knowing:
filenames carry four random hex after the timestamp (`img-<ts>-<rand>.jpg`),
because an album's updates share a second and `O_EXCL` would fail the second
file; and the 20 MB cap is enforced **twice** — before the fetch from Telegram's
reported `FileSize`, and again as a `LimitReader` on the copy, since the
reported size is a hint and only the copy is a guarantee.

## 1. The ask

Today `handleMessage` reads only `msg.Text` (`bridge.go:147-150`); a photo,
document, or voice note produces an empty string and a **silent** return. The
single most phone-native input — a screenshot of an error, a photo of a
whiteboard — is dropped without even an acknowledgment, which reads as the
bot being broken. After this plan: a photo with the caption "why is this
failing?" becomes a turn where the agent can see the image, and **no message
type from an allowlisted user is ever silently dropped**.

## 2. Design

### Where files land — a media root, not the workdir

Downloads go to `<StateDir>/media/<chatID>/`, never into the session workdir
(don't dirty cloned repos with binary blobs). For the agent's Read tool to
reach them through the jail (§4.3):

- `config.Load` derives `MediaDir` from `StateDir`, canonicalizes it
  (symlink-resolved, like the roots), creates it 0700, and **appends it to
  the jail roots** so `policy.resolve` / `WithinRoots` pass.
- `agent.Run` passes a second `--add-dir` for it (`--add-dir` is already
  used for the workdir; it's variadic).

Consequence to be honest about: the media root is inside the jail, so the
agent can also *write* there. That's fine — it's bridge-owned scratch space,
not a secret store, and nothing executes from it.

### Filenames are generated bridge-side

Never trust Telegram's `Document.FileName` (path-traversal surface, and the
value is attacker-influenceable in group futures). The bridge names files
itself: `img-20260716-150405.jpg`, `doc-20260716-150405.pdf` — timestamp plus
an extension derived from the MIME type / Telegram kind. At most, the
*sanitized basename* of the original name is mentioned in the prompt text so
the agent has context.

### Prompt construction

- Caption present → caption is the prompt.
- Caption empty → default prompt: "The user sent an image with no caption —
  describe what you see and ask what they want if it's unclear."
- Either way append:
  `\n\n[Attached file saved at <path> — use the Read tool to view it.]`

Claude Code's Read tool renders images natively; no API plumbing needed.
The image content is untrusted input to the agent, but that is already true
of every text prompt; the permission gate is unchanged.

### Size cap & acknowledgment

Telegram's `getFile` caps bot downloads at 20 MB — enforce 20 MB explicitly
and reply with a clear refusal above it. The download adds latency before
the turn starts, so send the 📥 acknowledgment (or start the status card)
*before* fetching.

### Every message type gets a response

- Photo / document → download + turn (above).
- Voice / video note → "🎤 voice isn't supported yet — text or images only."
  (until PLAN-6-VOICE.md lands).
- Anything else (sticker, location, contact, poll…) → "I can only handle
  text, images, and files."
- Unlisted users stay **silently** dropped (§4.1) — the no-silent-drop rule
  applies only past the user-ID gate.

### Albums (`MediaGroupID`) — documented limitation

Telegram delivers an album as separate updates sharing a `MediaGroupID`,
caption only on the first. Phase 1 treats each as its own message (with
PLAN-1-QUEUE.md landed, photos 2..n queue behind photo 1's turn, which is
tolerable). Proper batching needs a debounce timer; not worth it yet.

### Retention

Boot-time prune of media files older than 7 days (same spirit as the card's
"disposable box" posture — the media root is cache, not archive).

## 3. Changes

- `internal/config/config.go` — `MediaDir` derived from `StateDir`;
  canonicalize, create, append to roots. Config docs in
  `deploy/bitch.env.example` (no new env var — it's derived, not
  configurable; a movable path would need its own jail reasoning).
- `internal/bridge/media.go` (new) —
  `downloadAttachment(msg) (path, note string, err)`: picks the largest
  `msg.Photo[]` entry / `msg.Document`, enforces the cap, fetches via
  `bot.GetFileDirectURL` + HTTP GET, writes 0600; plus the boot-time prune.
  The Telegram fetch goes behind an injected `getFileURL func` + `*http.Client`
  so tests can serve bytes locally.
- `internal/bridge/bridge.go` — `handleMessage`: replace the `text == ""`
  early-return with the media dispatch table above. Downloads and the turn
  run in the turn goroutine, not the poll loop.
- `internal/agent/agent.go` — `RunRequest.MediaDir string`; append
  `--add-dir` when set.
- `DESIGN.md` — §7 gains an "inbound media" paragraph; same commit.

## 4. Tests

- Prompt construction (caption / no caption / document note) — pure, table.
- Filename generation from MIME/kind — pure, table.
- Download layer against a local test server: happy path, oversize refusal,
  HTTP error.
- Policy: a media-root path passes the jail; a `../`-laundered or symlinked
  escape from the media root does not (one test per rule, per convention).

## 5. Invariants check

- **Poll loop never blocks** — downloads happen in the turn goroutine.
- **Jail compares physical paths** — media root is canonicalized in
  `config.Load` like every other root.
- **Fail closed** — download error ⇒ the turn does not start; the user gets
  the error, not a prompt referencing a missing file.
- **Child env from scratch** — untouched; the Telegram token never reaches
  the child. The download uses the token bridge-side only (as all Telegram
  I/O already does).
