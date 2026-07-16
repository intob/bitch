package bridge

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/intob/bitch/internal/config"
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
	case "/model":
		b.cmdModel(msg, key, arg)
	case "/effort":
		b.cmdEffort(msg, key, arg)
	case "/cd":
		b.cmdCd(msg, key, arg)
	case "/secrets", "/secret":
		b.cmdSecrets(msg, arg)
	case "/remember":
		b.cmdRemember(msg, arg)
	case "/forget":
		b.cmdForget(msg, arg)
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
/model <name> — set model (opus|sonnet|haiku|fable or a claude-… id; "default" to reset)
/effort <level> — set effort (low|medium|high|xhigh|max; "default" to reset)
/stop — cancel the current turn
/secrets — list stored credentials; /secrets set NAME value to store one, /secrets rm NAME to delete
/remember [KEY value] — recall non-secret facts (email, usernames); no args lists them
/forget KEY — drop a remembered fact
/status — session, dir, tier, model, effort, today's spend
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

// modelAliases maps friendly shorthands to canonical model ids. Full
// claude-… ids are also accepted (passed through) so new models work without a
// code change; the CLI is the final authority on validity.
var modelAliases = map[string]string{
	"opus":   "claude-opus-4-8",
	"sonnet": "claude-sonnet-5",
	"haiku":  "claude-haiku-4-5-20251001",
	"fable":  "claude-fable-5",
}

// normalizeModel resolves an alias or accepts a claude-… id, returning the
// canonical id and true, or ("", false) if it looks like neither.
func normalizeModel(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "", false
	}
	if m, ok := modelAliases[s]; ok {
		return m, true
	}
	if strings.HasPrefix(s, "claude-") {
		return s, true
	}
	return "", false
}

// cmdModel sets the per-session model override (§9.1). Unlike /tier this is not
// a security-relevant change, so it applies immediately without confirmation;
// it takes effect on the next turn (the resumed session picks up --model).
func (b *Bridge) cmdModel(msg *tgbotapi.Message, key store.Key, arg string) {
	sess, err := b.session(key)
	if err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
		return
	}
	arg = strings.TrimSpace(arg)
	if arg == "" {
		cur := sess.Model
		if cur == "" {
			cur = b.cfg.Model + " (default)"
		}
		b.reply(msg.Chat.ID, "current model: "+cur+
			"\nusage: /model <opus|sonnet|haiku|fable | claude-… id | default>")
		return
	}
	if arg == "default" || arg == "reset" {
		sess.Model = ""
		if err := b.store.Upsert(sess); err != nil {
			b.reply(msg.Chat.ID, "⚠️ "+err.Error())
			return
		}
		b.reply(msg.Chat.ID, "🧠 model reset to default ("+b.cfg.Model+"); applies to the next turn.")
		return
	}
	m, ok := normalizeModel(arg)
	if !ok {
		b.reply(msg.Chat.ID, "unknown model \""+arg+"\". use opus|sonnet|haiku|fable, a claude-… id, or default.")
		return
	}
	sess.Model = m
	if err := b.store.Upsert(sess); err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
		return
	}
	b.reply(msg.Chat.ID, "🧠 model set to "+m+"; applies to the next turn.")
}

// cmdEffort sets the per-session effort override (§9.1). Like /model, applied
// immediately, effective next turn.
func (b *Bridge) cmdEffort(msg *tgbotapi.Message, key store.Key, arg string) {
	sess, err := b.session(key)
	if err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
		return
	}
	arg = strings.ToLower(strings.TrimSpace(arg))
	if arg == "" {
		cur := sess.Effort
		if cur == "" {
			cur = b.cfg.Effort + " (default)"
		}
		b.reply(msg.Chat.ID, "current effort: "+cur+"\nusage: /effort <low|medium|high|xhigh|max | default>")
		return
	}
	if arg == "default" || arg == "reset" {
		sess.Effort = ""
		if err := b.store.Upsert(sess); err != nil {
			b.reply(msg.Chat.ID, "⚠️ "+err.Error())
			return
		}
		b.reply(msg.Chat.ID, "⚙️ effort reset to default ("+b.cfg.Effort+"); applies to the next turn.")
		return
	}
	if config.NormalizeEffort(arg) == "" {
		b.reply(msg.Chat.ID, "effort must be one of: low, medium, high, xhigh, max (or default).")
		return
	}
	sess.Effort = arg
	if err := b.store.Upsert(sess); err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
		return
	}
	b.reply(msg.Chat.ID, "⚙️ effort set to "+arg+"; applies to the next turn.")
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
	// Show the effective model/effort, flagging when it's the config default.
	model := sess.Model
	if model == "" {
		model = b.cfg.Model + " (default)"
	}
	effort := sess.Effort
	if effort == "" {
		effort = b.cfg.Effort + " (default)"
	}
	b.reply(msg.Chat.ID, fmt.Sprintf(
		"status: %s\ndir: %s\ntier: %s\nmodel: %s\neffort: %s\nsession: %s\nspend today: $%.2f / %s",
		status, sess.WorkDir, tierNames[sess.Tier], model, effort,
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
	case strings.HasPrefix(data, "secretrm:"):
		b.confirmSecretRmCallback(cb, data)
	case data == "secretrmx":
		b.sender.AnswerCallback(cb.ID, "cancelled")
		if cb.Message != nil {
			b.sender.ResolveApproval(cb.Message.Chat.ID, cb.Message.MessageID, "deletion cancelled.")
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
	// Resolve symlinks before the jail check so a link inside a root pointing
	// outside it can't launder the target (§4.3).
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
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
