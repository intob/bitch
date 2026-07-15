package tg

import (
	"fmt"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Sender wraps the Telegram Bot API with the send patterns bitch needs:
// HTML-formatted chunked replies, a single edited status message, inline
// approval keyboards, and document attachments for big artifacts (§7).
type Sender struct {
	bot *tgbotapi.BotAPI
}

// NewSender wraps a bot API handle.
func NewSender(bot *tgbotapi.BotAPI) *Sender { return &Sender{bot: bot} }

// rateGap respects Telegram's ~1 message/sec/chat limit (§7).
const rateGap = 1100 * time.Millisecond

// SendHTML sends one HTML message and returns its message ID.
func (s *Sender) SendHTML(chatID int64, html string) (int, error) {
	msg := tgbotapi.NewMessage(chatID, html)
	msg.ParseMode = tgbotapi.ModeHTML
	msg.DisableWebPagePreview = true
	sent, err := s.bot.Send(msg)
	if err != nil {
		return 0, err
	}
	return sent.MessageID, nil
}

// SendMarkdown renders Markdown to HTML, chunks it, and sends each part. A
// failed chunk is retried once after a rate-gap pause (transient 429s must not
// silently eat the final answer), then falls back to plain text (a parse error
// will fail identically on retry).
func (s *Sender) SendMarkdown(chatID int64, md string) error {
	chunks := Chunk(md)
	for i, c := range chunks {
		if c == "" {
			continue
		}
		if _, err := s.SendHTML(chatID, c); err != nil {
			time.Sleep(rateGap)
			if _, err = s.SendHTML(chatID, c); err != nil {
				if _, perr := s.sendPlain(chatID, stripTags(c)); perr != nil {
					return fmt.Errorf("send chunk %d: %w", i, err)
				}
			}
		}
		if i < len(chunks)-1 {
			time.Sleep(rateGap)
		}
	}
	return nil
}

func (s *Sender) sendPlain(chatID int64, text string) (int, error) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.DisableWebPagePreview = true
	sent, err := s.bot.Send(msg)
	if err != nil {
		return 0, err
	}
	return sent.MessageID, nil
}

// SendDocument uploads bytes as a file attachment (long diffs, logs, §7).
func (s *Sender) SendDocument(chatID int64, filename string, data []byte, caption string) error {
	doc := tgbotapi.NewDocument(chatID, tgbotapi.FileBytes{Name: filename, Bytes: data})
	if caption != "" {
		doc.Caption = caption
	}
	_, err := s.bot.Send(doc)
	return err
}

// --- status message ------------------------------------------------------

// Status is a single Telegram message that gets edited in place to reflect
// progress ("🔧 running tests…"), instead of spamming new messages (§7). Set
// self-throttles to one edit per editGap: a tool-heavy turn fires updates far
// faster than Telegram's edit rate limit, and the library does not retry 429s.
type Status struct {
	s      *Sender
	chatID int64
	msgID  int

	mu       sync.Mutex
	last     string // last text actually sent
	want     string // latest desired text
	lastEdit time.Time
	timer    *time.Timer // pending deferred flush, nil if none
}

// editGap is the minimum interval between edits of the status message.
const editGap = 1500 * time.Millisecond

// NewStatus posts the initial status message.
func (s *Sender) NewStatus(chatID int64, initialHTML string) (*Status, error) {
	id, err := s.SendHTML(chatID, initialHTML)
	if err != nil {
		return nil, err
	}
	return &Status{s: s, chatID: chatID, msgID: id, last: initialHTML, lastEdit: time.Now()}, nil
}

// Set records the desired status text and edits the message, deferring the
// edit if one happened within editGap. Only the newest text survives a defer.
func (st *Status) Set(html string) {
	if st == nil || html == "" {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.want = html
	if st.timer != nil {
		return // a flush is already scheduled; it will pick up want
	}
	if since := time.Since(st.lastEdit); since < editGap {
		st.timer = time.AfterFunc(editGap-since, st.flush)
		return
	}
	st.edit(html)
}

func (st *Status) flush() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.timer = nil
	st.edit(st.want)
}

// edit performs the API call. Callers must hold st.mu.
func (st *Status) edit(html string) {
	if html == st.last || html == "" {
		return
	}
	edit := tgbotapi.NewEditMessageText(st.chatID, st.msgID, html)
	edit.ParseMode = tgbotapi.ModeHTML
	edit.DisableWebPagePreview = true
	if _, err := st.s.bot.Send(edit); err == nil {
		st.last = html
	}
	st.lastEdit = time.Now()
}

// Delete removes the status message once the turn is done.
func (st *Status) Delete() {
	if st == nil {
		return
	}
	st.mu.Lock()
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
	st.mu.Unlock()
	_, _ = st.s.bot.Request(tgbotapi.NewDeleteMessage(st.chatID, st.msgID))
}

// --- approvals -----------------------------------------------------------

// SendApproval posts an approval card with ✅ / ❌ inline buttons (§4.3). The
// callback data encodes the pending request id so the bridge can resolve it.
func (s *Sender) SendApproval(chatID int64, reqID, action, detail string) (int, error) {
	body := "⚠️ <b>Approval needed</b>\n" + escapeHTML(action)
	if detail != "" && detail != action {
		body += "\n<pre>" + escapeHTML(truncate(detail, 1500)) + "</pre>"
	}
	msg := tgbotapi.NewMessage(chatID, body)
	msg.ParseMode = tgbotapi.ModeHTML
	msg.DisableWebPagePreview = true
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Allow", "ok:"+reqID),
			tgbotapi.NewInlineKeyboardButtonData("❌ Deny", "no:"+reqID),
		),
	)
	sent, err := s.bot.Send(msg)
	if err != nil {
		return 0, err
	}
	return sent.MessageID, nil
}

// ResolveApproval rewrites the approval card to show the outcome and removes the
// buttons so it can't be answered twice.
func (s *Sender) ResolveApproval(chatID int64, msgID int, outcome string) {
	edit := tgbotapi.NewEditMessageText(chatID, msgID, outcome)
	edit.ParseMode = tgbotapi.ModeHTML
	edit.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}}
	_, _ = s.bot.Send(edit)
}

// AnswerCallback acknowledges a button press so Telegram stops the spinner.
func (s *Sender) AnswerCallback(callbackID, text string) {
	_, _ = s.bot.Request(tgbotapi.NewCallback(callbackID, text))
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// stripTags is a crude fallback that removes angle-bracket tags for plain-text
// resend when HTML parsing fails.
func stripTags(s string) string {
	var b []byte
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b = append(b, s[i])
			}
		}
	}
	return string(b)
}
