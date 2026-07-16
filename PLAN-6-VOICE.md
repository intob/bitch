# PLAN 6 — voice input (transcription)

Status: **implemented** (2026-07-16). Build order was **6 of 6 — decided:
wanted, but last** (user, 2026-07-16). Depended on PLAN-2-MEDIA-IN.md (the
download plumbing and the no-silent-drop dispatch). Authority for intent is
`DESIGN.md` §4/§7; read alongside `CLAUDE.md` (invariants). This doc is kept as
the rationale record — the code, not this file, is now the reference.

Built as designed, with two things the plan didn't say: the transcript echo is
truncated for display (`voiceEchoLimit`, since `reply` doesn't chunk and a long
ramble would blow Telegram's message limit — the agent still gets all of it),
and `config` refuses a plain-http endpoint unless it's loopback rather than
accepting any URL.

## 1. The ask

Voice is how people actually talk to a phone while walking. Today a voice
note is silently dropped; after PLAN-2-MEDIA-IN.md it gets a polite decline.
This plan makes it a real input: speak a prompt, the agent acts on the
transcript.

## 2. Why it's last

Claude can't read audio, so this is the one inbound type that needs a **new
external dependency**: a transcription service and a key for it. That means
a provider decision, a new secret in the env schema, and a new egress
destination on a box whose firewall posture is deliberately tight
(DEPLOY.md). Everything else in plans 1–5 works with what's already on the
box.

## 3. Design

### Provider: OpenAI-compatible transcription endpoint, configurable

Target the de-facto standard `POST /v1/audio/transcriptions`
(multipart: `file`, `model`) — served by OpenAI Whisper, Groq, and most
self-hosted gateways (speaches, LocalAI). Two env vars in `config.Load`:

- `BITCH_TRANSCRIBE_URL` — full endpoint URL. Unset ⇒ feature off.
- `BITCH_TRANSCRIBE_KEY` — bearer token; may be empty for keyless
  self-hosted endpoints, but `URL` set + key expected is the provider's
  problem, not ours (a 401 surfaces as a normal failure).
- Optional `BITCH_TRANSCRIBE_MODEL` (default `whisper-1`).

**Fail closed:** unset URL keeps the PLAN-2 decline message. A configured
but failing endpoint replies "⚠️ transcription failed: …" — the turn does
not start on garbage, and the voice note is **not** guessed at.

Local whisper.cpp on the CAX21 was considered and rejected for v1: ARM
builds and model management on a disposable box are more moving parts than
one HTTPS call, and the box already trusts a remote model with the whole
transcript anyway (the prompt goes to Anthropic).

### Flow

1. `handleMessage` voice/video-note arm (replacing the PLAN-2 decline):
   download the `.oga` via the PLAN-2 media plumbing (voice notes are
   small; same 20 MB cap applies).
2. POST to the transcription endpoint bridge-side (the key lives in bridge
   env only — like the Telegram token, it must never enter `childEnv`;
   nothing new needed, the scratch-built env already guarantees it).
3. On success: run a normal turn with
   `(transcribed voice message) <transcript>` as the prompt, and echo the
   transcript back to the chat first ("🎤 → "…"") so the user can catch a
   bad transcription and `/stop` before the agent runs far with it.
4. Delete the audio file after transcription (nothing reads it later; the
   7-day prune is the backstop).

### Privacy note (DESIGN.md, be honest)

Voice audio leaves the box to a third party the user configures. That is a
posture change worth one sentence in DESIGN.md §4 and one in the env
example file — the user chooses the provider, including a self-hosted one.

## 4. Changes

- `internal/config/config.go` — the three env vars; validation: `URL` must
  be https (or localhost) if set. Secrets stay env-only per convention
  (`config.Load` is the single `os.Getenv` reader).
- `internal/bridge/transcribe.go` (new) — small client for the multipart
  call, injected `*http.Client` for tests; timeout ~60 s.
- `internal/bridge/bridge.go` / `media.go` — voice arm switches from
  decline to transcribe-and-run when configured.
- `deploy/bitch.env.example` + `DEPLOY.md` env schema + egress note (the
  Hetzner firewall already allows 443 egress; no rule change, but document
  the new destination).
- `DESIGN.md` — §4 privacy sentence, §7 inbound-voice paragraph; same
  commit.

## 5. Tests

- Config: off when unset; https validation; key optional.
- Transcribe client against a local test server: happy path, 401, timeout,
  malformed JSON — each fails the turn cleanly (no turn started).
- Dispatch: voice with feature off → decline; on → transcript echo + turn
  prompt shape (table-driven with a stubbed transcriber).

## 6. Invariants check

- **Fail closed** — unset ⇒ off; any transcription error ⇒ no turn.
- **Child env from scratch** — `BITCH_TRANSCRIBE_KEY` is bridge-only; the
  existing `childEnv` allowlist already excludes it (add the test assert
  alongside the `BITCH_TELEGRAM_TOKEN` one).
- **Secrets come from the environment** — new key follows the `config.Load`
  convention; nothing else reads `os.Getenv`.
