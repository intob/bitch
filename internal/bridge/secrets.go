package bridge

import (
	"fmt"
	"log"
	"sort"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/intob/bitch/internal/store"
)

// The credential vault UX (PLAN-CREDENTIALS.md). Storage/validation live in
// internal/store; this file is the Telegram surface: the /secrets command and
// the per-turn injection manifest.

// vaultForTurn returns the stored secrets (name→value) to inject as env vars and
// a names+descriptions manifest to prepend to the prompt. On any vault error it
// returns nils (fail-closed: inject nothing, let the agent ask) — a turn is
// never aborted over the vault. Values never appear in the manifest.
func (b *Bridge) vaultForTurn() (map[string]string, string) {
	if b.secrets == nil {
		return nil, ""
	}
	vals, err := b.secrets.AllSecrets()
	if err != nil {
		log.Printf("vault: read values: %v", err)
		return nil, ""
	}
	if len(vals) == 0 {
		return nil, ""
	}
	list, err := b.secrets.ListSecrets()
	if err != nil {
		log.Printf("vault: read listing: %v", err)
		// Still inject the values; just skip the descriptive manifest.
		return vals, genericManifest(vals)
	}
	return vals, buildManifest(list)
}

// buildManifest renders the names+descriptions preamble. Never includes values.
func buildManifest(list []store.Secret) string {
	if len(list) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Stored credentials are available to this session as environment variables ")
	sb.WriteString("(values hidden). Reference them by name; do NOT ask the user for these:\n")
	for _, s := range list {
		if s.Description != "" {
			fmt.Fprintf(&sb, "- %s: %s\n", s.Name, s.Description)
		} else {
			fmt.Fprintf(&sb, "- %s\n", s.Name)
		}
	}
	sb.WriteString("If the user gives you a new credential, tell them to store it for ")
	sb.WriteString("future tasks with: /secrets set NAME value")
	return sb.String()
}

// genericManifest is the fallback when descriptions can't be read.
func genericManifest(vals map[string]string) string {
	names := make([]string, 0, len(vals))
	for n := range vals {
		names = append(names, n)
	}
	sort.Strings(names)
	return "Stored credentials are available as environment variables (values hidden): " +
		strings.Join(names, ", ") + ". Reference them by name; do NOT ask the user for these."
}

// cmdSecrets handles /secrets, /secrets set NAME value, and /secrets rm NAME.
func (b *Bridge) cmdSecrets(msg *tgbotapi.Message, arg string) {
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		b.listSecrets(msg)
		return
	}
	switch strings.ToLower(fields[0]) {
	case "set":
		b.setSecret(msg, arg)
	case "rm", "del", "delete", "remove":
		if len(fields) < 2 {
			b.reply(msg.Chat.ID, "usage: /secrets rm NAME")
			return
		}
		b.confirmSecretRm(msg, fields[1])
	case "ls", "list":
		b.listSecrets(msg)
	default:
		b.reply(msg.Chat.ID, "usage:\n/secrets — list stored credentials\n"+
			"/secrets set NAME value — store one\n/secrets rm NAME — delete one")
	}
}

func (b *Bridge) listSecrets(msg *tgbotapi.Message) {
	list, err := b.secrets.ListSecrets()
	if err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
		return
	}
	if len(list) == 0 {
		b.reply(msg.Chat.ID, "no stored credentials. add one with /secrets set NAME value")
		return
	}
	var sb strings.Builder
	sb.WriteString("🔐 stored credentials (values hidden):\n")
	for _, s := range list {
		if s.Description != "" {
			fmt.Fprintf(&sb, "• %s — %s\n", s.Name, s.Description)
		} else {
			fmt.Fprintf(&sb, "• %s\n", s.Name)
		}
	}
	b.reply(msg.Chat.ID, strings.TrimRight(sb.String(), "\n"))
}

// setSecret parses `set NAME value…` and stores it. The value is the remainder
// of the message after NAME, so it may contain spaces. On success the original
// message is deleted to scrub the plaintext from Telegram history (§6); note
// this is best-effort and does not remove it from Telegram's servers instantly.
func (b *Bridge) setSecret(msg *tgbotapi.Message, arg string) {
	name, value := parseSecretSet(arg)
	if name == "" || value == "" {
		b.reply(msg.Chat.ID, "usage: /secrets set NAME value")
		return
	}
	if err := store.ValidSecretName(name); err != nil {
		b.reply(msg.Chat.ID, "⚠️ invalid name: "+err.Error())
		return
	}
	if err := b.secrets.SaveSecret(name, value, ""); err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
		return
	}
	// Scrub the plaintext credential from the chat (best-effort).
	if _, err := b.bot.Request(tgbotapi.NewDeleteMessage(msg.Chat.ID, msg.MessageID)); err != nil {
		log.Printf("secrets: delete plaintext message: %v", err)
	}
	b.reply(msg.Chat.ID, "🔐 stored "+name+". it'll be available to future tasks; i deleted your message with the value.")
}

// parseSecretSet splits `set NAME value…` into NAME and the verbatim value
// remainder (which may contain spaces). The leading subcommand token is peeled
// generically so it works regardless of case (the dispatch is case-insensitive).
func parseSecretSet(arg string) (name, value string) {
	_, rest, _ := strings.Cut(strings.TrimSpace(arg), " ") // drop the "set" token
	name, value, _ = strings.Cut(strings.TrimSpace(rest), " ")
	return strings.TrimSpace(name), strings.TrimSpace(value)
}

// confirmSecretRm asks before deleting — cheap insurance against a fat-fingered
// removal of a credential you'd then have to re-provision.
func (b *Bridge) confirmSecretRm(msg *tgbotapi.Message, name string) {
	confirm := tgbotapi.NewMessage(msg.Chat.ID, "Delete stored credential <b>"+tgEscape(name)+"</b>?")
	confirm.ParseMode = tgbotapi.ModeHTML
	confirm.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Delete", "secretrm:"+name),
			tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "secretrmx"),
		),
	)
	if _, err := b.bot.Send(confirm); err != nil {
		b.reply(msg.Chat.ID, "⚠️ "+err.Error())
	}
}

func (b *Bridge) confirmSecretRmCallback(cb *tgbotapi.CallbackQuery, data string) {
	name := strings.TrimPrefix(data, "secretrm:")
	if cb.Message == nil || name == "" {
		b.sender.AnswerCallback(cb.ID, "error")
		return
	}
	ok, err := b.secrets.DeleteSecret(name)
	if err != nil {
		b.sender.AnswerCallback(cb.ID, "error")
		b.sender.ResolveApproval(cb.Message.Chat.ID, cb.Message.MessageID, "⚠️ "+tgEscape(err.Error()))
		return
	}
	b.sender.AnswerCallback(cb.ID, "deleted")
	if ok {
		b.sender.ResolveApproval(cb.Message.Chat.ID, cb.Message.MessageID, "🗑️ deleted "+tgEscape(name))
	} else {
		b.sender.ResolveApproval(cb.Message.Chat.ID, cb.Message.MessageID, "no such credential: "+tgEscape(name))
	}
}
