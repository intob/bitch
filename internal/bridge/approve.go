package bridge

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/intob/bitch/internal/policy"
	"github.com/intob/bitch/internal/store"
)

// pendingApproval is an in-flight Telegram approval awaiting a button press.
type pendingApproval struct {
	ch     chan bool // true = allow
	chatID int64
	msgID  int // the approval card message id
}

// hookRequest is what `bitch hook` POSTs to the loopback server: the tool call
// Claude Code is about to make, plus the session id so we can find the chat.
type hookRequest struct {
	SessionID string         `json:"session_id"`
	Tool      string         `json:"tool_name"`
	Input     map[string]any `json:"tool_input"`
	CWD       string         `json:"cwd"`
}

// hookResponse is the decision returned to the hook, which translates it into
// Claude Code's PreToolUse contract.
type hookResponse struct {
	Decision string `json:"decision"` // allow | deny
	Reason   string `json:"reason"`
}

// serveApprovals runs the loopback HTTP server the permission hook calls into
// (§4.3). It binds to 127.0.0.1 only — no inbound app ports (§3) — and is gated
// by a shared secret so nothing else on the box can drive approvals. The
// listener is created by Run so a bind failure aborts startup cleanly.
func (b *Bridge) serveApprovals(ctx context.Context, ln net.Listener) {
	mux := http.NewServeMux()
	mux.HandleFunc("/approve", b.handleApprove)
	mux.HandleFunc("/store", b.handleStore)
	mux.HandleFunc("/sendfile", b.handleSendFile)

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	log.Printf("approval server listening on %s (loopback)", b.cfg.HookAddr)
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Printf("approval server: %v", err)
	}
}

func (b *Bridge) handleApprove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	// Shared-secret gate: the hook sends the secret we injected into claude's env.
	if r.Header.Get("X-Bitch-Secret") != b.cfg.HookSecret {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req hookRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDecision(w, "deny", "bad request")
		return
	}

	decision, reason := b.decide(req)
	writeDecision(w, decision, reason)
}

// decide applies the tier policy and, when the verdict is Ask, blocks on a
// Telegram approval. It fails closed: an unknown session, or any error, denies.
func (b *Bridge) decide(req hookRequest) (decision, reason string) {
	sess, err := b.store.GetByClaudeSession(req.SessionID)
	if err != nil || sess == nil {
		return "deny", "no matching session (fail closed)"
	}
	workdir := sess.WorkDir
	if req.CWD != "" {
		workdir = req.CWD
	}

	v := policy.Evaluate(policy.Request{
		Tool:    req.Tool,
		Input:   req.Input,
		WorkDir: workdir,
		Tier:    sess.Tier,
	}, b.cfg.WithinRoots)

	switch v.Decision {
	case policy.Allow:
		return "allow", v.Reason
	case policy.Deny:
		return "deny", v.Reason
	default: // Ask over Telegram (§4.3)
		return b.askHuman(sess.Key, v)
	}
}

// askHuman posts an approval card and blocks until the user answers or the
// configured timeout elapses (timeout ⇒ deny, §4.3).
func (b *Bridge) askHuman(key store.Key, v policy.Verdict) (string, string) {
	id := randHex(8)
	msgID, err := b.sender.SendApproval(key.ChatID, id, v.Action, v.Detail)
	if err != nil {
		return "deny", "could not reach you for approval"
	}
	p := &pendingApproval{ch: make(chan bool, 1), chatID: key.ChatID, msgID: msgID}
	b.mu.Lock()
	b.pending[id] = p
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
	}()

	select {
	case allow := <-p.ch:
		if allow {
			b.sender.ResolveApproval(key.ChatID, msgID, "✅ approved: "+tgEscape(v.Action))
			return "allow", "approved by user"
		}
		b.sender.ResolveApproval(key.ChatID, msgID, "❌ denied: "+tgEscape(v.Action))
		return "deny", "denied by user"
	case <-time.After(b.cfg.ApprovalTimeout):
		b.sender.ResolveApproval(key.ChatID, msgID, "⌛ timed out (denied): "+tgEscape(v.Action))
		return "deny", "approval timed out"
	}
}

// resolveApprovalCallback handles an ✅/❌ button press on an approval card.
func (b *Bridge) resolveApprovalCallback(cb *tgbotapi.CallbackQuery) {
	allow := strings.HasPrefix(cb.Data, "ok:")
	id := cb.Data[strings.IndexByte(cb.Data, ':')+1:]

	b.mu.Lock()
	p, ok := b.pending[id]
	b.mu.Unlock()
	if !ok {
		b.sender.AnswerCallback(cb.ID, "expired")
		if cb.Message != nil {
			b.sender.ResolveApproval(cb.Message.Chat.ID, cb.Message.MessageID, "⌛ already resolved.")
		}
		return
	}
	select {
	case p.ch <- allow:
	default:
	}
	if allow {
		b.sender.AnswerCallback(cb.ID, "allowed")
	} else {
		b.sender.AnswerCallback(cb.ID, "denied")
	}
}

func writeDecision(w http.ResponseWriter, decision, reason string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(hookResponse{Decision: decision, Reason: reason})
}
