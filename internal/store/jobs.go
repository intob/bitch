package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Jobs are scheduled turns (DESIGN.md §6.1, PLAN-3-JOBS.md): a cron spec plus a
// prompt, run unattended against the owning chat. A job's turns run under a
// session keyed with a synthetic negative ThreadID (see JobKey) so a job keeps
// its own Claude session continuity without clobbering the interactive one.

// Job is one scheduled turn definition.
type Job struct {
	ID         int64
	ChatID     int64
	BotID      int64
	Spec       string    // cron spec or @every descriptor, always UTC
	Prompt     string    // the task text handed to the agent
	WorkDir    string    // seeds the job's session row on first run
	CapUSD     float64   // per-run spend reservation (§4.4)
	Enabled    bool      // paused jobs stay in the table but never come due
	NextRun    time.Time // persisted before each launch: crash ⇒ missed run, never double
	LastRun    time.Time // zero if it has never run
	LastStatus string
}

// JobKey is the session key a job's turns run under. Real chats always have
// ThreadID 0 today, and forum topic ids are positive if they ever land, so a
// negative id derived from the job id cannot collide (§11).
func JobKey(chatID, botID, jobID int64) Key {
	return Key{ChatID: chatID, ThreadID: -jobID, BotID: botID}
}

// AddJob inserts a job and returns its id.
func (s *Store) AddJob(j *Job) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(
		`INSERT INTO jobs (chat_id, bot_id, spec, prompt, workdir, cap_usd, enabled, next_run, last_run, last_status)
         VALUES (?,?,?,?,?,?,?,?,0,'')`,
		j.ChatID, j.BotID, j.Spec, j.Prompt, j.WorkDir, j.CapUSD,
		boolToInt(j.Enabled), j.NextRun.UTC().Unix())
	if err != nil {
		return 0, fmt.Errorf("store: add job: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: add job id: %w", err)
	}
	return id, nil
}

const jobCols = `id, chat_id, bot_id, spec, prompt, workdir, cap_usd, enabled, next_run, last_run, last_status`

// GetJob returns the job, or (nil, nil) if there is no such id.
func (s *Store) GetJob(id int64) (*Job, error) {
	row := s.db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id=?`, id)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get job: %w", err)
	}
	return j, nil
}

// ListJobs returns every job owned by a chat, oldest first.
func (s *Store) ListJobs(chatID, botID int64) ([]Job, error) {
	rows, err := s.db.Query(`SELECT `+jobCols+` FROM jobs WHERE chat_id=? AND bot_id=? ORDER BY id`,
		chatID, botID)
	if err != nil {
		return nil, fmt.Errorf("store: list jobs: %w", err)
	}
	defer rows.Close()
	return scanJobs(rows)
}

// DueJobs returns the enabled jobs whose next run has arrived.
func (s *Store) DueJobs(now time.Time) ([]Job, error) {
	rows, err := s.db.Query(`SELECT `+jobCols+` FROM jobs WHERE enabled=1 AND next_run<=? ORDER BY next_run`,
		now.UTC().Unix())
	if err != nil {
		return nil, fmt.Errorf("store: due jobs: %w", err)
	}
	defer rows.Close()
	return scanJobs(rows)
}

// DeleteJob removes a job. Returns (false, nil) if there was no such id.
func (s *Store) DeleteJob(id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM jobs WHERE id=?`, id)
	if err != nil {
		return false, fmt.Errorf("store: delete job: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetJobEnabled pauses or resumes a job. Resuming also sets next_run so a job
// paused for a week doesn't come due the instant it's switched back on.
func (s *Store) SetJobEnabled(id int64, enabled bool, next time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE jobs SET enabled=?, next_run=? WHERE id=?`,
		boolToInt(enabled), next.UTC().Unix(), id)
	if err != nil {
		return fmt.Errorf("store: set job enabled: %w", err)
	}
	return nil
}

// SetJobNextRun records when the job is due again. The scheduler calls this
// before launching a turn, so a crash mid-turn costs a run rather than
// repeating one (fail closed applies to spending too).
func (s *Store) SetJobNextRun(id int64, next time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE jobs SET next_run=? WHERE id=?`, next.UTC().Unix(), id)
	if err != nil {
		return fmt.Errorf("store: set job next run: %w", err)
	}
	return nil
}

// SetJobResult records the outcome of a run for /jobs list.
func (s *Store) SetJobResult(id int64, when time.Time, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE jobs SET last_run=?, last_status=? WHERE id=?`,
		when.UTC().Unix(), status, id)
	if err != nil {
		return fmt.Errorf("store: set job result: %w", err)
	}
	return nil
}

type scanner interface{ Scan(...any) error }

func scanJob(sc scanner) (*Job, error) {
	var (
		j                Job
		enabled          int
		nextRun, lastRun int64
	)
	err := sc.Scan(&j.ID, &j.ChatID, &j.BotID, &j.Spec, &j.Prompt, &j.WorkDir,
		&j.CapUSD, &enabled, &nextRun, &lastRun, &j.LastStatus)
	if err != nil {
		return nil, err
	}
	j.Enabled = enabled != 0
	j.NextRun = time.Unix(nextRun, 0).UTC()
	if lastRun != 0 {
		j.LastRun = time.Unix(lastRun, 0).UTC()
	}
	return &j, nil
}

func scanJobs(rows *sql.Rows) ([]Job, error) {
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan job: %w", err)
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
