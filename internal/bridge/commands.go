package bridge

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/intob/bitch/internal/store"
)

var tierNames = map[int]string{
	0: "T0 advisory (read-only)",
	1: "T1 constrained (edits in workspace, risky ops ask)",
	2: "T2 trusted (auto bash, risky ops ask)",
	3: "T3 full autonomy",
}

// handleCommand dispatches the /slash control commands (§6).
func (b *Bridge) handleCommand(msg *tgbotapi.Message, key store.Key, text string) {
	fields := strings.Fields(text)
	cmd := strings.ToLower(fields[0])
	// Strip a @botname suffix (groups deliver /cmd@bot).
	if i := strings.IndexByte(cmd, '@'); i >= 0 {
		cmd = cmd[:i]
	}
	arg := strings.TrimSpace(strings.TrimPrefix(text, fields[0]))

	switch cmd {
	case "/start", "/help":
		b.reply(msg.Chat.ID, helpText)
	case "/new":
		b.cmdNew(msg, key, arg)
	case "/stop":
		b.cmdStop(msg, key)
	case "/tier":
		b.cmdTier(msg, key, arg)
	case "/cd":
		b.cmdCd(msg, key, arg)
	case "/status":
		b.cmdStatus(msg, key)
	default:
		b.reply(msg.Chat.ID, "unknown command. "+helpText)
	}
}

const helpText = `bitch — commands:
/new [dir] — start a fresh session (optionally in dir)
/cd <path> — switch working directory (starts a fresh session)
/tier <0-3> — set permission tier (with confirm)
/stop — cancel the current turn
/status — session, dir, tier, today's spend
Anything else is sent to the agent.`

func (b *Bridge) cmdNew(msg *tgbotapi.Message, key store.Key, arg string) {
	sess, err := b.session(key)
	if err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
		return
	}
	dir := sess.WorkDir
	if arg != "" {
		resolved, err := b.resolveDir(sess.WorkDir, arg)
		if err != nil {
			b.reply(msg.Chat.ID, "⚠️ "+err.Error())
			return
		}
		dir = resolved
	}
	sess.WorkDir = dir
	sess.ClaudeSessionID = "" // fresh session
	if err := b.store.Upsert(sess); err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
		return
	}
	b.reply(msg.Chat.ID, "🆕 new session in "+dir)
}

func (b *Bridge) cmdCd(msg *tgbotapi.Message, key store.Key, arg string) {
	if arg == "" {
		b.reply(msg.Chat.ID, "usage: /cd <path>")
		return
	}
	sess, err := b.session(key)
	if err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
		return
	}
	dir, err := b.resolveDir(sess.WorkDir, arg)
	if err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
		return
	}
	sess.WorkDir = dir
	// Claude Code sessions are project-scoped; changing dir starts fresh.
	sess.ClaudeSessionID = ""
	if err := b.store.Upsert(sess); err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
		return
	}
	b.reply(msg.Chat.ID, "📁 "+dir+" (fresh session)")
}

func (b *Bridge) cmdStop(msg *tgbotapi.Message, key store.Key) {
	b.mu.Lock()
	run, ok := b.runs[key]
	b.mu.Unlock()
	if !ok {
		b.reply(msg.Chat.ID, "nothing running.")
		return
	}
	run.cancel()
	// The turn goroutine reports the stop; no reply here to avoid a race.
}

func (b *Bridge) cmdTier(msg *tgbotapi.Message, key store.Key, arg string) {
	sess, err := b.session(key)
	if err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
		return
	}
	if arg == "" {
		b.reply(msg.Chat.ID, "current: "+tierNames[sess.Tier]+"\nusage: /tier <0-3>")
		return
	}
	n, err := strconv.Atoi(strings.TrimSpace(arg))
	if err != nil || n < 0 || n > 3 {
		b.reply(msg.Chat.ID, "tier must be 0, 1, 2, or 3.")
		return
	}
	if n == sess.Tier {
		b.reply(msg.Chat.ID, "already at "+tierNames[n])
		return
	}
	// Confirm every tier change with inline buttons (§6). Raising autonomy is a
	// security-relevant action, so it is never a single fat-fingered keystroke.
	confirm := tgbotapi.NewMessage(msg.Chat.ID,
		"Change to <b>"+tgEscape(tierNames[n])+"</b>?")
	confirm.ParseMode = tgbotapi.ModeHTML
	confirm.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Confirm", "tier:"+strconv.Itoa(n)),
			tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "tierx"),
		),
	)
	if _, err := b.bot.Send(confirm); err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
	}
}

func (b *Bridge) cmdStatus(msg *tgbotapi.Message, key store.Key) {
	sess, err := b.session(key)
	if err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
		return
	}
	spent, _ := b.store.SpendToday()
	b.mu.Lock()
	_, busy := b.runs[key]
	b.mu.Unlock()

	sid := sess.ClaudeSessionID
	if sid == "" {
		sid = "(none yet)"
	} else if len(sid) > 8 {
		sid = sid[:8]
	}
	budget := "off"
	if b.cfg.DailyBudgetUSD > 0 {
		budget = fmt.Sprintf("$%.2f", b.cfg.DailyBudgetUSD)
	}
	status := "idle"
	if busy {
		status = "running"
	}
	b.reply(msg.Chat.ID, fmt.Sprintf(
		"status: %s\ndir: %s\ntier: %s\nmodel: %s (effort %s)\nsession: %s\nspend today: $%.2f / %s",
		status, sess.WorkDir, tierNames[sess.Tier], b.cfg.Model, b.cfg.EffortFlag(),
		sid, spent, budget))
}

// handleCallback resolves inline-button presses: approval decisions and tier
// confirmations. The user gate applies here too.
func (b *Bridge) handleCallback(cb *tgbotapi.CallbackQuery) {
	if cb.From == nil || !b.cfg.AllowedUser(cb.From.ID) {
		b.sender.AnswerCallback(cb.ID, "not allowed")
		return
	}
	data := cb.Data
	switch {
	case strings.HasPrefix(data, "ok:"), strings.HasPrefix(data, "no:"):
		b.resolveApprovalCallback(cb)
	case strings.HasPrefix(data, "tier:"):
		b.confirmTierCallback(cb, data)
	case data == "tierx":
		b.sender.AnswerCallback(cb.ID, "cancelled")
		if cb.Message != nil {
			b.sender.ResolveApproval(cb.Message.Chat.ID, cb.Message.MessageID, "tier change cancelled.")
		}
	default:
		b.sender.AnswerCallback(cb.ID, "")
	}
}

func (b *Bridge) confirmTierCallback(cb *tgbotapi.CallbackQuery, data string) {
	n, err := strconv.Atoi(strings.TrimPrefix(data, "tier:"))
	if err != nil || cb.Message == nil {
		b.sender.AnswerCallback(cb.ID, "error")
		return
	}
	key := store.Key{ChatID: cb.Message.Chat.ID, ThreadID: 0, BotID: b.botID}
	sess, err := b.session(key)
	if err != nil {
		b.sender.AnswerCallback(cb.ID, "error")
		return
	}
	sess.Tier = n
	if err := b.store.Upsert(sess); err != nil {
		b.sender.AnswerCallback(cb.ID, "error")
		return
	}
	b.sender.AnswerCallback(cb.ID, "tier set")
	b.sender.ResolveApproval(cb.Message.Chat.ID, cb.Message.MessageID, "✅ now at "+tgEscape(tierNames[n]))
}

// resolveDir resolves arg (absolute or relative to base) and enforces the
// workspace jail plus existence (§4.3).
func (b *Bridge) resolveDir(base, arg string) (string, error) {
	arg = strings.TrimSpace(arg)
	if strings.HasPrefix(arg, "~") {
		return "", fmt.Errorf("~ paths are not allowed; use an absolute path within a root")
	}
	p := arg
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, arg)
	}
	p = filepath.Clean(p)
	if !b.cfg.WithinRoots(p) {
		return "", fmt.Errorf("%s is outside the allowed roots", p)
	}
	info, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("%s: %v", p, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", p)
	}
	return p, nil
}
