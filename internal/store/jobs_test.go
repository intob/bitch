package store

import (
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// baseTime is a fixed instant so the tests don't depend on the wall clock.
var baseTime = time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)

func sampleJob(chatID int64, next time.Time) *Job {
	return &Job{
		ChatID:  chatID,
		BotID:   42,
		Spec:    "0 2 * * *",
		Prompt:  "pull main and run the tests",
		WorkDir: "/srv/work",
		CapUSD:  2,
		Enabled: true,
		NextRun: next,
	}
}

func TestJobRoundTrip(t *testing.T) {
	st := openTestStore(t)

	// Not-found convention.
	if got, err := st.GetJob(1); err != nil || got != nil {
		t.Fatalf("GetJob on empty = (%v, %v), want (nil, nil)", got, err)
	}

	id, err := st.AddJob(sampleJob(7, baseTime))
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetJob(id)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case got.ID != id:
		t.Errorf("ID = %d, want %d", got.ID, id)
	case got.Spec != "0 2 * * *":
		t.Errorf("Spec = %q", got.Spec)
	case got.Prompt != "pull main and run the tests":
		t.Errorf("Prompt = %q", got.Prompt)
	case got.WorkDir != "/srv/work":
		t.Errorf("WorkDir = %q", got.WorkDir)
	case got.CapUSD != 2:
		t.Errorf("CapUSD = %v, want 2", got.CapUSD)
	case !got.Enabled:
		t.Error("Enabled = false, want true")
	case !got.NextRun.Equal(baseTime):
		t.Errorf("NextRun = %v, want %v", got.NextRun, baseTime)
	case !got.LastRun.IsZero():
		t.Errorf("LastRun = %v, want zero for a job that never ran", got.LastRun)
	case got.LastStatus != "":
		t.Errorf("LastStatus = %q, want empty", got.LastStatus)
	}

	// Deleting is idempotent-reporting: second delete says "nothing there".
	if ok, err := st.DeleteJob(id); err != nil || !ok {
		t.Fatalf("DeleteJob = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := st.DeleteJob(id); err != nil || ok {
		t.Fatalf("second DeleteJob = (%v, %v), want (false, nil)", ok, err)
	}
}

// ListJobs must be scoped to the owning chat: job ids are per-chat handles, not
// a cross-chat one (§11).
func TestListJobsScopedToChat(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.AddJob(sampleJob(7, baseTime)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddJob(sampleJob(8, baseTime)); err != nil {
		t.Fatal(err)
	}
	jobs, err := st.ListJobs(7, 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ChatID != 7 {
		t.Fatalf("ListJobs(7) = %+v, want exactly chat 7's job", jobs)
	}
	if jobs, err := st.ListJobs(7, 99); err != nil || len(jobs) != 0 {
		t.Fatalf("ListJobs with a foreign bot id = (%v, %v), want empty", jobs, err)
	}
}

func TestDueJobs(t *testing.T) {
	tests := []struct {
		name    string
		next    time.Time
		enabled bool
		now     time.Time
		want    bool
	}{
		{name: "future is not due", next: baseTime.Add(time.Minute), enabled: true, now: baseTime},
		{name: "exactly now is due", next: baseTime, enabled: true, now: baseTime, want: true},
		{name: "overdue is due", next: baseTime.Add(-time.Hour), enabled: true, now: baseTime, want: true},
		{name: "paused is never due", next: baseTime.Add(-time.Hour), enabled: false, now: baseTime},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := openTestStore(t)
			j := sampleJob(7, tc.next)
			j.Enabled = tc.enabled
			if _, err := st.AddJob(j); err != nil {
				t.Fatal(err)
			}
			due, err := st.DueJobs(tc.now)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(due) == 1; got != tc.want {
				t.Errorf("DueJobs returned %d job(s), want due=%v", len(due), tc.want)
			}
		})
	}
}

func TestDueJobsOrderedAndSubsecond(t *testing.T) {
	st := openTestStore(t)
	late, err := st.AddJob(sampleJob(7, baseTime))
	if err != nil {
		t.Fatal(err)
	}
	early, err := st.AddJob(sampleJob(7, baseTime.Add(-time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	// Sub-second precision is lost to the unix-second column; a job due at
	// x.999 must still be due at x, not skipped by a truncation.
	due, err := st.DueJobs(baseTime.Add(999 * time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 2 {
		t.Fatalf("DueJobs = %d job(s), want 2", len(due))
	}
	if due[0].ID != early || due[1].ID != late {
		t.Errorf("DueJobs order = [%d %d], want [%d %d] (soonest first)",
			due[0].ID, due[1].ID, early, late)
	}
}

func TestSetJobEnabledAndNextRun(t *testing.T) {
	st := openTestStore(t)
	id, err := st.AddJob(sampleJob(7, baseTime))
	if err != nil {
		t.Fatal(err)
	}

	later := baseTime.Add(24 * time.Hour)
	if err := st.SetJobEnabled(id, false, later); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetJob(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled || !got.NextRun.Equal(later) {
		t.Errorf("after pause: enabled=%v next=%v, want false / %v", got.Enabled, got.NextRun, later)
	}

	next := baseTime.Add(48 * time.Hour)
	if err := st.SetJobNextRun(id, next); err != nil {
		t.Fatal(err)
	}
	if got, err = st.GetJob(id); err != nil {
		t.Fatal(err)
	} else if !got.NextRun.Equal(next) {
		t.Errorf("NextRun = %v, want %v", got.NextRun, next)
	}

	if err := st.SetJobResult(id, baseTime, "ok"); err != nil {
		t.Fatal(err)
	}
	if got, err = st.GetJob(id); err != nil {
		t.Fatal(err)
	} else if got.LastStatus != "ok" || !got.LastRun.Equal(baseTime) {
		t.Errorf("result = %q at %v, want ok at %v", got.LastStatus, got.LastRun, baseTime)
	}
}

// The jobs table lands on databases that already exist on the box, so migrate
// must be idempotent over a reopen and must not disturb existing rows.
func TestMigrateIdempotentOnExistingDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.sqlite")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.AddJob(sampleJob(7, baseTime))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen (re-running migrate): %v", err)
	}
	t.Cleanup(func() { st2.Close() })
	got, err := st2.GetJob(id)
	if err != nil || got == nil {
		t.Fatalf("GetJob after reopen = (%v, %v), want the job to survive", got, err)
	}
}

// JobKey must not collide with the interactive session's key for the same chat
// — that separation is the whole reason jobs get their own thread id (§11).
func TestJobKeyDoesNotCollide(t *testing.T) {
	chat := Key{ChatID: 7, ThreadID: 0, BotID: 42}
	j1 := JobKey(7, 42, 1)
	j2 := JobKey(7, 42, 2)
	if j1 == chat || j2 == chat {
		t.Error("a job key equals the chat's interactive key")
	}
	if j1 == j2 {
		t.Error("two jobs in one chat share a key")
	}
	if j1.ThreadID >= 0 {
		t.Errorf("job ThreadID = %d, want negative (positive ids are reserved for forum topics)", j1.ThreadID)
	}
}
