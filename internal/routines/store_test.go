package routines

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	fixed := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return fixed })
	return s
}

func sampleJob(id string) *Job {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	sched, _ := ParseSchedule("every 30m", now)
	return &Job{
		ID:        id,
		Name:      "test job",
		Prompt:    "do a thing",
		Schedule:  sched,
		Enabled:   true,
		State:     JobStateIdle,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func TestStoreJobRoundTrip(t *testing.T) {
	s := newTestStore(t)
	job := sampleJob("job-1")
	if err := s.PutJob(job); err != nil {
		t.Fatalf("PutJob: %v", err)
	}
	got, err := s.GetJob("job-1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got == nil || got.Name != "test job" || got.Prompt != "do a thing" {
		t.Fatalf("GetJob = %+v", got)
	}
	if got.Schedule.Kind != ScheduleInterval || got.Schedule.Minutes != 30 {
		t.Fatalf("schedule lost in round-trip: %+v", got.Schedule)
	}
	jobs, err := s.ListJobs()
	if err != nil || len(jobs) != 1 {
		t.Fatalf("ListJobs = %d jobs, err %v", len(jobs), err)
	}
}

func TestStorePutJobReplaces(t *testing.T) {
	s := newTestStore(t)
	if err := s.PutJob(sampleJob("job-1")); err != nil {
		t.Fatal(err)
	}
	updated := sampleJob("job-1")
	updated.Name = "renamed"
	if err := s.PutJob(updated); err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.ListJobs()
	if len(jobs) != 1 || jobs[0].Name != "renamed" {
		t.Fatalf("expected 1 job renamed, got %+v", jobs)
	}
}

func TestStoreUpdateJob(t *testing.T) {
	s := newTestStore(t)
	if err := s.PutJob(sampleJob("job-1")); err != nil {
		t.Fatal(err)
	}
	got, err := s.UpdateJob("job-1", func(j *Job) error {
		j.LastStatus = "ok"
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateJob: %v", err)
	}
	if got == nil || got.LastStatus != "ok" {
		t.Fatalf("UpdateJob result = %+v", got)
	}
	// UpdateJob on a missing ID returns nil, nil.
	missing, err := s.UpdateJob("nope", func(j *Job) error { return nil })
	if err != nil || missing != nil {
		t.Fatalf("UpdateJob(missing) = %v, %v", missing, err)
	}
}

func TestStoreDeleteJob(t *testing.T) {
	s := newTestStore(t)
	if err := s.PutJob(sampleJob("job-1")); err != nil {
		t.Fatal(err)
	}
	ok, err := s.DeleteJob("job-1")
	if err != nil || !ok {
		t.Fatalf("DeleteJob = %v, %v", ok, err)
	}
	ok, _ = s.DeleteJob("job-1")
	if ok {
		t.Fatal("second DeleteJob should report not-found")
	}
	jobs, _ := s.ListJobs()
	if len(jobs) != 0 {
		t.Fatalf("jobs after delete = %d", len(jobs))
	}
}

func TestStorePersistenceAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.PutJob(sampleJob("job-1")); err != nil {
		t.Fatal(err)
	}
	w := &WebhookSubscription{Slug: "pr-watch", Prompt: "review {event.title}", CreatedAt: time.Now().UTC()}
	if err := s1.PutWebhook(w); err != nil {
		t.Fatalf("PutWebhook: %v", err)
	}
	// Reopen with a fresh instance — data must survive on disk.
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	job, err := s2.GetJob("job-1")
	if err != nil || job == nil {
		t.Fatalf("reopened GetJob = %v, %v", job, err)
	}
	hook, err := s2.GetWebhook("pr-watch")
	if err != nil || hook == nil || hook.Prompt != "review {event.title}" {
		t.Fatalf("reopened GetWebhook = %+v, %v", hook, err)
	}
}

func TestStoreMissingFilesAreEmpty(t *testing.T) {
	s := newTestStore(t)
	jobs, err := s.ListJobs()
	if err != nil || len(jobs) != 0 {
		t.Fatalf("empty store ListJobs = %d, %v", len(jobs), err)
	}
	hooks, err := s.ListWebhooks()
	if err != nil || len(hooks) != 0 {
		t.Fatalf("empty store ListWebhooks = %d, %v", len(hooks), err)
	}
}

func TestStoreWebhookRoundTrip(t *testing.T) {
	s := newTestStore(t)
	w := &WebhookSubscription{
		Slug:        "pr-watch",
		Events:      []string{"pull_request"},
		Secret:      "sekret",
		Prompt:      "PR #{pull_request.number}: {pull_request.title}",
		Deliver:     "local",
		DeliverOnly: false,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if err := s.PutWebhook(w); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetWebhook("pr-watch")
	if err != nil || got == nil {
		t.Fatalf("GetWebhook = %+v, %v", got, err)
	}
	if got.Secret != "sekret" || len(got.Events) != 1 || got.Events[0] != "pull_request" {
		t.Fatalf("webhook fields lost: %+v", got)
	}
	if err := s.PutWebhook(&WebhookSubscription{Slug: "pr-watch", Prompt: "v2"}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetWebhook("pr-watch")
	if got.Prompt != "v2" {
		t.Fatalf("upsert did not replace: %+v", got)
	}
	ok, err := s.DeleteWebhook("pr-watch")
	if err != nil || !ok {
		t.Fatalf("DeleteWebhook = %v, %v", ok, err)
	}
	if _, err := s.GetWebhook("pr-watch"); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRejectsEmptyID(t *testing.T) {
	s := newTestStore(t)
	if err := s.PutJob(&Job{}); err == nil {
		t.Fatal("PutJob with empty id should fail")
	}
	if err := s.PutWebhook(&WebhookSubscription{}); err == nil {
		t.Fatal("PutWebhook with empty slug should fail")
	}
}

func TestStoreFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not model POSIX file permissions")
	}
	s := newTestStore(t)
	if err := s.PutJob(sampleJob("job-1")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(s.dir, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("jobs.json perms %v, want owner-only", info.Mode().Perm())
	}
}
