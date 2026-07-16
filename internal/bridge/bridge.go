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
	"runtime/debug"
	"strings"
	"sync"
	"time"

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
	media   *media // inbound attachment downloads (§7)
	botID   int64

	mu      sync.Mutex
	runs    map[store.Key]*activeRun    // in-flight agent turns, one per chat
	pending map[string]*pendingApproval // approval id → waiter
}

// activeRun is the turn slot for one chat: the cancel for the turn currently
// running, plus prompts that arrived while it ran and will be drained into the
// next turn (§6). Both fields are guarded by Bridge.mu.
type activeRun struct {
	cancel     context.CancelFunc
	queue      []string
	queueBytes int
}

// Queue bounds. Unbounded growth would be a memory hole on a long turn, and
// past this many pending thoughts the user is fighting the tool anyway —
// overflow falls back to the old "still working" refusal.
const (
	maxQueuedPrompts = 10
	maxQueuedBytes   = 8 << 10
)

// claimResult is what claimOrQueue did with a prompt.
type claimResult int

const (
	turnClaimed  claimResult = iota // caller owns the chat's turn slot
	turnQueued                      // appended to the running turn's queue
	turnRejected                    // busy and the queue is at its bound
)

// claimOrQueue is the one-turn-per-chat gate (§6). If nothing owns key it
// installs a run entry (cancel becomes the running turn's) and returns
// turnClaimed; otherwise the prompt joins the owner's queue. Gate and append
// share one critical section so a prompt can never slip between them.
func (b *Bridge) claimOrQueue(key store.Key, prompt string, cancel context.CancelFunc) claimResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	run, busy := b.runs[key]
	if !busy {
		b.runs[key] = &activeRun{cancel: cancel}
		return turnClaimed
	}
	if len(run.queue) >= maxQueuedPrompts || run.queueBytes+len(prompt) > maxQueuedBytes {
		return turnRejected
	}
	run.queue = append(run.queue, prompt)
	run.queueBytes += len(prompt)
	return turnQueued
}

// nextTurn ends one iteration of the drain loop. It pops the whole queue and
// installs a fresh cancel for the turn about to start, or — when the queue is
// empty — releases the chat's turn slot and returns false.
//
// Pop-or-release is a single critical section on purpose: the b.runs entry must
// be held continuously from the first turn to the final drain, or a prompt
// landing in the gap would start a rival goroutine for the same chat.
func (b *Bridge) nextTurn(ctx context.Context, key store.Key) (string, context.Context, context.CancelFunc, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	run, ok := b.runs[key]
	if !ok || len(run.queue) == 0 {
		delete(b.runs, key)
		return "", nil, nil, false
	}
	prompt := strings.Join(run.queue, "\n\n")
	run.queue, run.queueBytes = nil, 0
	runCtx, cancel := context.WithCancel(ctx)
	run.cancel = cancel
	return prompt, runCtx, cancel, true
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
		media:   newMedia(bot, cfg.MediaDir),
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
	rev := buildRevision()
	log.Printf("bitch %s online as @%s (id %d); %d allowed user(s); tier default T%d; model %s; claude %s",
		rev, b.bot.Self.UserName, b.botID, len(b.cfg.AllowedUsers), b.cfg.DefaultTier, b.cfg.Model, version)

	// Loopback callback server for the permission hook. Listen synchronously so
	// a bad HookAddr fails startup instead of dying inside a goroutine.
	ln, err := net.Listen("tcp", b.cfg.HookAddr)
	if err != nil {
		return fmt.Errorf("bridge: approval server listen %s: %w", b.cfg.HookAddr, err)
	}
	go b.serveApprovals(ctx, ln)

	// The media root is cache, not archive (§7). Best-effort — housekeeping must
	// never fail a boot.
	if n, err := pruneMedia(b.cfg.MediaDir, mediaRetention); err != nil {
		log.Printf("media prune: %v", err)
	} else if n > 0 {
		log.Printf("media prune: removed %d file(s) older than %s", n, mediaRetention)
	}

	// Boot notice to the human (§8): confirms a deploy (or crash restart —
	// systemd restarts land here too, which is a signal worth having) came back
	// up, and pins the running commit. For 1:1 chats the chat id equals the
	// user id, so the allowlist doubles as the recipient list. Best-effort:
	// a failed send must never block the boot.
	for _, uid := range b.cfg.AllowedUsers {
		b.reply(uid, fmt.Sprintf("🟢 online — bitch %s · claude %s · %s", rev, version, b.cfg.Model))
	}

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
	key := b.keyFor(msg)
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		// Not text: an attachment becomes a turn, anything else gets an honest
		// refusal. Past the user-ID gate nothing is dropped in silence — a
		// screenshot that vanishes reads as the bot being broken (§7).
		b.handleNonText(ctx, msg, key)
		return
	}
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

// handleNonText routes a message with no text body (§7). Photos and documents
// download and become a turn; the rest gets told why it can't. Service messages
// (joins, pins) fall through the default arm — harmless noise in a 1:1 chat, and
// the alternative is enumerating a moving target of message types.
func (b *Bridge) handleNonText(ctx context.Context, msg *tgbotapi.Message, key store.Key) {
	switch {
	case len(msg.Photo) > 0 || msg.Document != nil:
		// The download runs in the turn goroutine, not the poll loop: it's a
		// network fetch of up to 20 MB and the loop must keep draining updates.
		go b.handleMediaTurn(ctx, msg, key)
	case msg.Voice != nil || msg.VideoNote != nil || msg.Audio != nil:
		b.reply(msg.Chat.ID, "🎤 voice isn't supported yet — text, images, or files.")
	default:
		b.reply(msg.Chat.ID, "I can only handle text, images, and files.")
	}
}

// handleMediaTurn downloads the attachment and runs the turn it becomes. The
// acknowledgment goes out before the fetch: the download is latency the user
// would otherwise read as silence.
func (b *Bridge) handleMediaTurn(ctx context.Context, msg *tgbotapi.Message, key store.Key) {
	chatID := msg.Chat.ID
	b.reply(chatID, "📥 downloading…")
	prompt, err := b.media.fetch(chatID, msg)
	if err != nil {
		// Fail closed: no turn starts, so the agent never gets a prompt pointing
		// at a file that isn't there.
		log.Printf("media download (chat %d): %v", chatID, err)
		b.reply(chatID, "⚠️ couldn't fetch that file: "+err.Error())
		return
	}
	b.handleAgentTurn(ctx, msg, key, prompt)
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

// handleAgentTurn owns the chat's turn slot for as long as there is work: it
// runs the prompt, then drains anything that arrived while it ran into a
// follow-up turn, until the queue comes up empty (§6).
func (b *Bridge) handleAgentTurn(ctx context.Context, msg *tgbotapi.Message, key store.Key, prompt string) {
	chatID := msg.Chat.ID
	runCtx, cancel := context.WithCancel(ctx)
	switch b.claimOrQueue(key, prompt, cancel) {
	case turnQueued:
		cancel()
		b.reply(chatID, "📥 queued — runs after the current turn (/stop cancels both).")
		return
	case turnRejected:
		cancel()
		b.reply(chatID, "⏳ still working — send /stop to cancel.")
		return
	}

	// Backstop: nextTurn releases the slot on the normal path, but a panic in a
	// turn must not leave the chat wedged as permanently busy.
	defer func() {
		b.mu.Lock()
		if run, ok := b.runs[key]; ok {
			run.cancel()
			delete(b.runs, key)
		}
		b.mu.Unlock()
	}()

	for {
		b.runTurn(runCtx, key, chatID, prompt)
		cancel()
		var more bool
		prompt, runCtx, cancel, more = b.nextTurn(ctx, key)
		if !more {
			return
		}
	}
}

// runTurn runs one Claude Code turn for the chat, streaming progress to a status
// message and the answer back as chunked replies (§7). The session is re-read
// per turn so a drained follow-up resumes the id the previous turn persisted.
func (b *Bridge) runTurn(runCtx context.Context, key store.Key, chatID int64, prompt string) {
	sess, err := b.session(key)
	if err != nil {
		b.reply(chatID, "⚠️ store error: "+err.Error())
		return
	}

	// Budget guard (§4.4): refuse new work once the daily ceiling is hit.
	if b.cfg.DailyBudgetUSD > 0 {
		if spent, err := b.store.SpendToday(); err == nil && spent >= b.cfg.DailyBudgetUSD {
			b.reply(chatID, fmt.Sprintf(
				"🛑 daily budget reached ($%.2f / $%.2f). New work is paused until midnight UTC.",
				spent, b.cfg.DailyBudgetUSD))
			return
		}
	}

	status, _ := b.sender.NewStatus(chatID, stateThinking)

	// The live activity card (§7): per-tool lines, elapsed time, tokens, and a
	// running cost estimate. Model resolution mirrors agent.Run so the price
	// table sees the id the CLI will actually bill.
	model := sess.Model
	if model == "" {
		model = b.cfg.Model
	}
	card := newTurnCard(model)

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
			b.sendReply(chatID, text)
		},
		OnToolUse: func(id, name, summary string) {
			card.StartTool(id, name, summary)
			status.Set(card.Render())
		},
		OnToolResult: func(id string, isError bool) {
			card.FinishTool(id, isError)
			status.Set(card.Render())
		},
		OnUsage: func(u agent.Usage) {
			card.AddUsage(u)
			status.Set(card.Render())
		},
		OnThinking: func() {
			card.SetState(stateThinking)
			status.Set(card.Render())
		},
	}

	// Keep the card's clock and cost ticking during long tool runs: re-render
	// on a timer even when no stream events arrive. Status throttles the
	// actual Telegram edits, so this costs at most one edit per editGap.
	tickDone := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-tickDone:
				return
			case <-t.C:
				status.Set(card.Render())
			}
		}
	}()

	// Recall preamble: non-secret facts (values inline) plus the credential-vault
	// manifest (secret names only; values go into the env). Both let the agent
	// reuse things the user provided once instead of re-asking. Vault/fact read
	// errors are non-fatal — worst case the agent asks; a turn is never aborted.
	secretVals, secretsManifest := b.vaultForTurn()
	// captureNudge is always present (gate-independent): the MCP capture tools may
	// be deferred behind ToolSearch, so the agent needs to be told they exist (§4.7).
	manifest := joinPreamble(captureNudge, b.factsBlock(), secretsManifest)

	runReq := agent.RunRequest{
		Prompt:          prompt,
		WorkDir:         sess.WorkDir,
		ClaudeSessionID: sess.ClaudeSessionID,
		MediaDir:        b.cfg.MediaDir,
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

	close(tickDone)

	// Finalize the card in place instead of deleting it — the resolved feed
	// plus final time/tokens/cost is the turn's activity record (§7). The
	// result event's cost is authoritative; the estimate stands in on failure.
	switch {
	case err != nil && runCtx.Err() != nil:
		status.Finish(card.RenderFinal("🛑 stopped", nil))
	case err != nil:
		status.Finish(card.RenderFinal("⚠️ failed", nil))
	default:
		status.Finish(card.RenderFinal("✅ done", res))
	}

	if err != nil {
		if runCtx.Err() != nil {
			b.reply(chatID, "🛑 stopped.")
		} else {
			log.Printf("agent turn error (chat %d): %v", chatID, err)
			b.reply(chatID, "⚠️ agent error: "+err.Error())
		}
		return
	}

	// Ensure the final answer is delivered even if it never streamed as a block.
	if !sentText && res != nil && strings.TrimSpace(res.Text) != "" {
		b.sendReply(chatID, res.Text)
	}
	if res != nil && res.IsError {
		b.reply(chatID, "⚠️ the agent turn ended with an error ("+res.Subtype+").")
	}

	// Record spend and warn on crossing the budget (§4.4).
	if res != nil && res.CostUSD > 0 {
		if total, err := b.store.AddSpend(res.CostUSD); err == nil &&
			b.cfg.DailyBudgetUSD > 0 && total >= b.cfg.DailyBudgetUSD {
			b.reply(chatID, fmt.Sprintf(
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

// buildRevision returns the short git commit the binary was built from, via
// the Go toolchain's automatic VCS stamping — no ldflags plumbing in the build
// scripts to keep in sync. "+dirty" marks a build from a modified tree;
// "unknown" means the build had no VCS info (e.g. `go run` from a tarball).
func buildRevision() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	rev, modified := "", false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if rev == "" {
		return "unknown"
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if modified {
		rev += "+dirty"
	}
	return rev
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
