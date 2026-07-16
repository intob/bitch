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
