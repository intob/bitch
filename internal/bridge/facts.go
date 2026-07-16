package bridge

import (
	"fmt"
	"log"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/intob/bitch/internal/store"
)

// Facts are non-secret recall values (email, usernames, preferences) the user
// provides once so a later task doesn't re-ask. Unlike the secret vault, values
// are injected as plaintext into the prompt preamble and shown back on request.

// factsBlock renders the facts preamble prepended to each turn's prompt, or ""
// if there are none. Values are included on purpose — facts are non-secret. On a
// read error it returns "" (fail-open is harmless here: worst case the agent
// asks for something it could have recalled).
func (b *Bridge) factsBlock() string {
	list, err := b.store.ListFacts()
	if err != nil {
		log.Printf("facts: read: %v", err)
		return ""
	}
	if len(list) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Known facts the user has told you (use them; do NOT re-ask). ")
	sb.WriteString("They can update these with /remember and view them with /forget:\n")
	for _, f := range list {
		fmt.Fprintf(&sb, "- %s: %s\n", f.Key, f.Value)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// cmdRemember handles /remember (list), /remember KEY value (store).
func (b *Bridge) cmdRemember(msg *tgbotapi.Message, arg string) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		b.listFacts(msg)
		return
	}
	key, value := parseKeyValue(arg)
	if key == "" || value == "" {
		b.reply(msg.Chat.ID, "usage: /remember KEY value   (e.g. /remember email joey@rmrf.uk)")
		return
	}
	if err := store.ValidFactKey(key); err != nil {
		b.reply(msg.Chat.ID, "⚠️ invalid key: "+err.Error())
		return
	}
	if err := b.store.SaveFact(key, value); err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
		return
	}
	b.reply(msg.Chat.ID, "🧠 remembered "+key+". i'll use it in future tasks without asking.")
}

// cmdForget handles /forget (list, so it doubles as a viewer) and /forget KEY.
func (b *Bridge) cmdForget(msg *tgbotapi.Message, arg string) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		b.listFacts(msg)
		return
	}
	key, _ := parseKeyValue(arg) // ignore anything after the key
	ok, err := b.store.DeleteFact(key)
	if err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
		return
	}
	if !ok {
		b.reply(msg.Chat.ID, "nothing stored under "+key+".")
		return
	}
	b.reply(msg.Chat.ID, "🗑️ forgot "+key+".")
}

func (b *Bridge) listFacts(msg *tgbotapi.Message) {
	list, err := b.store.ListFacts()
	if err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
		return
	}
	if len(list) == 0 {
		b.reply(msg.Chat.ID, "nothing remembered yet. add a fact with /remember KEY value")
		return
	}
	var sb strings.Builder
	sb.WriteString("🧠 remembered facts:\n")
	for _, f := range list {
		fmt.Fprintf(&sb, "• %s: %s\n", f.Key, f.Value)
	}
	sb.WriteString("\n/remember KEY value to add/update, /forget KEY to remove.")
	b.reply(msg.Chat.ID, strings.TrimRight(sb.String(), "\n"))
}

// parseKeyValue splits "KEY value…" into the first token and the verbatim
// remainder (which may contain spaces).
func parseKeyValue(arg string) (key, value string) {
	key, value, _ = strings.Cut(strings.TrimSpace(arg), " ")
	return strings.TrimSpace(key), strings.TrimSpace(value)
}

// captureNudge is a standing preamble line that tells the agent the bitch MCP
// tools exist and to reach for them. Recent Claude Code hides MCP tools behind a
// ToolSearch stub (the server-side tengu_deferred_stub_tool gate) rather than
// loading them into the model's tool list, so the agent no longer sees
// save_secret / remember_fact / send_file upfront and won't reach for them on its
// own. The gate is server-controlled and can't be pinned in setup.sh, so we make
// these gate-independent from our side: name the tools in the prompt and note
// they may need a ToolSearch first (§4.7). The PreToolUse gate still confirms
// each call over Telegram before it runs, so the agent should just call the
// tool, not ask.
//
// This is prepended to every turn (~140 words on the hot path). Future: scope it
// so it isn't paid every turn — see CLAUDE.md "Known limitations / deferred".
const captureNudge = "You have three tools from the \"bitch\" MCP server. Two are for durable recall so " +
	"future tasks don't re-ask: remember_fact (non-secret details — email, usernames, handles, " +
	"preferences) and save_secret (credentials — API keys, tokens, passwords; the value is " +
	"stored as an env var and never shown back). The third is send_file (deliver a file from the " +
	"workspace to the user in Telegram — a rendered image, screenshot, or built artifact; images " +
	"preview inline). These may not appear in your tool list directly; if you don't see them, use " +
	"ToolSearch to load them (search for the tool name). When the user gives you a durable personal " +
	"detail or a credential, or you produce a file they should see, call the matching tool — don't " +
	"ask first; you'll be prompted to confirm before it runs."

// joinPreamble joins the non-empty preamble blocks with a blank line between,
// so an empty facts or secrets section doesn't leave stray whitespace.
func joinPreamble(blocks ...string) string {
	nonEmpty := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if s := strings.TrimSpace(b); s != "" {
			nonEmpty = append(nonEmpty, s)
		}
	}
	return strings.Join(nonEmpty, "\n\n")
}
