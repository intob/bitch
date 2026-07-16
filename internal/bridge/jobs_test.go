package bridge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/intob/bitch/internal/store"
)

func TestNextRun(t *testing.T) {
	// A Thursday, 02:30 UTC.
	from := time.Date(2026, 7, 16, 2, 30, 0, 0, time.UTC)
	tests := []struct {
		name string
		spec string
		want time.Time
	}{
		{
			name: "daily cron rolls to tomorrow when today's slot has passed",
			spec: "0 2 * * *",
			want: time.Date(2026, 7, 17, 2, 0, 0, 0, time.UTC),
		},
		{
			name: "cron slot later today",
			spec: "45 2 * * *",
			want: time.Date(2026, 7, 16, 2, 45, 0, 0, time.UTC),
		},
		{
			name: "@every is relative to now",
			spec: "@every 4h",
			want: time.Date(2026, 7, 16, 6, 30, 0, 0, time.UTC),
		},
		{
			name: "@daily descriptor is midnight UTC",
			spec: "@daily",
			want: time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "day-of-week cron",
			spec: "0 9 * * MON",
			want: time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := nextRun(tc.spec, from)
			if err != nil {
				t.Fatalf("nextRun(%q) error: %v", tc.spec, err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("nextRun(%q) = %v, want %v", tc.spec, got.UTC(), tc.want)
			}
		})
	}
}

// A local-zone `from` must still schedule against UTC — the box's zone is
// irrelevant to a user whose phone is elsewhere, so /jobs states one zone.
func TestNextRunIsUTC(t *testing.T) {
	zone := time.FixedZone("UTC+5", 5*60*60)
	from := time.Date(2026, 7, 16, 6, 0, 0, 0, zone) // 01:00 UTC
	got, err := nextRun("0 2 * * *", from)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("nextRun from a non-UTC clock = %v, want %v", got.UTC(), want)
	}
}

func TestNextRunRejectsBadSpec(t *testing.T) {
	for _, spec := range []string{"", "not a spec", "0 2 * *", "99 * * * *", "@every"} {
		if _, err := nextRun(spec, time.Now()); err == nil {
			t.Errorf("nextRun(%q) = nil error, want a parse failure", spec)
		}
	}
}

func TestParseJobAdd(t *testing.T) {
	tests := []struct {
		name       string
		arg        string
		wantSpec   string
		wantPrompt string
		wantCap    float64
		wantErr    bool
	}{
		{
			name:       "cron spec and prompt",
			arg:        "0 2 * * * :: pull main and run the tests",
			wantSpec:   "0 2 * * *",
			wantPrompt: "pull main and run the tests",
			wantCap:    defaultCapUSD,
		},
		{
			name:       "explicit cap",
			arg:        "0 2 * * * cap=5 :: do the thing",
			wantSpec:   "0 2 * * *",
			wantPrompt: "do the thing",
			wantCap:    5,
		},
		{
			name:       "cap may lead the spec",
			arg:        "cap=0.5 @every 4h :: poll the queue",
			wantSpec:   "@every 4h",
			wantPrompt: "poll the queue",
			wantCap:    0.5,
		},
		{
			name:       "a :: inside the prompt stays in the prompt",
			arg:        "@daily :: run make :: then report",
			wantSpec:   "@daily",
			wantPrompt: "run make :: then report",
			wantCap:    defaultCapUSD,
		},
		{name: "missing separator", arg: "0 2 * * * do a thing", wantErr: true},
		{name: "empty prompt", arg: "0 2 * * * ::", wantErr: true},
		{name: "empty spec", arg: ":: do a thing", wantErr: true},
		{name: "spec of only a cap", arg: "cap=5 :: do a thing", wantErr: true},
		{name: "non-numeric cap", arg: "@daily cap=lots :: do a thing", wantErr: true},
		{name: "zero cap", arg: "@daily cap=0 :: do a thing", wantErr: true},
		{name: "negative cap", arg: "@daily cap=-1 :: do a thing", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec, prompt, capUSD, err := parseJobAdd(tc.arg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseJobAdd(%q) = (%q, %q, %v, nil), want an error",
						tc.arg, spec, prompt, capUSD)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseJobAdd(%q) error: %v", tc.arg, err)
			}
			if spec != tc.wantSpec {
				t.Errorf("spec = %q, want %q", spec, tc.wantSpec)
			}
			if prompt != tc.wantPrompt {
				t.Errorf("prompt = %q, want %q", prompt, tc.wantPrompt)
			}
			if capUSD != tc.wantCap {
				t.Errorf("cap = %v, want %v", capUSD, tc.wantCap)
			}
		})
	}
}

// A job that overruns its own schedule must skip the occurrence, not queue a
// pile-up behind itself.
func TestClaimIdleSkipsWhenBusy(t *testing.T) {
	b := testBridge()
	key := store.JobKey(7, 42, 1)
	if !b.claimIdle(key, noopCancel) {
		t.Fatal("claimIdle on an idle key = false, want true")
	}
	if b.claimIdle(key, noopCancel) {
		t.Fatal("claimIdle on a busy key = true, want false (the tick must skip)")
	}
	b.releaseTurn(key)
	if !b.claimIdle(key, noopCancel) {
		t.Fatal("claimIdle after release = false, want true")
	}
}

// A job turn and an interactive turn in the same chat run on different keys, so
// a nightly job must never make the human's chat look busy.
func TestJobTurnDoesNotBlockTheChat(t *testing.T) {
	b := testBridge()
	if !b.claimIdle(store.JobKey(testKey.ChatID, testKey.BotID, 1), noopCancel) {
		t.Fatal("claimIdle for the job = false, want true")
	}
	if got := b.claimOrQueue(testKey, "hello", noopCancel); got != turnClaimed {
		t.Errorf("interactive claim while a job runs = %v, want turnClaimed", got)
	}
}

// /stop means "stop what I can see": a job streams its card into the chat, so
// it must be cancellable from that chat even though it runs on its own key.
func TestStopSweepsJobTurnsInTheChat(t *testing.T) {
	b := testBridge()
	var stopped []string
	mark := func(s string) context.CancelFunc { return func() { stopped = append(stopped, s) } }

	if got := b.claimOrQueue(testKey, "interactive", mark("chat")); got != turnClaimed {
		t.Fatal("interactive claim failed")
	}
	if got := b.claimOrQueue(testKey, "queued behind it", noopCancel); got != turnQueued {
		t.Fatal("expected the second prompt to queue")
	}
	jobKey := store.JobKey(testKey.ChatID, testKey.BotID, 1)
	if !b.claimIdle(jobKey, mark("job")) {
		t.Fatal("job claim failed")
	}
	// A turn in a different chat must be left strictly alone.
	other := store.Key{ChatID: 99, BotID: testKey.BotID}
	if !b.claimIdle(other, mark("other chat")) {
		t.Fatal("other-chat claim failed")
	}

	cancels, dropped, jobs := b.takeStops(testKey)
	for _, c := range cancels {
		c()
	}
	if len(cancels) != 2 {
		t.Errorf("cancelled %d turn(s), want 2 (the chat's turn and its job)", len(cancels))
	}
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1 queued prompt", dropped)
	}
	if jobs != 1 {
		t.Errorf("jobs = %d, want 1", jobs)
	}
	for _, s := range stopped {
		if s == "other chat" {
			t.Error("/stop cancelled a turn in a different chat")
		}
	}
	// The queue must be cleared, not just the turn cancelled — stopping means
	// stopping the stream, not queuing a surprise follow-up.
	b.mu.Lock()
	defer b.mu.Unlock()
	if n := len(b.runs[testKey].queue); n != 0 {
		t.Errorf("queue after /stop = %d, want 0", n)
	}
}

func TestJobPromptCarriesIDAndSpec(t *testing.T) {
	got := jobPrompt(store.Job{ID: 3, Spec: "0 2 * * *", Prompt: "run the tests"})
	for _, want := range []string{"#3", "0 2 * * *", "run the tests", "unattended"} {
		if !strings.Contains(got, want) {
			t.Errorf("jobPrompt() = %q, missing %q", got, want)
		}
	}
}
