package routines

import (
	"testing"
	"time"
)

// TestCronScheduleSurvivesStoreRoundTrip guards the root cause of a cron job
// firing exactly once then dying: Schedule.cron is json:"-" (runtime only), so
// a schedule loaded from the store used to come back with a nil parser and
// NextRun always returned nil. UnmarshalJSON must re-parse Expr on load.
func TestCronScheduleSurvivesStoreRoundTrip(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	sched, err := ParseSchedule("0 11 * * *", now)
	if err != nil {
		t.Fatal(err)
	}
	job := &Job{
		ID:        "job-cron",
		Name:      "cron",
		Prompt:    "x",
		Schedule:  sched,
		Enabled:   true,
		State:     JobStateIdle,
		NextRunAt: sched.NextRun(now),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.PutJob(job); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetJob("job-cron")
	if err != nil {
		t.Fatal(err)
	}
	next := loaded.Schedule.NextRun(time.Date(2026, 3, 1, 10, 30, 0, 0, time.UTC))
	want := time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC)
	if next == nil || !next.Equal(want) {
		t.Fatalf("NextRun after store round-trip = %v, want %v (cron parser lost?)", next, want)
	}
}

// TestCronJobAdvancesNextRunAfterFire: after the scheduler fires a cron job,
// next_run_at must advance to the following slot so the job keeps firing.
// Regression: with the parser lost on load, advanceNextRun returned nil and
// the job was dead after its first run.
func TestCronJobAdvancesNextRunAfterFire(t *testing.T) {
	runner := &fakeRunner{response: "out"}
	s, store, _ := newScheduler(t, runner, "")
	defer s.Stop()

	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	due := time.Date(2026, 3, 1, 11, 58, 0, 0, time.UTC)
	sched, err := ParseSchedule("0 11 * * *", now)
	if err != nil {
		t.Fatal(err)
	}
	job := &Job{
		ID:        "job-cron-1",
		Name:      "cron",
		Prompt:    "x",
		Schedule:  sched,
		Enabled:   true,
		State:     JobStateIdle,
		NextRunAt: &due,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := store.PutJob(job); err != nil {
		t.Fatal(err)
	}

	s.TickOnce(now)
	waitFor(t, "job run", func() bool { return runner.calls() == 1 })

	var got *Job
	waitFor(t, "lifecycle", func() bool {
		got, _ = store.GetJob("job-cron-1")
		return got != nil && got.LastStatus != ""
	})
	if got.NextRunAt == nil {
		t.Fatal("next_run_at is nil after cron fire; job will never run again")
	}
	want := time.Date(2026, 3, 2, 11, 0, 0, 0, time.UTC)
	if !got.NextRunAt.Equal(want) {
		t.Fatalf("next_run_at = %v, want %v", got.NextRunAt, want)
	}
}
