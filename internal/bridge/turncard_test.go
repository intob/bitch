package bridge

import (
	"fmt"
	"strings"
	"testing"

	"github.com/intob/bitch/internal/agent"
)

func TestTurnCardLifecycle(t *testing.T) {
	c := newTurnCard("claude-opus-4-8")

	if got := c.Render(); !strings.HasPrefix(got, stateThinking) {
		t.Errorf("initial render = %q, want %q header", got, stateThinking)
	}

	// A tool call renders as a running line; HTML in the summary is escaped.
	c.StartTool("t1", "Bash", "grep '<secret>' file")
	got := c.Render()
	if !strings.Contains(got, "⏳ <b>Bash</b>") {
		t.Errorf("running line missing: %q", got)
	}
	if !strings.Contains(got, "&lt;secret&gt;") || strings.Contains(got, "<secret>") {
		t.Errorf("summary not escaped: %q", got)
	}
	if !strings.HasPrefix(got, stateWorking) {
		t.Errorf("header should flip to working: %q", got)
	}

	// Results resolve by id, success and error respectively.
	c.StartTool("t2", "Edit", "policy.go")
	c.FinishTool("t1", false)
	c.FinishTool("t2", true)
	got = c.Render()
	if !strings.Contains(got, "✅ <b>Bash</b>") || !strings.Contains(got, "❌ <b>Edit</b>") {
		t.Errorf("resolution icons wrong: %q", got)
	}
}

func TestTurnCardCostAndTokens(t *testing.T) {
	c := newTurnCard("claude-opus-4-8")
	c.AddUsage(agent.Usage{InputTokens: 1000, OutputTokens: 1000})
	c.AddUsage(agent.Usage{CacheCreationInputTokens: 8000, CacheReadInputTokens: 100_000})

	// opus: (1000 + 1.25*8000 + 0.1*100000)*$5/M + 1000*$25/M = $0.13
	got := c.Render()
	if !strings.Contains(got, "~$0.13") {
		t.Errorf("estimate missing/wrong: %q", got)
	}
	// Sent (input + cache writes), received, and cache reads stay separate: the
	// 100k of replayed context must not read as 110k of fresh work.
	if !strings.Contains(got, "9.0k↑ 1.0k↓ 100.0k cached") {
		t.Errorf("token split missing/wrong: %q", got)
	}

	// The final render prefers the CLI's authoritative cost, without the ~.
	final := c.RenderFinal("✅ done", &agent.Result{CostUSD: 0.42})
	if !strings.Contains(final, "$0.42") || strings.Contains(final, "~$") {
		t.Errorf("final cost wrong: %q", final)
	}
	if !strings.HasPrefix(final, "✅ done") {
		t.Errorf("final header wrong: %q", final)
	}
}

func TestFmtUsage(t *testing.T) {
	tests := []struct {
		name string
		u    agent.Usage
		want string
	}{
		{name: "no usage yet renders nothing", u: agent.Usage{}, want: ""},
		{
			name: "cache writes count as sent, not cached",
			u:    agent.Usage{InputTokens: 1000, CacheCreationInputTokens: 4000, OutputTokens: 500},
			want: "5.0k↑ 500↓",
		},
		{
			name: "a fresh first call omits the cached part",
			u:    agent.Usage{InputTokens: 2000, OutputTokens: 300},
			want: "2.0k↑ 300↓",
		},
		{
			name: "a resumed turn is dominated by cache reads",
			u:    agent.Usage{InputTokens: 200, OutputTokens: 2000, CacheReadInputTokens: 370_000},
			want: "200↑ 2.0k↓ 370.0k cached",
		},
		{
			name: "output-only usage still renders",
			u:    agent.Usage{OutputTokens: 42},
			want: "42↓",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := fmtUsage(tc.u); got != tc.want {
				t.Errorf("fmtUsage() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTurnCardUnknownModelOmitsCost(t *testing.T) {
	c := newTurnCard("claude-future-9")
	c.AddUsage(agent.Usage{InputTokens: 1000, OutputTokens: 1000})
	if got := c.Render(); strings.Contains(got, "$") {
		t.Errorf("unknown model must omit cost: %q", got)
	}
}

func TestTurnCardEviction(t *testing.T) {
	c := newTurnCard("claude-opus-4-8")
	for i := 0; i < maxCardLines+4; i++ {
		c.StartTool(fmt.Sprintf("t%d", i), "Read", fmt.Sprintf("file%d.go", i))
	}
	got := c.Render()
	if !strings.Contains(got, "… 4 earlier step(s)") {
		t.Errorf("eviction note missing: %q", got)
	}
	if strings.Count(got, "<b>Read</b>") != maxCardLines {
		t.Errorf("want %d lines, got %d: %q", maxCardLines, strings.Count(got, "<b>Read</b>"), got)
	}
	// A result for an evicted line must be ignored, not corrupt the card.
	c.FinishTool("t0", false)
	if strings.Contains(c.Render(), "✅") {
		t.Errorf("evicted line resolved: %q", c.Render())
	}
}

func TestEstimateCostPrefixMatch(t *testing.T) {
	// Dated haiku id must resolve via prefix: 1M in + 1M out = $1 + $5.
	got, ok := estimateCost("claude-haiku-4-5-20251001", agent.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	if !ok || got != 6 {
		t.Errorf("haiku estimate = %v %v, want 6 true", got, ok)
	}
	if _, ok := estimateCost("gpt-9", agent.Usage{InputTokens: 1}); ok {
		t.Error("unknown model must not estimate")
	}
}
