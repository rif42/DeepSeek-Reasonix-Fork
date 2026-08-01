package routines

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// errJobSkip aborts a claim/update without persisting anything: another
// process (or a stale snapshot) already claimed the job.
var errJobSkip = errors.New("routines: job skipped (already claimed or not due)")

// RunResult is the outcome of one headless agent run.
type RunResult struct {
	// FinalResponse is the agent's final answer text (or script stdout for
	// no_agent jobs) — the text that gets delivered.
	FinalResponse string
}

// Runner executes one job's agent run. The real implementation is AgentRunner
// (runner.go); tests inject a fake.
type Runner interface {
	Run(ctx context.Context, opts RunOptions) (RunResult, error)
}

// RunOptions carries what the runner needs for one job run.
type RunOptions struct {
	JobID  string // job id, for session naming and diagnostics
	Prompt string // final prompt (after script injection)
	Model  string // resolved model name (job.Model or the service default)
}

// Deliverer routes a completed job's content to its delivery target.
// Phase 3's DeliveryRouter implements it; nil disables delivery.
type Deliverer interface {
	Deliver(ctx context.Context, job *Job, content string) error
}

// SchedulerOptions configures the scheduler.
type SchedulerOptions struct {
	Store         *Store
	Runner        Runner
	Deliverer     Deliverer
	DefaultModel  string        // fallback when a job has no explicit model
	MaxParallel   int           // concurrent job cap (default 4)
	TickInterval  time.Duration // default 60s
	JobTimeout    time.Duration // per-job run timeout (default 10m)
	OutputDir     string        // where output docs are saved ("" = disabled)
	WorkspaceRoot string        // project root for routine agent runs
	Now           func() time.Time
	Logger        func(format string, args ...any)
}

// Scheduler fires due jobs on a ticker with at-most-once semantics and runs
// them concurrently under a parallel cap. It mirrors Hermes' cron ticker:
// due jobs have their next_run_at advanced *before* dispatch, so a crash or
// restart never double-fires a job.
type Scheduler struct {
	store        *Store
	runner       Runner
	deliverer    Deliverer
	defaultModel string
	sem          chan struct{}
	tickInterval time.Duration
	jobTimeout   time.Duration
	outputDir    string
	workspace    string
	now          func() time.Time
	logf         func(format string, args ...any)

	mu       sync.Mutex
	inflight map[string]bool
	stopCh   chan struct{}
	done     chan struct{}
	wg       sync.WaitGroup
	started  bool
}

// NewScheduler builds a scheduler with defaults applied.
func NewScheduler(opts SchedulerOptions) *Scheduler {
	if opts.MaxParallel <= 0 {
		opts.MaxParallel = 4
	}
	if opts.TickInterval <= 0 {
		opts.TickInterval = 60 * time.Second
	}
	if opts.JobTimeout <= 0 {
		opts.JobTimeout = 10 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = func(string, ...any) {}
	}
	return &Scheduler{
		store:        opts.Store,
		runner:       opts.Runner,
		deliverer:    opts.Deliverer,
		defaultModel: opts.DefaultModel,
		sem:          make(chan struct{}, opts.MaxParallel),
		tickInterval: opts.TickInterval,
		jobTimeout:   opts.JobTimeout,
		outputDir:    opts.OutputDir,
		workspace:    opts.WorkspaceRoot,
		now:          opts.Now,
		logf:         opts.Logger,
		inflight:     map[string]bool{},
		stopCh:       make(chan struct{}),
		done:         make(chan struct{}),
	}
}

// Start launches the ticker loop. It returns immediately; the loop runs until
// the context is cancelled or Stop is called.
func (s *Scheduler) Start(ctx context.Context) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()
	go s.runLoop(ctx)
}

// Stop halts the ticker and waits for in-flight jobs to finish. When the
// scheduler was never started (manual TickOnce driving, tests), it only waits
// for in-flight work.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		s.wg.Wait()
		return
	}
	select {
	case <-s.stopCh:
		s.mu.Unlock()
		return
	default:
		close(s.stopCh)
	}
	s.mu.Unlock()
	<-s.done
	s.wg.Wait()
}

// Stopped reports whether Stop has been called.
func (s *Scheduler) Stopped() bool {
	select {
	case <-s.stopCh:
		return true
	default:
		return false
	}
}

func (s *Scheduler) runLoop(ctx context.Context) {
	defer close(s.done)
	ticker := time.NewTicker(s.tickInterval)
	defer ticker.Stop()
	// Fire immediately on start so a job created while stopped runs promptly.
	s.TickOnce(s.now())
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.TickOnce(s.now())
		}
	}
}

// TickOnce scans for due jobs and dispatches them. Exported so the CLI
// `routines cron tick` command and tests can drive a tick synchronously.
func (s *Scheduler) TickOnce(now time.Time) {
	if s.Stopped() {
		return
	}
	jobs, err := s.store.ListJobs()
	if err != nil {
		s.logf("routines: list jobs: %v", err)
		return
	}
	for _, j := range jobs {
		if !s.due(j, now) {
			continue
		}
		claimed, err := s.claim(j, now)
		if err != nil {
			if !errors.Is(err, errJobSkip) {
				s.logf("routines: claim job %s: %v", j.ID, err)
			}
			continue
		}
		if claimed != nil {
			s.launch(claimed)
		}
	}
}

// due reports whether the job should be considered this tick. An errored job
// keeps firing (LastStatus records the failure); paused/running/completed and
// disabled jobs do not.
func (s *Scheduler) due(j *Job, now time.Time) bool {
	if j == nil || !j.Enabled || j.NextRunAt == nil {
		return false
	}
	switch j.State {
	case JobStateRunning, JobStatePaused, JobStateCompleted:
		return false
	}
	return !j.NextRunAt.After(now)
}

// claim atomically marks the job running and advances its next run *before*
// dispatch (at-most-once). It returns the claimed clone, or nil when the job
// was already claimed elsewhere or is no longer due.
func (s *Scheduler) claim(j *Job, now time.Time) (*Job, error) {
	var claimed *Job
	_, err := s.store.UpdateJob(j.ID, func(cur *Job) error {
		if !s.due(cur, now) {
			return errJobSkip
		}
		cur.NextRunAt = advanceNextRun(cur.Schedule, cur.NextRunAt, now)
		cur.State = JobStateRunning
		cur.ExecutionID = newExecutionID()
		claimed = cur.Clone()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// advanceNextRun moves next forward to the first future slot. Stale schedules
// fast-forward so a scheduler that was down for hours fires each job only
// once instead of burst-firing every missed slot.
func advanceNextRun(sched Schedule, prev *time.Time, now time.Time) *time.Time {
	if prev == nil {
		return nil
	}
	next := sched.NextRun(*prev)
	for next != nil && !next.After(now) {
		next = sched.NextRun(*next)
	}
	return next
}

func (s *Scheduler) launch(j *Job) {
	s.mu.Lock()
	if s.inflight[j.ID] {
		s.mu.Unlock()
		return
	}
	s.inflight[j.ID] = true
	s.wg.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.inflight, j.ID)
			s.mu.Unlock()
		}()
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		case <-s.stopCh:
			return
		}
		s.execute(j)
	}()
}

// execute runs one claimed job end-to-end: pre-script (optional), agent run,
// output save, delivery, then lifecycle update.
func (s *Scheduler) execute(j *Job) {
	runCtx, cancel := context.WithTimeout(context.Background(), s.jobTimeout)
	defer cancel()

	var result RunResult
	var runErr error

	switch {
	case j.NoAgent && j.Script != "":
		// The script IS the job; its stdout is delivered verbatim.
		out, err := RunScript(runCtx, j.Script, j.Workdir)
		if err != nil {
			runErr = fmt.Errorf("job script: %w", err)
			break
		}
		if strings.TrimSpace(out) == "" {
			s.finish(j, "suppressed", "", nil, nil)
			return
		}
		result = RunResult{FinalResponse: out}
	default:
		prompt := j.Prompt
		if j.Script != "" {
			out, err := RunScript(runCtx, j.Script, j.Workdir)
			if err != nil {
				runErr = fmt.Errorf("pre-run script: %w", err)
				break
			}
			if strings.TrimSpace(out) == "" {
				// Hermes wake-gate: an empty script output means "nothing to do".
				s.finish(j, "suppressed", "", nil, nil)
				return
			}
			prompt = j.Prompt + "\n\n## Script Output\n```\n" + out + "\n```\n"
		}
		result, runErr = s.runner.Run(runCtx, RunOptions{
			JobID:  j.ID,
			Prompt: prompt,
			Model:  s.resolveModel(j),
		})
	}

	if runErr != nil {
		s.saveOutput(j, runErr.Error())
		s.finish(j, "error", "", runErr, nil)
		return
	}
	if IsSilenceResponse(result.FinalResponse) {
		s.finish(j, "suppressed", result.FinalResponse, nil, nil)
		return
	}
	s.saveOutput(j, result.FinalResponse)
	var deliveryErr error
	if s.deliverer != nil {
		deliveryErr = s.deliverer.Deliver(runCtx, j, result.FinalResponse)
	}
	s.finish(j, "ok", result.FinalResponse, nil, deliveryErr)
}

// resolveModel applies the job → service-default precedence.
func (s *Scheduler) resolveModel(j *Job) string {
	if strings.TrimSpace(j.Model) != "" {
		return j.Model
	}
	return s.defaultModel
}

// finish persists the post-run lifecycle: state, last_* fields, repeat count,
// and repeat-limit completion.
func (s *Scheduler) finish(j *Job, status, output string, runErr, deliveryErr error) {
	now := s.now().UTC()
	lastRunAt := now
	_, err := s.store.UpdateJob(j.ID, func(cur *Job) error {
		cur.LastRunAt = &lastRunAt
		cur.LastStatus = status
		cur.LastError = ""
		cur.LastDeliveryError = ""
		if runErr != nil {
			cur.LastError = runErr.Error()
		}
		if deliveryErr != nil {
			cur.LastDeliveryError = deliveryErr.Error()
		}
		cur.Repeat.Completed++
		cur.State = JobStateIdle
		if cur.Repeat.Times > 0 && cur.Repeat.Completed >= cur.Repeat.Times {
			cur.State = JobStateCompleted
		}
		return nil
	})
	if err != nil {
		s.logf("routines: update job %s after run: %v", j.ID, err)
	}
	_ = output
}

// saveOutput writes the run's output doc to OutputDir/<jobID>/<timestamp>.md.
func (s *Scheduler) saveOutput(j *Job, content string) {
	if s.outputDir == "" {
		return
	}
	dir := filepath.Join(s.outputDir, j.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.logf("routines: output dir %s: %v", dir, err)
		return
	}
	path := filepath.Join(dir, s.now().UTC().Format("20060102-150405.000")+".md")
	doc := fmt.Sprintf("# %s\n\n%s\n", j.Name, content)
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		s.logf("routines: write output %s: %v", path, err)
	}
}

// newExecutionID returns a 16-hex execution id for diagnostics.
func newExecutionID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("exec-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
