package bridge

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/intob/bitch/internal/store"
)

// Scheduled turns — the half of the always-on box that isn't reactive (§6.1,
// PLAN-3-JOBS.md). A job is a cron spec plus a prompt; its turns run under a
// session keyed by a synthetic negative thread id (store.JobKey), so a job has
// its own Claude session continuity, workdir, and tier without touching the
// interactive session, and approval routing (§4.3) works unchanged — the hook
// has no idea the turn was scheduled.

// tickInterval is how often the scheduler looks for due jobs. Cron's finest
// grain here is a minute, so 30s is enough to fire punctually without spinning.
const tickInterval = 30 * time.Second

// defaultCapUSD is the per-run spend reservation a job gets when `cap=` is
// omitted. A job burning the daily budget at 03:00 while nobody is watching is
// the failure mode this exists for.
const defaultCapUSD = 2.00

// cronParser accepts both 5-field specs ("0 2 * * *") and descriptors
// ("@every 4h", "@daily"). Everything is UTC and said so in /jobs output.
var cronParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// nextRun computes when spec fires after from. Always UTC: the box's local zone
// is irrelevant to a user whose phone is somewhere else, so one stated zone
// beats a guessed one.
func nextRun(spec string, from time.Time) (time.Time, error) {
	sched, err := cronParser.Parse(spec)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad schedule %q: %w", spec, err)
	}
	return sched.Next(from.UTC()), nil
}

// runScheduler is the job loop. Its own goroutine, started from Run — it must
// never touch the poll loop, and each job turn is `go`-spawned like an
// interactive one. An overdue job (box was down) fires once on the first tick;
// older misses collapse into it, because cron's Next always returns a future
// time.
func (b *Bridge) runScheduler(ctx context.Context) {
	t := time.NewTicker(tickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			due, err := b.store.DueJobs(time.Now())
			if err != nil {
				log.Printf("scheduler: due jobs: %v", err)
				continue
			}
			for _, job := range due {
				b.startJob(ctx, job)
			}
		}
	}
}

// startJob takes the job's turn slot, advances next_run, and launches the turn.
// It runs on the scheduler goroutine and does no I/O beyond the store, so the
// tick stays quick.
//
// The order matters: next_run is persisted *before* the turn launches, so a
// crash mid-turn costs a run rather than repeating one — fail closed applies to
// money too.
func (b *Bridge) startJob(ctx context.Context, job store.Job) {
	key := store.JobKey(job.ChatID, job.BotID, job.ID)
	runCtx, cancel := context.WithCancel(ctx)
	if !b.claimIdle(key, cancel) {
		// The previous run of this same job is still going. Leave next_run in the
		// past so the next tick retries once it finishes.
		cancel()
		return
	}
	next, err := nextRun(job.Spec, time.Now())
	if err != nil {
		// The spec parsed at /jobs add time, so this is a corrupt row. Pause it
		// rather than spin on it every 30s forever.
		log.Printf("scheduler: job %d: %v", job.ID, err)
		_ = b.store.SetJobEnabled(job.ID, false, time.Now())
		b.releaseTurn(key)
		b.reply(job.ChatID, fmt.Sprintf("⚠️ job #%d paused — unparseable schedule %q.", job.ID, job.Spec))
		return
	}
	if err := b.store.SetJobNextRun(job.ID, next); err != nil {
		// Couldn't reserve the slot in the schedule; skip rather than risk a
		// double run on the next tick.
		log.Printf("scheduler: job %d: persist next run: %v", job.ID, err)
		b.releaseTurn(key)
		return
	}
	go b.runJobTurn(runCtx, key, job)
}

// runJobTurn runs one occurrence. The caller already owns the turn slot for key.
func (b *Bridge) runJobTurn(runCtx context.Context, key store.Key, job store.Job) {
	defer b.releaseTurn(key)

	// Pre-flight spend reservation (§4.4). The CLI only reports cost at the
	// result event, so this cannot abort a turn mid-flight — it decides whether
	// to start one. The daily budget guard in runTurn remains the backstop.
	if b.cfg.DailyBudgetUSD > 0 {
		spent, err := b.store.SpendToday()
		if err == nil && spent+job.CapUSD > b.cfg.DailyBudgetUSD {
			b.reply(job.ChatID, fmt.Sprintf(
				"⏭️ job #%d skipped — its $%.2f cap would risk the daily budget ($%.2f of $%.2f spent).",
				job.ID, job.CapUSD, spent, b.cfg.DailyBudgetUSD))
			_ = b.store.SetJobResult(job.ID, time.Now(), "skipped (budget)")
			return
		}
	}

	// The job's session row carries its workdir and inherited tier; runTurn would
	// otherwise invent chat defaults for this synthetic key.
	if err := b.ensureJobSession(job); err != nil {
		log.Printf("job %d: session: %v", job.ID, err)
		b.reply(job.ChatID, fmt.Sprintf("⚠️ job #%d: %v", job.ID, err))
		_ = b.store.SetJobResult(job.ID, time.Now(), "failed")
		return
	}

	status := b.runTurn(runCtx, key, job.ChatID, jobPrompt(job), fmt.Sprintf("⏰ job #%d", job.ID))
	_ = b.store.SetJobResult(job.ID, time.Now(), status)
}

// jobPrompt tells the agent what it's doing here — an unprompted turn with no
// human in the loop reads very differently from a reply, and the spec is the
// context for "why now".
func jobPrompt(job store.Job) string {
	return fmt.Sprintf("This is scheduled job #%d (schedule: %s, UTC). "+
		"It runs unattended — nobody is necessarily watching. Task: %s",
		job.ID, job.Spec, job.Prompt)
}

// ensureJobSession creates the job's session row if it's missing, seeding the
// workdir from the job and the tier from the config default. The normal path is
// that /jobs add already made it with the chat's tier inherited; this is the
// repair path (a hand-edited DB, a row lost), and it fails closed to the default
// tier rather than to the chat's current one.
func (b *Bridge) ensureJobSession(job store.Job) error {
	key := store.JobKey(job.ChatID, job.BotID, job.ID)
	sess, err := b.store.Get(key)
	if err != nil {
		return err
	}
	if sess != nil {
		return nil
	}
	return b.store.Upsert(&store.Session{
		Key:     key,
		WorkDir: job.WorkDir,
		Tier:    b.cfg.DefaultTier,
	})
}
