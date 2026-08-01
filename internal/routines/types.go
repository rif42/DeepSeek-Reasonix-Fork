// Package routines implements scheduled jobs ("routines") and webhook
// subscriptions for Reasonix: cron/interval/one-shot schedules, a headless
// agent run path, delivery routing, and an HMAC-protected webhook receiver.
//
// The design follows Hermes' automations subsystem: jobs persist as JSON and
// are fired by an in-process ticker; each job runs the agent headlessly with a
// one-shot prompt and the captured final response is routed to a delivery
// target (local file or a bot platform chat). Webhooks are a separate lane
// that renders event payloads into prompts.
package routines

import "time"

// ScheduleKind discriminates the three schedule shapes.
type ScheduleKind string

const (
	// ScheduleOnce fires exactly once at RunAt (or immediately if already due).
	ScheduleOnce ScheduleKind = "once"
	// ScheduleInterval fires every Minutes minutes.
	ScheduleInterval ScheduleKind = "interval"
	// ScheduleCron fires on a 5-field cron expression.
	ScheduleCron ScheduleKind = "cron"
)

// Schedule describes when a job fires.
type Schedule struct {
	Kind    ScheduleKind `json:"kind"`
	RunAt   *time.Time   `json:"run_at,omitempty"`  // once
	Minutes int          `json:"minutes,omitempty"` // interval
	Expr    string       `json:"expr,omitempty"`    // cron
	Display string       `json:"display,omitempty"` // human-readable original input

	cron *cron `json:"-"` // parsed cron expression (runtime only)
}

// Repeat bounds how many times a job may run. Times == 0 means unlimited.
type Repeat struct {
	Times     int `json:"times,omitempty"`
	Completed int `json:"completed,omitempty"`
}

// Origin records where a job was created from, used to route delivery back to
// the originating chat when Deliver == "origin".
type Origin struct {
	Platform string `json:"platform,omitempty"`
	ChatID   string `json:"chat_id,omitempty"`
	ThreadID string `json:"thread_id,omitempty"`
}

// Job states (State field).
const (
	JobStateIdle      = "idle"      // enabled, waiting for next run
	JobStateRunning   = "running"   // dispatched, in flight
	JobStatePaused    = "paused"    // paused by the user
	JobStateCompleted = "completed" // finished all repeats
	JobStateError     = "error"     // last run failed
)

// Job is one scheduled routine. Zero-value fields use JSON omitempty so the
// stored file stays readable and diff-friendly.
type Job struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	Prompt            string     `json:"prompt"`
	Model             string     `json:"model,omitempty"`
	Script            string     `json:"script,omitempty"`   // pre-run script path (stdout becomes prompt context)
	NoAgent           bool       `json:"no_agent,omitempty"` // script IS the job; stdout delivered verbatim
	Skills            []string   `json:"skills,omitempty"`
	Schedule          Schedule   `json:"schedule"`
	Repeat            Repeat     `json:"repeat"`
	Enabled           bool       `json:"enabled"`
	State             string     `json:"state"`
	Deliver           string     `json:"deliver,omitempty"` // local|origin|platform|platform:chat[:thread]
	Workdir           string     `json:"workdir,omitempty"`
	Origin            Origin     `json:"origin,omitempty"`
	PausedAt          *time.Time `json:"paused_at,omitempty"`
	PausedReason      string     `json:"paused_reason,omitempty"`
	NextRunAt         *time.Time `json:"next_run_at,omitempty"`
	LastRunAt         *time.Time `json:"last_run_at,omitempty"`
	LastStatus        string     `json:"last_status,omitempty"` // ok|error|suppressed
	LastError         string     `json:"last_error,omitempty"`
	LastDeliveryError string     `json:"last_delivery_error,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`

	// ExecutionID is runtime-only state (the in-flight run's id); it is not
	// persisted meaningfully but survives store round-trips as a no-op.
	ExecutionID string `json:"execution_id,omitempty"`
}

// Clone returns a deep copy safe for mutation by the scheduler.
func (j *Job) Clone() *Job {
	c := *j
	c.Skills = append([]string(nil), j.Skills...)
	if j.Schedule.RunAt != nil {
		t := *j.Schedule.RunAt
		c.Schedule.RunAt = &t
	}
	if j.NextRunAt != nil {
		t := *j.NextRunAt
		c.NextRunAt = &t
	}
	if j.LastRunAt != nil {
		t := *j.LastRunAt
		c.LastRunAt = &t
	}
	if j.PausedAt != nil {
		t := *j.PausedAt
		c.PausedAt = &t
	}
	return &c
}

// WebhookSubscription is one HMAC-protected webhook route. Events empty means
// any event triggers it.
type WebhookSubscription struct {
	Slug        string    `json:"slug"`
	Description string    `json:"description,omitempty"`
	Events      []string  `json:"events,omitempty"`
	Secret      string    `json:"secret,omitempty"`
	Prompt      string    `json:"prompt"`
	Skills      []string  `json:"skills,omitempty"`
	Deliver     string    `json:"deliver,omitempty"`      // empty = "log" (deliver_only default)
	DeliverOnly bool      `json:"deliver_only,omitempty"` // no agent; deliver payload directly
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Clone returns a deep copy of the subscription.
func (w *WebhookSubscription) Clone() *WebhookSubscription {
	c := *w
	c.Events = append([]string(nil), w.Events...)
	c.Skills = append([]string(nil), w.Skills...)
	return &c
}
