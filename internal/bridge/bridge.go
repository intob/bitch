// Package bridge is the glue: it owns the Telegram I/O loop, the user-ID gate,
// control commands, the per-chat agent turns, and the loopback approval server
// the permission hook calls back into (DESIGN.md §2, §4, §6, §7).
package bridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"sync"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/intob/bitch/internal/agent"
	"github.com/intob/bitch/internal/config"
	"github.com/intob/bitch/internal/store"
	"github.com/intob/bitch/internal/tg"
)

// Bridge wires Telegram, the session store, and the Claude Code runner together.
type Bridge struct {
	cfg    *config.Config
	store  *store.Store
	bot    *tgbotapi.BotAPI
	sender *tg.Sender
	runner *agent.Runner
	botID  int64

	mu      sync.Mutex
	runs    map[store.Key]*activeRun    // in-flight agent turns, one per chat
	pending map[string]*pendingApproval // approval id → waiter
}

type activeRun struct {
	cancel context.CancelFunc
}

// New constructs a Bridge. It logs in to Telegram to learn the bot's own ID
// (part of the group-ready session key, §11) and generates a hook secret if one
// wasn't provided.
func New(cfg *config.Config, st *store.Store, exe string) (*Bridge, error) {
	if cfg.HookSecret == "" {
		cfg.HookSecret = randHex(16)
	}
	bot, err := tgbotapi.NewBotAPI(cfg.TelegramToken)
	if err != nil {
		return nil, fmt.Errorf("bridge: telegram login: %w", err)
	}
	runner, err := agent.New(cfg, exe)
	if err != nil {
		return nil, err
	}
	return &Bridge{
		cfg:     cfg,
		store:   st,
		bot:     bot,
		sender:  tg.NewSender(bot),
		runner:  runner,
		botID:   bot.Self.ID,
		runs:    make(map[store.Key]*activeRun),
		pending: make(map[string]*pendingApproval),
	}, nil
}

// Run starts the approval server and the Telegram poll loop, blocking until ctx
// is cancelled.
func (b *Bridge) Run(ctx context.Context) error {
	log.Printf("bitch online as @%s (id %d); %d allowed user(s); tier default T%d; model %s",
		b.bot.Self.UserName, b.botID, len(b.cfg.AllowedUsers), b.cfg.DefaultTier, b.cfg.Model)

	go b.serveApprovals(ctx) // loopback callback server for the permission hook

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 50 // long-poll; messages queue on Telegram during downtime (§3)
	updates := b.bot.GetUpdatesChan(u)

	for {
		select {
		case <-ctx.Done():
			b.bot.StopReceivingUpdates()
			return ctx.Err()
		case update, ok := <-updates:
			if !ok {
				return nil
			}
			b.handleUpdate(ctx, update)
		}
	}
}

func (b *Bridge) handleUpdate(ctx context.Context, update tgbotapi.Update) {
	switch {
	case update.CallbackQuery != nil:
		b.handleCallback(update.CallbackQuery)
	case update.Message != nil:
		b.handleMessage(ctx, update.Message)
	}
}

// handleMessage applies the user-ID gate then routes to a command or an agent
// turn.
func (b *Bridge) handleMessage(ctx context.Context, msg *tgbotapi.Message) {
	// Hard allowlist by numeric user ID; silently drop everyone else (§4.1).
	if msg.From == nil || !b.cfg.AllowedUser(msg.From.ID) {
		if msg.From != nil {
			log.Printf("dropped message from unlisted user %d (@%s)", msg.From.ID, msg.From.UserName)
		}
		return
	}
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return
	}
	key := b.keyFor(msg)
	if strings.HasPrefix(text, "/") {
		b.handleCommand(msg, key, text)
		return
	}
	b.handleAgentTurn(ctx, msg, key, text)
}

// keyFor builds the group-ready session key. ThreadID is 0 for 1:1 chats; forum
// topic routing arrives with a newer Telegram lib (§11).
func (b *Bridge) keyFor(msg *tgbotapi.Message) store.Key {
	return store.Key{ChatID: msg.Chat.ID, ThreadID: 0, BotID: b.botID}
}

// session loads the session for key, creating a default one on first contact.
func (b *Bridge) session(key store.Key) (*store.Session, error) {
	sess, err := b.store.Get(key)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		sess = &store.Session{Key: key, WorkDir: b.cfg.DefaultDir, Tier: b.cfg.DefaultTier}
		if err := b.store.Upsert(sess); err != nil {
			return nil, err
		}
	}
	return sess, nil
}

// handleAgentTurn runs one Claude Code turn for the chat, streaming progress to
// a status message and the answer back as chunked replies (§7).
func (b *Bridge) handleAgentTurn(ctx context.Context, msg *tgbotapi.Message, key store.Key, prompt string) {
	sess, err := b.session(key)
	if err != nil {
		b.reply(msg.Chat.ID, "⚠️ store error: "+err.Error())
		return
	}

	// One turn at a time per chat (§6). A second prompt while busy is refused.
	b.mu.Lock()
	if _, busy := b.runs[key]; busy {
		b.mu.Unlock()
		b.reply(msg.Chat.ID, "⏳ still working — send /stop to cancel.")
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	b.runs[key] = &activeRun{cancel: cancel}
	b.mu.Unlock()

	defer func() {
		cancel()
		b.mu.Lock()
		delete(b.runs, key)
		b.mu.Unlock()
	}()

	// Budget guard (§4.4): refuse new work once the daily ceiling is hit.
	if b.cfg.DailyBudgetUSD > 0 {
		if spent, err := b.store.SpendToday(); err == nil && spent >= b.cfg.DailyBudgetUSD {
			b.reply(msg.Chat.ID, fmt.Sprintf(
				"🛑 daily budget reached ($%.2f / $%.2f). New work is paused until midnight UTC.",
				spent, b.cfg.DailyBudgetUSD))
			return
		}
	}

	status, _ := b.sender.NewStatus(msg.Chat.ID, "🤔 thinking…")

	var sentText bool
	cb := agent.Callbacks{
		OnInit: func(sessionID string) {
			// Persist the Claude session id so (a) follow-ups --resume it and
			// (b) the permission hook can route approvals to this chat (§4.3).
			_ = b.store.SetClaudeSession(key, sessionID)
		},
		OnText: func(text string) {
			if strings.TrimSpace(text) == "" {
				return
			}
			sentText = true
			if err := b.sender.SendMarkdown(msg.Chat.ID, text); err != nil {
				log.Printf("send reply: %v", err)
			}
		},
		OnToolUse: func(name, summary string) {
			s := "🔧 " + name
			if summary != "" {
				s += ": " + tgEscape(summary)
			}
			status.Set(s)
		},
		OnThinking: func() { status.Set("💭 thinking…") },
	}

	res, err := b.runner.Run(runCtx, agent.RunRequest{
		Prompt:          prompt,
		WorkDir:         sess.WorkDir,
		ClaudeSessionID: sess.ClaudeSessionID,
	}, cb)

	status.Delete()

	if err != nil {
		if runCtx.Err() != nil {
			b.reply(msg.Chat.ID, "🛑 stopped.")
		} else {
			b.reply(msg.Chat.ID, "⚠️ agent error: "+err.Error())
		}
		return
	}

	// Ensure the final answer is delivered even if it never streamed as a block.
	if !sentText && res != nil && strings.TrimSpace(res.Text) != "" {
		if err := b.sender.SendMarkdown(msg.Chat.ID, res.Text); err != nil {
			log.Printf("send final: %v", err)
		}
	}
	if res != nil && res.IsError {
		b.reply(msg.Chat.ID, "⚠️ the agent turn ended with an error ("+res.Subtype+").")
	}

	// Record spend and warn on crossing the budget (§4.4).
	if res != nil && res.CostUSD > 0 {
		if total, err := b.store.AddSpend(res.CostUSD); err == nil &&
			b.cfg.DailyBudgetUSD > 0 && total >= b.cfg.DailyBudgetUSD {
			b.reply(msg.Chat.ID, fmt.Sprintf(
				"⚠️ daily budget reached: $%.2f / $%.2f.", total, b.cfg.DailyBudgetUSD))
		}
	}
}

// reply sends a plain status line (already trusted text, escaped for HTML).
func (b *Bridge) reply(chatID int64, text string) {
	if _, err := b.sender.SendHTML(chatID, tgEscape(text)); err != nil {
		log.Printf("reply: %v", err)
	}
}

func tgEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
