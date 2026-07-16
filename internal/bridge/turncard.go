package bridge

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/intob/bitch/internal/agent"
)

// turnCard is the live activity model behind the status message (§7): a
// rolling feed of tool calls that resolve to ✅/❌ as their results stream in,
// plus elapsed time, token totals, and a running cost estimate — an RC-style
// real-time view of the turn, inside Telegram. The agent callbacks mutate it
// and a ticker re-renders it so the clock keeps moving during long tool runs;
// tg.Status throttles the actual edits. When the turn ends the card is
// finalized in place (not deleted) so the chat keeps a record of what ran.
type turnCard struct {
	mu    sync.Mutex
	start time.Time
	model string
	state string      // header while running: "thinking…" / "working…"
	lines []cardLine  // most recent tool calls, oldest first
	drop  int         // lines evicted from the front to bound the card
	used  agent.Usage // summed across assistant message events
}

type cardLine struct {
	id      string
	name    string
	summary string
	status  byte // 0 running, 'v' ok, 'x' error
}

// maxCardLines bounds the rendered feed: 8 lines plus header stays far under
// Telegram's 4096-char message cap even with maxed-out summaries.
const maxCardLines = 8

const (
	stateThinking = "💭 thinking…"
	stateWorking  = "🔧 working…"
)

func newTurnCard(model string) *turnCard {
	return &turnCard{start: time.Now(), model: model, state: stateThinking}
}

func (c *turnCard) SetState(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = s
}

// StartTool appends a running line for a tool call, evicting the oldest line
// once the card is full.
func (c *turnCard) StartTool(id, name, summary string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = stateWorking
	c.lines = append(c.lines, cardLine{id: id, name: name, summary: summary})
	if len(c.lines) > maxCardLines {
		c.lines = c.lines[len(c.lines)-maxCardLines:]
		c.drop++
	}
}

// FinishTool resolves the line with the given tool_use id. Results for lines
// already evicted are dropped silently; tool calls can run in parallel, so the
// match is by id, not position.
func (c *turnCard) FinishTool(id string, isError bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.lines) - 1; i >= 0; i-- {
		if c.lines[i].id == id && c.lines[i].status == 0 {
			if isError {
				c.lines[i].status = 'x'
			} else {
				c.lines[i].status = 'v'
			}
			return
		}
	}
}

func (c *turnCard) AddUsage(u agent.Usage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.used.InputTokens += u.InputTokens
	c.used.OutputTokens += u.OutputTokens
	c.used.CacheCreationInputTokens += u.CacheCreationInputTokens
	c.used.CacheReadInputTokens += u.CacheReadInputTokens
}

// Render builds the in-flight card: header (state · elapsed · tokens · ~cost)
// plus the activity feed.
func (c *turnCard) Render() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.render(c.state, 0)
}

// RenderFinal builds the last edit of the card. outcome is the header ("✅
// done", "🛑 stopped", "⚠️ failed"); res, when non-nil, supplies the
// authoritative cost from the result event (preferred over the estimate).
func (c *turnCard) RenderFinal(outcome string, res *agent.Result) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var cost float64
	if res != nil {
		cost = res.CostUSD
	}
	return c.render(outcome, cost)
}

// render assembles the card. Callers must hold c.mu. actualCost > 0 replaces
// the estimate (final render); otherwise the estimate is shown with a ~.
func (c *turnCard) render(header string, actualCost float64) string {
	parts := []string{header, fmtDuration(time.Since(c.start))}
	if t := c.used.Total(); t > 0 {
		parts = append(parts, fmtTokens(t)+" tok")
	}
	if actualCost > 0 {
		parts = append(parts, fmt.Sprintf("$%.2f", actualCost))
	} else if est, ok := estimateCost(c.model, c.used); ok && est > 0 {
		parts = append(parts, fmt.Sprintf("~$%.2f", est))
	}
	var b strings.Builder
	b.WriteString(tgEscape(strings.Join(parts, " · ")))
	if c.drop > 0 {
		fmt.Fprintf(&b, "\n… %d earlier step(s)", c.drop)
	}
	for _, l := range c.lines {
		icon := "⏳"
		switch l.status {
		case 'v':
			icon = "✅"
		case 'x':
			icon = "❌"
		}
		b.WriteString("\n" + icon + " <b>" + tgEscape(l.name) + "</b>")
		if l.summary != "" {
			b.WriteString(" <code>" + tgEscape(truncateRunes(l.summary, 72)) + "</code>")
		}
	}
	return b.String()
}

// tokenPrices are USD per million tokens (input, output), prefix-matched on
// the model id so dated ids (claude-haiku-4-5-20251001) resolve. Used only
// for the card's live estimate — the budget guard consumes the CLI's
// authoritative total_cost_usd. Cache writes bill at 1.25× input and cache
// reads at 0.1× (5-minute TTL). Prices as of 2026-07
// (platform.claude.com/docs/en/pricing); an unknown model just omits the $.
var tokenPrices = []struct {
	prefix  string
	in, out float64 // $/MTok
}{
	{"claude-opus-4", 5, 25},
	{"claude-sonnet-5", 3, 15},
	{"claude-sonnet-4", 3, 15},
	{"claude-haiku-4", 1, 5},
	{"claude-fable-5", 10, 50},
	{"claude-mythos-5", 10, 50},
}

func estimateCost(model string, u agent.Usage) (float64, bool) {
	for _, p := range tokenPrices {
		if strings.HasPrefix(model, p.prefix) {
			weightedIn := float64(u.InputTokens) +
				1.25*float64(u.CacheCreationInputTokens) +
				0.1*float64(u.CacheReadInputTokens)
			return (weightedIn*p.in + float64(u.OutputTokens)*p.out) / 1e6, true
		}
	}
	return 0, false
}

func fmtTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func fmtDuration(d time.Duration) string {
	d = d.Truncate(time.Second)
	if d < time.Second {
		d = time.Second
	}
	return d.String()
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
