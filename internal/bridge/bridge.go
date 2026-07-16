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
	"net"
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
	cfg     *config.Config
	store   *store.Store
	secrets *store.Store // the credential vault, a separate DB at cfg.SecretsPath (§4.7)
	bot     *tgbotapi.BotAPI
	sender  *tg.Sender
	runner  *agent.Runner
	botID   int64

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
	// The credential vault is a separate DB at a fixed, deny-listed path (§4.7);
	// keeping it out of the session DB is what lets one static deny term cover it.
	secrets, err := store.Open(cfg.SecretsPath)
	if err != nil {
		return nil, fmt.Errorf("bridge: open secrets vault %s: %w", cfg.SecretsPath, err)
	}
	return &Bridge{
		cfg:     cfg,
		store:   st,
		secrets: secrets,
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
	// CLI smoke check first: the stream-json shapes, hook contract, and flags
	// are version-coupled, so a missing or drifted claude binary should fail
	// the boot, not the first turn (§9.2).
	version, err := b.runner.Preflight(ctx)
	if err != nil {
		return err
	}
	log.Printf("bitch online as @%s (id %d); %d allowed user(s); tier default T%d; model %s; claude %s",
		b.bot.Self.UserName, b.botID, len(b.cfg.AllowedUsers), b.cfg.DefaultTier, b.cfg.Model, version)

	// Loopback callback server for the permission hook. Listen synchronously so
	// a bad HookAddr fails startup instead of dying inside a goroutine.
	ln, err := net.Listen("tcp", b.cfg.HookAddr)
	if err != nil {
		return fmt.Errorf("bridge: approval server listen %s: %w", b.cfg.HookAddr, err)
	}
	go b.serveApprovals(ctx, ln)

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
	// Agent turns run in their own goroutine: the poll loop must keep draining
	// updates or approval button presses and /stop would deadlock behind the
	// very turn that is waiting for them. Commands and callbacks stay
	// synchronous — they are fast. The b.runs busy-gate below keeps it to one
	// turn per chat.
	go b.handleAgentTurn(ctx, msg, key, text)
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
			b.sendReply(msg.Chat.ID, text)
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

	// Credential vault (§4.7): inject stored secrets as env vars and tell the
	// agent which vars exist so it doesn't re-ask. A vault error is non-fatal —
	// fail-closed means inject nothing and let the agent ask, not abort the turn.
	secretVals, manifest := b.vaultForTurn()

	runReq := agent.RunRequest{
		Prompt:          prompt,
		WorkDir:         sess.WorkDir,
		ClaudeSessionID: sess.ClaudeSessionID,
		Model:           sess.Model,
		Effort:          sess.Effort,
		Secrets:         secretVals,
		Manifest:        manifest,
	}
	res, err := b.runner.Run(runCtx, runReq, cb)

	// If we tried to resume a session that no longer exists — a prior failed
	// turn that never persisted, an expired session, or a rebuilt box — clear
	// it and transparently start fresh once. The failing resume exits before
	// emitting any output, so no partial reply reached the user.
	if err != nil && runReq.ClaudeSessionID != "" && runCtx.Err() == nil &&
		strings.Contains(err.Error(), "No conversation found") {
		_ = b.store.SetClaudeSession(key, "")
		runReq.ClaudeSessionID = ""
		res, err = b.runner.Run(runCtx, runReq, cb)
	}
	// A failed fresh turn never created a resumable conversation; drop the id
	// stored optimistically on init so the next turn doesn't resume a ghost.
	if err != nil && runReq.ClaudeSessionID == "" {
		_ = b.store.SetClaudeSession(key, "")
	}

	status.Delete()

	if err != nil {
		if runCtx.Err() != nil {
			b.reply(msg.Chat.ID, "🛑 stopped.")
		} else {
			log.Printf("agent turn error (chat %d): %v", msg.Chat.ID, err)
			b.reply(msg.Chat.ID, "⚠️ agent error: "+err.Error())
		}
		return
	}

	// Ensure the final answer is delivered even if it never streamed as a block.
	if !sentText && res != nil && strings.TrimSpace(res.Text) != "" {
		b.sendReply(msg.Chat.ID, res.Text)
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

// docThreshold is the reply length (bytes) above which the answer goes out as
// a document attachment instead of a stream of chunked messages (§7): past ~3
// full chunks, a file is more readable than a minute of rate-gapped sends.
const docThreshold = 12000

// sendReply delivers agent output: as chunked HTML messages normally, as a
// Markdown document attachment when it's long (§7). Failures are logged; the
// chunk path already retries once internally.
func (b *Bridge) sendReply(chatID int64, text string) {
	if len(text) > docThreshold {
		if err := b.sender.SendDocument(chatID, "reply.md", []byte(text), "📄 long reply attached"); err == nil {
			return
		}
		// fall back to chunked messages if the upload failed
	}
	if err := b.sender.SendMarkdown(chatID, text); err != nil {
		log.Printf("send reply: %v", err)
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
