package bridge

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/intob/bitch/internal/store"
)

// The /sessions and /resume surface (§6, PLAN-4-SESSION-HISTORY.md). Recording
// happens in runTurn; storage in internal/store/history.go.

// titleLen is how much of a session's first prompt is kept as its label. Long
// enough to recognise yesterday's thread, short enough to list on a phone.
const titleLen = 64

// sessionTitle labels a session by its first prompt: the first non-empty line,
// truncated rune-safe (a prompt can open with anything, including multibyte).
func sessionTitle(prompt string) string {
	for _, line := range strings.Split(prompt, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return truncateRunes(line, titleLen)
		}
	}
	return "(untitled)"
}

// humanAge renders a rough "how long ago" for the listing. Approximate on
// purpose — the point is picking the right session out of a list, not audit.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// cmdSessions lists the chat's past Claude sessions, most recent first. The
// listing is keyed, so a job's sessions (their own synthetic key, §6.1) never
// appear here — a job's continuity is managed by the job, not the human.
func (b *Bridge) cmdSessions(msg *tgbotapi.Message, key store.Key) {
	sess, err := b.session(key)
	if err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
		return
	}
	recs, err := b.store.ListSessions(key, 0)
	if err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
		return
	}
	if len(recs) == 0 {
		b.reply(msg.Chat.ID, "no past sessions yet — this chat's history starts with its first turn.")
		return
	}
	var sb strings.Builder
	sb.WriteString("🗂 sessions (newest first) — /resume <n> to switch:\n")
	now := time.Now()
	for i, rec := range recs {
		marker := ""
		if rec.ClaudeSessionID == sess.ClaudeSessionID {
			marker = "  ← current"
		}
		fmt.Fprintf(&sb, "\n%d. [%s] %s%s\n%s\n",
			i+1, humanAge(now.Sub(rec.LastUsed)), rec.Title, marker, rec.WorkDir)
	}
	b.reply(msg.Chat.ID, strings.TrimRight(sb.String(), "\n"))
}

// cmdResume switches the chat back to a past session.
func (b *Bridge) cmdResume(msg *tgbotapi.Message, key store.Key, arg string) {
	rec, err := b.resumeSession(key, arg)
	if err != nil {
		b.reply(msg.Chat.ID, err.Error())
		return
	}
	b.reply(msg.Chat.ID, fmt.Sprintf("↩️ resumed: %s\ndir: %s", rec.Title, rec.WorkDir))
}

// resumeSession is the decision half of /resume, split out so it's testable
// without a Telegram sender. It restores the session's workdir along with its
// id: Claude Code sessions are project-scoped (the same fact that makes /cd
// start fresh), so resuming without the dir would hand --resume a session
// belonging to another project. The returned record carries the re-jailed dir.
// Errors are user-facing text.
func (b *Bridge) resumeSession(key store.Key, arg string) (*store.SessionRecord, error) {
	n, err := strconv.Atoi(strings.TrimSpace(arg))
	if err != nil || n < 1 {
		return nil, fmt.Errorf("usage: /resume <n> — see /sessions for the numbers")
	}
	// Refuse mid-turn: the session id is what routes approvals back to this chat
	// (§4.3), so swapping it under a live turn would corrupt that routing. No
	// turn can start while this runs — commands are synchronous on the poll loop
	// that spawns them.
	b.mu.Lock()
	_, busy := b.runs[key]
	b.mu.Unlock()
	if busy {
		return nil, fmt.Errorf("⏳ a turn is running — finish or /stop first")
	}

	sess, err := b.session(key)
	if err != nil {
		return nil, fmt.Errorf("⚠️ %w", err)
	}
	recs, err := b.store.ListSessions(key, 0)
	if err != nil {
		return nil, fmt.Errorf("⚠️ %w", err)
	}
	if n > len(recs) {
		return nil, fmt.Errorf("no session %d — there are %d. see /sessions", n, len(recs))
	}
	rec := recs[n-1]
	// Re-jail the stored dir: the roots may have changed since the session ran,
	// and a resume must not be a way back into a directory that's no longer
	// allowed. Fail closed — refuse rather than resume somewhere else.
	dir, err := b.resolveDir(sess.WorkDir, rec.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("⚠️ can't resume there: %w", err)
	}
	sess.WorkDir = dir
	sess.ClaudeSessionID = rec.ClaudeSessionID
	if err := b.store.Upsert(sess); err != nil {
		return nil, fmt.Errorf("⚠️ %w", err)
	}
	rec.WorkDir = dir
	return &rec, nil
}
