package bridge

import (
	"context"
	"strings"
	"testing"

	"github.com/intob/bitch/internal/store"
)

// testBridge is the bare minimum for exercising the turn-slot gate: the queue
// mechanics touch only b.mu and b.runs, so no Telegram or runner is needed.
func testBridge() *Bridge {
	return &Bridge{runs: make(map[store.Key]*activeRun)}
}

var testKey = store.Key{ChatID: 1, BotID: 2}

func noopCancel() {}

// claim installs a turn slot for testKey the way handleAgentTurn's first
// claimOrQueue does, and fails the test if it doesn't win the gate.
func claim(t *testing.T, b *Bridge) {
	t.Helper()
	if got := b.claimOrQueue(testKey, "first", noopCancel); got != turnClaimed {
		t.Fatalf("claimOrQueue on an idle chat = %v, want turnClaimed", got)
	}
}

func TestClaimOrQueue(t *testing.T) {
	tests := []struct {
		name   string
		before []string // prompts queued behind the running turn first
		prompt string
		want   claimResult
	}{
		{name: "idle chat claims the slot", prompt: "hello", want: turnClaimed},
		{name: "busy chat queues", before: []string{"a"}, prompt: "b", want: turnQueued},
		{
			name:   "queue full by count refuses",
			before: repeat("x", maxQueuedPrompts),
			prompt: "one too many",
			want:   turnRejected,
		},
		{
			name:   "queue full by bytes refuses",
			before: []string{strings.Repeat("x", maxQueuedBytes-1)},
			prompt: "ab", // 1 byte of headroom, 2 bytes offered
			want:   turnRejected,
		},
		{
			name:   "a prompt that exactly fills the byte budget is accepted",
			before: []string{strings.Repeat("x", maxQueuedBytes-1)},
			prompt: "a",
			want:   turnQueued,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := testBridge()
			if tc.want != turnClaimed {
				claim(t, b) // occupy the slot so the prompt hits the queue path
			}
			for _, p := range tc.before {
				if got := b.claimOrQueue(testKey, p, noopCancel); got != turnQueued {
					t.Fatalf("setup: claimOrQueue(%q) = %v, want turnQueued", p, got)
				}
			}
			if got := b.claimOrQueue(testKey, tc.prompt, noopCancel); got != tc.want {
				t.Errorf("claimOrQueue(%q) = %v, want %v", tc.prompt, got, tc.want)
			}
		})
	}
}

// A rejected prompt must not silently consume queue budget.
func TestClaimOrQueueRejectionDoesNotAppend(t *testing.T) {
	b := testBridge()
	claim(t, b)
	for _, p := range repeat("x", maxQueuedPrompts) {
		b.claimOrQueue(testKey, p, noopCancel)
	}
	b.claimOrQueue(testKey, "rejected", noopCancel)
	if got := len(b.runs[testKey].queue); got != maxQueuedPrompts {
		t.Errorf("queue length after a rejection = %d, want %d", got, maxQueuedPrompts)
	}
}

func TestNextTurnDrainsAll(t *testing.T) {
	b := testBridge()
	claim(t, b)
	b.claimOrQueue(testKey, "check the config", noopCancel)
	b.claimOrQueue(testKey, "and the tests", noopCancel)

	prompt, runCtx, cancel, more := b.nextTurn(context.Background(), testKey)
	if !more {
		t.Fatal("nextTurn with a non-empty queue returned more=false")
	}
	if want := "check the config\n\nand the tests"; prompt != want {
		t.Errorf("drained prompt = %q, want %q", prompt, want)
	}
	// The slot stays owned across the drain — no window for a rival goroutine.
	run, ok := b.runs[testKey]
	if !ok {
		t.Fatal("nextTurn released the turn slot while draining")
	}
	if len(run.queue) != 0 || run.queueBytes != 0 {
		t.Errorf("queue not reset after drain: %d entries, %d bytes", len(run.queue), run.queueBytes)
	}
	// The drained turn gets a fresh cancel, so /stop hits the turn now running.
	if runCtx == nil || cancel == nil {
		t.Fatal("nextTurn returned a nil context or cancel")
	}
	cancel()
	if runCtx.Err() == nil {
		t.Error("cancel from nextTurn did not cancel its context")
	}

	// Second drain: queue is empty, so the slot is released.
	if _, _, _, more := b.nextTurn(context.Background(), testKey); more {
		t.Error("nextTurn with an empty queue returned more=true")
	}
	if _, ok := b.runs[testKey]; ok {
		t.Error("nextTurn left the turn slot occupied after draining empty")
	}
}

// A prompt landing between turn end and drain must join the next combined
// prompt rather than start a second concurrent run.
func TestQueueAppendedDuringTurnJoinsNextPrompt(t *testing.T) {
	b := testBridge()
	claim(t, b)
	// Turn 1 finishes; before nextTurn runs, another message arrives. The slot
	// is still held, so it queues instead of claiming.
	if got := b.claimOrQueue(testKey, "late thought", noopCancel); got != turnQueued {
		t.Fatalf("prompt arriving before the drain = %v, want turnQueued", got)
	}
	prompt, _, _, more := b.nextTurn(context.Background(), testKey)
	if !more || prompt != "late thought" {
		t.Errorf("nextTurn = (%q, %v), want (\"late thought\", true)", prompt, more)
	}
}

func TestCmdStopClearsQueue(t *testing.T) {
	b := testBridge()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if got := b.claimOrQueue(testKey, "first", cancel); got != turnClaimed {
		t.Fatalf("claimOrQueue = %v, want turnClaimed", got)
	}
	b.claimOrQueue(testKey, "follow-up", noopCancel)

	// The body of cmdStop, minus the Telegram replies it can't make here.
	b.mu.Lock()
	run := b.runs[testKey]
	dropped := len(run.queue)
	run.queue, run.queueBytes = nil, 0
	stop := run.cancel
	b.mu.Unlock()
	stop()

	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
	if ctx.Err() == nil {
		t.Error("/stop did not cancel the running turn")
	}
	// With the queue cleared the drain loop must exit, not run the follow-up.
	if prompt, _, _, more := b.nextTurn(context.Background(), testKey); more {
		t.Errorf("a follow-up turn ran after /stop with prompt %q", prompt)
	}
}

func repeat(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}
