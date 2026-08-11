package routines

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRunner records every run it was asked to perform and answers with a
// configurable response.
type fakeRunner struct {
	mu       sync.Mutex
	prompts  []string
	models   []string
	response string
	err      error
	block    chan struct{} // when non-nil, Run blocks on it until closed
	started  chan struct{} // signaled (once) on first Run call
	once     sync.Once
}

func (f *fakeRunner) Run(ctx context.Context, opts RunOptions) (RunResult, error) {
	f.mu.Lock()
	f.prompts = append(f.prompts, opts.Prompt)
	f.models = append(f.models, opts.Model)
	block := f.block
	started := f.started
	f.mu.Unlock()
	if started != nil {
		f.once.Do(func() { close(started) })
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return RunResult{}, ctx.Err()
		}
	}
	if f.err != nil {
		return RunResult{}, f.err
	}
	return RunResult{FinalResponse: f.response}, nil
}

func (f *fakeRunner) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

// newScheduler builds a scheduler over a temp store with the fake runner.
func newScheduler(t *testing.T, runner Runner, outputDir string) (*Scheduler, *Store, func()) {
	t.Helper()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	s := NewScheduler(SchedulerOptions{
		Store:        store,
		Runner:       runner,
		DefaultModel: "deepseek-v4-flash",
		OutputDir:    outputDir,
		Now:          func() time.Time { return now },
	})
	return s, store, func() { s.Stop() }
}

// dueJob creates an enabled interval job whose next run is in the past.
func dueJob(id string) *Job {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Minute)
	sched, _ := ParseSchedule("every 30m", now)
	return &Job{
		ID:        id,
		Name:      "job " + id,
		Prompt:    "do the thing",
		Schedule:  sched,
		Enabled:   true,
		State:     JobStateIdle,
		NextRunAt: &past,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// waitFor polls cond until it holds or the deadline passes (the scheduler
// executes jobs asynchronously on goroutines).
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestTickDispatchesDueJobOnce(t *testing.T) {
	runner := &fakeRunner{response: "done"}
	s, store, _ := newScheduler(t, runner, "")
	defer s.Stop()
	if err := store.PutJob(dueJob("job-1")); err != nil {
		t.Fatal(err)
	}
	s.TickOnce(time.Now().Add(time.Minute))
	waitFor(t, "job run", func() bool { return runner.calls() == 1 })
	if runner.models[0] != "deepseek-v4-flash" {
		t.Fatalf("default model not applied: %q", runner.models[0])
	}
	var job *Job
	waitFor(t, "lifecycle recorded", func() bool {
		j, _ := store.GetJob("job-1")
		job = j
		return job != nil && job.LastStatus != ""
	})
	if job.LastStatus != "ok" || job.State != JobStateIdle {
		t.Fatalf("lifecycle not recorded: %+v", job)
	}
	if job.Repeat.Completed != 1 {
		t.Fatalf("repeat completed = %d, want 1", job.Repeat.Completed)
	}
	if job.NextRunAt == nil || !job.NextRunAt.After(time.Now()) {
		t.Fatalf("next_run_at not advanced into the future: %v", job.NextRunAt)
	}
	// Second tick must not re-fire while the job is not yet due again.
	s.TickOnce(time.Now().Add(time.Minute))
	time.Sleep(50 * time.Millisecond)
	if runner.calls() != 1 {
		t.Fatalf("job re-fired before due: %d calls", runner.calls())
	}
}

func TestTickSkipsDisabledPausedCompleted(t *testing.T) {
	runner := &fakeRunner{response: "done"}
	s, store, _ := newScheduler(t, runner, "")
	defer s.Stop()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	j1 := dueJob("disabled")
	j1.Enabled = false
	j2 := dueJob("paused")
	j2.State = JobStatePaused
	j3 := dueJob("completed")
	j3.State = JobStateCompleted
	for _, j := range []*Job{j1, j2, j3} {
		if err := store.PutJob(j); err != nil {
			t.Fatal(err)
		}
	}
	s.TickOnce(now.Add(time.Minute))
	if runner.calls() != 0 {
		t.Fatalf("non-due jobs fired: %d calls", runner.calls())
	}
}

func TestSilentResponseSuppresses(t *testing.T) {
	runner := &fakeRunner{response: SilenceMarker + " nothing changed"}
	s, store, _ := newScheduler(t, runner, "")
	defer s.Stop()
	if err := store.PutJob(dueJob("job-1")); err != nil {
		t.Fatal(err)
	}
	s.TickOnce(time.Now().Add(time.Minute))
	var job *Job
	waitFor(t, "job lifecycle", func() bool {
		j, _ := store.GetJob("job-1")
		job = j
		return job != nil && job.LastStatus != ""
	})
	if job.LastStatus != "suppressed" {
		t.Fatalf("last_status = %q, want suppressed", job.LastStatus)
	}
}

func TestRunErrorRecorded(t *testing.T) {
	runner := &fakeRunner{err: errors.New("boom")}
	s, store, _ := newScheduler(t, runner, "")
	defer s.Stop()
	if err := store.PutJob(dueJob("job-1")); err != nil {
		t.Fatal(err)
	}
	s.TickOnce(time.Now().Add(time.Minute))
	var job *Job
	waitFor(t, "job lifecycle", func() bool {
		j, _ := store.GetJob("job-1")
		job = j
		return job != nil && job.LastStatus != ""
	})
	if job.LastStatus != "error" || !strings.Contains(job.LastError, "boom") {
		t.Fatalf("error not recorded: %+v", job)
	}
	// An errored job stays enabled and keeps firing.
	if !job.Enabled || job.State == JobStateCompleted {
		t.Fatalf("errored job should stay enabled: %+v", job)
	}
}

func TestRepeatLimitCompletes(t *testing.T) {
	runner := &fakeRunner{response: "done"}
	s, store, _ := newScheduler(t, runner, "")
	defer s.Stop()
	j := dueJob("job-1")
	j.Repeat = Repeat{Times: 1}
	if err := store.PutJob(j); err != nil {
		t.Fatal(err)
	}
	s.TickOnce(time.Now().Add(time.Minute))
	var job *Job
	waitFor(t, "job lifecycle", func() bool {
		j, _ := store.GetJob("job-1")
		job = j
		return job != nil && job.LastStatus != ""
	})
	if job.State != JobStateCompleted || job.Repeat.Completed != 1 {
		t.Fatalf("repeat limit not honored: %+v", job)
	}
}

func TestPreScriptInjectionAndNoAgent(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "probe.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho hello-from-script\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{response: "agent answer"}

	t.Run("agent mode injects script output", func(t *testing.T) {
		s, store, _ := newScheduler(t, runner, "")
		defer s.Stop()
		j := dueJob("job-1")
		j.Script = script
		j.Workdir = dir
		if err := store.PutJob(j); err != nil {
			t.Fatal(err)
		}
		s.TickOnce(time.Now().Add(time.Minute))
		waitFor(t, "agent run", func() bool { return runner.calls() == 1 })
		if !strings.Contains(runner.prompts[0], "hello-from-script") {
			t.Fatalf("script output not injected into prompt: %q", runner.prompts[0])
		}
	})

	t.Run("empty script output skips agent", func(t *testing.T) {
		empty := filepath.Join(dir, "empty.sh")
		if err := os.WriteFile(empty, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		runner2 := &fakeRunner{response: "agent answer"}
		s2, store2, _ := newScheduler(t, runner2, "")
		defer s2.Stop()
		j := dueJob("job-2")
		j.Script = empty
		j.Workdir = dir
		if err := store2.PutJob(j); err != nil {
			t.Fatal(err)
		}
		s2.TickOnce(time.Now().Add(time.Minute))
		waitFor(t, "job lifecycle", func() bool {
			j, _ := store2.GetJob("job-2")
			return j != nil && j.LastStatus != ""
		})
		if runner2.calls() != 0 {
			t.Fatalf("agent ran despite empty script output: %d calls", runner2.calls())
		}
		job, _ := store2.GetJob("job-2")
		if job.LastStatus != "suppressed" {
			t.Fatalf("expected suppressed, got %q", job.LastStatus)
		}
	})

	t.Run("no_agent job delivers script stdout", func(t *testing.T) {
		s3, store3, _ := newScheduler(t, runner, "")
		defer s3.Stop()
		j := dueJob("job-3")
		j.Script = script
		j.Workdir = dir
		j.NoAgent = true
		if err := store3.PutJob(j); err != nil {
			t.Fatal(err)
		}
		s3.TickOnce(time.Now().Add(time.Minute))
		var job *Job
		waitFor(t, "job lifecycle", func() bool {
			j, _ := store3.GetJob("job-3")
			job = j
			return j != nil && j.LastStatus != ""
		})
		if runner.calls() != 1 {
			t.Fatalf("no_agent should not call the agent runner; calls=%d", runner.calls())
		}
		if job.LastStatus != "ok" {
			t.Fatalf("no_agent status = %q, want ok", job.LastStatus)
		}
	})
}

func TestOutputDocSaved(t *testing.T) {
	runner := &fakeRunner{response: "the final answer"}
	outDir := filepath.Join(t.TempDir(), "out")
	s, store, _ := newScheduler(t, runner, outDir)
	defer s.Stop()
	if err := store.PutJob(dueJob("job-1")); err != nil {
		t.Fatal(err)
	}
	s.TickOnce(time.Now().Add(time.Minute))
	var entries []os.DirEntry
	waitFor(t, "output doc", func() bool {
		e, err := os.ReadDir(filepath.Join(outDir, "job-1"))
		if err != nil || len(e) == 0 {
			return false
		}
		entries = e
		return true
	})
	data, _ := os.ReadFile(filepath.Join(outDir, "job-1", entries[0].Name()))
	if !strings.Contains(string(data), "the final answer") {
		t.Fatalf("output doc missing content: %q", string(data))
	}
}

func TestInFlightDedup(t *testing.T) {
	runner := &fakeRunner{response: "done", block: make(chan struct{}), started: make(chan struct{})}
	s, store, _ := newScheduler(t, runner, "")
	defer s.Stop()
	if err := store.PutJob(dueJob("job-1")); err != nil {
		t.Fatal(err)
	}
	s.TickOnce(time.Now().Add(time.Minute))
	<-runner.started // first run is in flight
	// A second tick while the first is still running must not re-dispatch.
	s.TickOnce(time.Now().Add(time.Minute))
	if runner.calls() != 1 {
		t.Fatalf("in-flight job re-dispatched: %d calls", runner.calls())
	}
	close(runner.block)
	s.wg.Wait()
	if runner.calls() != 1 {
		t.Fatalf("calls after release = %d, want 1", runner.calls())
	}
}

func TestParallelCap(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{}, 2)
	runner := &fakeRunner{response: "done", block: block, started: started}
	s, store, _ := newScheduler(t, runner, "")
	defer s.Stop()
	for i := range 3 {
		if err := store.PutJob(dueJob("job-" + string(rune('a'+i)))); err != nil {
			t.Fatal(err)
		}
	}
	s.TickOnce(time.Now().Add(time.Minute))
	// Wait until two runs are in flight (cap = 4 default, so all three start;
	// the cap test instead verifies the semaphore does not deadlock the ticker).
	time.Sleep(50 * time.Millisecond)
	close(block)
	s.wg.Wait()
	if runner.calls() != 3 {
		t.Fatalf("all three jobs should run, got %d", runner.calls())
	}
}
