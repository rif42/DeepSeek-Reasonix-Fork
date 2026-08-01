package serve

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/routines"
)

// routinesHub is the lazily-initialized routines service embedded in the serve
// process: the store plus a running scheduler (headless agent runner + local
// delivery). It is created on the first routines API call so `reasonix serve`
// without routines config stays untouched.
type routinesHub struct {
	store  *routines.Store
	sched  *routines.Scheduler
	ctx    context.Context
	cancel context.CancelFunc
}

// routinesHub builds (once) and returns the embedded routines service.
func (s *Server) routinesHub() (*routinesHub, error) {
	s.mu.RLock()
	if s.rh != nil {
		s.mu.RUnlock()
		return s.rh, nil
	}
	s.mu.RUnlock()

	dir := routines.RoutinesDataDir()
	if dir == "" {
		return nil, errors.New("reasonix home directory unavailable (set REASONIX_HOME)")
	}
	store, err := routines.NewStore(dir)
	if err != nil {
		return nil, err
	}
	cfg, _ := config.Load()
	defaultModel := strings.TrimSpace(cfg.Routines.Model)
	if defaultModel == "" {
		defaultModel = strings.TrimSpace(cfg.DefaultModel)
	}
	wd, _ := os.Getwd()
	runner := &routines.AgentRunner{
		WorkspaceRoot: wd,
		SessionDir:    filepath.Join(dir, "sessions"),
	}
	router := &routines.DeliveryRouter{LocalDir: dir}
	maxParallel := cfg.Routines.MaxParallelJobs
	if maxParallel <= 0 {
		maxParallel = 4
	}
	ctx, cancel := context.WithCancel(context.Background())
	sched := routines.NewScheduler(routines.SchedulerOptions{
		Store:         store,
		Runner:        runner,
		Deliverer:     router,
		DefaultModel:  defaultModel,
		MaxParallel:   maxParallel,
		OutputDir:     dir,
		WorkspaceRoot: wd,
	})
	sched.Start(ctx)
	hub := &routinesHub{store: store, sched: sched, ctx: ctx, cancel: cancel}

	s.mu.Lock()
	if s.rh != nil { // lost a race; stop the duplicate
		s.mu.Unlock()
		cancel()
		sched.Stop()
		return s.rh, nil
	}
	s.rh = hub
	s.mu.Unlock()
	return hub, nil
}

// webhookView is the browser-facing shape of a subscription (secret redacted).
type webhookView struct {
	Slug        string    `json:"slug"`
	Description string    `json:"description,omitempty"`
	Events      []string  `json:"events"`
	Prompt      string    `json:"prompt"`
	Deliver     string    `json:"deliver"`
	DeliverOnly bool      `json:"deliver_only"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type routinesIndexResp struct {
	Jobs     []*routines.Job `json:"jobs"`
	Webhooks []webhookView   `json:"webhooks"`
}

// routinesIndex returns all jobs and subscriptions (redacted).
func (s *Server) routinesIndex(w http.ResponseWriter, r *http.Request) {
	hub, err := s.routinesHub()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	jobs, err := hub.store.ListJobs()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	subs, err := hub.store.ListWebhooks()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	views := make([]webhookView, 0, len(subs))
	for _, sub := range subs {
		views = append(views, webhookView{
			Slug:        sub.Slug,
			Description: sub.Description,
			Events:      sub.Events,
			Prompt:      sub.Prompt,
			Deliver:     sub.Deliver,
			DeliverOnly: sub.DeliverOnly,
			CreatedAt:   sub.CreatedAt,
			UpdatedAt:   sub.UpdatedAt,
		})
	}
	writeJSON(w, routinesIndexResp{Jobs: jobs, Webhooks: views})
}

type routinesCreateJobReq struct {
	Schedule string `json:"schedule"`
	Prompt   string `json:"prompt"`
	Name     string `json:"name"`
	Model    string `json:"model"`
	Deliver  string `json:"deliver"`
	Script   string `json:"script"`
	NoAgent  bool   `json:"no_agent"`
	Repeat   int    `json:"repeat"`
	Workdir  string `json:"workdir"`
}

// routinesCreateJob validates and persists a new scheduled job.
func (s *Server) routinesCreateJob(w http.ResponseWriter, r *http.Request) {
	hub, err := s.routinesHub()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var req routinesCreateJobReq
	if err := decodeJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Schedule = strings.TrimSpace(req.Schedule)
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Schedule == "" {
		writeJSONError(w, http.StatusBadRequest, "schedule is required")
		return
	}
	if req.Prompt == "" && !(req.NoAgent && req.Script != "") {
		writeJSONError(w, http.StatusBadRequest, "prompt is required (unless no_agent with a script)")
		return
	}
	now := time.Now()
	sched, err := routines.ParseSchedule(req.Schedule, now)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	next := sched.NextRun(now)
	if next == nil {
		writeJSONError(w, http.StatusBadRequest, "schedule has no future run time")
		return
	}
	job := &routines.Job{
		ID:        newServeJobID(),
		Name:      strings.TrimSpace(req.Name),
		Prompt:    req.Prompt,
		Model:     strings.TrimSpace(req.Model),
		Script:    strings.TrimSpace(req.Script),
		NoAgent:   req.NoAgent,
		Schedule:  sched,
		Repeat:    routines.Repeat{Times: req.Repeat},
		Enabled:   true,
		State:     routines.JobStateIdle,
		Deliver:   strings.TrimSpace(req.Deliver),
		Workdir:   strings.TrimSpace(req.Workdir),
		NextRunAt: next,
		CreatedAt: now.UTC(),
		UpdatedAt: now.UTC(),
	}
	if job.Name == "" {
		job.Name = job.ID
	}
	if err := hub.store.PutJob(job); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, job)
}

// routinesJobAction pauses / resumes / removes / runs a job by id.
func (s *Server) routinesJobAction(w http.ResponseWriter, r *http.Request, action string) {
	hub, err := s.routinesHub()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "missing job id")
		return
	}
	now := time.Now()
	switch action {
	case "remove":
		ok, err := hub.store.DeleteJob(id)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !ok {
			writeJSONError(w, http.StatusNotFound, "no such job")
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	case "pause":
		job, err := hub.store.UpdateJob(id, func(j *routines.Job) error {
			j.State = routines.JobStatePaused
			t := now.UTC()
			j.PausedAt = &t
			j.PausedReason = "serve"
			return nil
		})
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if job == nil {
			writeJSONError(w, http.StatusNotFound, "no such job")
			return
		}
		writeJSON(w, job)
	case "resume":
		job, err := hub.store.UpdateJob(id, func(j *routines.Job) error {
			j.State = routines.JobStateIdle
			j.PausedAt = nil
			j.PausedReason = ""
			return nil
		})
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if job == nil {
			writeJSONError(w, http.StatusNotFound, "no such job")
			return
		}
		writeJSON(w, job)
	case "run":
		job, err := hub.store.UpdateJob(id, func(j *routines.Job) error {
			j.Enabled = true
			j.State = routines.JobStateIdle
			next := time.Now()
			j.NextRunAt = &next
			return nil
		})
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if job == nil {
			writeJSONError(w, http.StatusNotFound, "no such job")
			return
		}
		hub.sched.TickOnce(time.Now())
		writeJSON(w, map[string]any{"ok": true, "dispatched": job.ID})
	default:
		writeJSONError(w, http.StatusBadRequest, "unknown action")
	}
}

type routinesCreateWebhookReq struct {
	Slug        string   `json:"slug"`
	Description string   `json:"description"`
	Events      []string `json:"events"`
	Secret      string   `json:"secret"`
	Prompt      string   `json:"prompt"`
	Deliver     string   `json:"deliver"`
	DeliverOnly bool     `json:"deliver_only"`
}

// routinesCreateWebhook subscribes a route. An empty secret is auto-generated
// and returned once (the store keeps it for verification).
func (s *Server) routinesCreateWebhook(w http.ResponseWriter, r *http.Request) {
	hub, err := s.routinesHub()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var req routinesCreateWebhookReq
	if err := decodeJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Slug = strings.TrimSpace(req.Slug)
	if req.Slug == "" || strings.ContainsAny(req.Slug, "/ \t") {
		writeJSONError(w, http.StatusBadRequest, "invalid slug")
		return
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" && !req.DeliverOnly {
		writeJSONError(w, http.StatusBadRequest, "prompt is required (unless deliver_only)")
		return
	}
	secret := strings.TrimSpace(req.Secret)
	generated := false
	if secret == "" {
		secret = newServeSecret()
		generated = true
	}
	now := time.Now().UTC()
	sub := &routines.WebhookSubscription{
		Slug:        req.Slug,
		Description: strings.TrimSpace(req.Description),
		Events:      cleanStringList(req.Events),
		Secret:      secret,
		Prompt:      req.Prompt,
		Deliver:     strings.TrimSpace(req.Deliver),
		DeliverOnly: req.DeliverOnly,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := hub.store.PutWebhook(sub); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	view := webhookView{
		Slug:        sub.Slug,
		Description: sub.Description,
		Events:      sub.Events,
		Prompt:      sub.Prompt,
		Deliver:     sub.Deliver,
		DeliverOnly: sub.DeliverOnly,
		CreatedAt:   sub.CreatedAt,
		UpdatedAt:   sub.UpdatedAt,
	}
	writeJSON(w, map[string]any{
		"webhook":   view,
		"secret":    secret, // shown exactly once
		"generated": generated,
	})
}

// routinesWebhookRemove deletes a subscription by slug.
func (s *Server) routinesWebhookRemove(w http.ResponseWriter, r *http.Request) {
	hub, err := s.routinesHub()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	slug := r.PathValue("slug")
	if slug == "" {
		writeJSONError(w, http.StatusBadRequest, "missing slug")
		return
	}
	ok, err := hub.store.DeleteWebhook(slug)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeJSONError(w, http.StatusNotFound, "no such webhook")
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// routinesJobPause / Resume / Remove / Run dispatch to the shared action
// handler with the path-derived action.
func (s *Server) routinesJobPause(w http.ResponseWriter, r *http.Request) {
	s.routinesJobAction(w, r, "pause")
}
func (s *Server) routinesJobResume(w http.ResponseWriter, r *http.Request) {
	s.routinesJobAction(w, r, "resume")
}
func (s *Server) routinesJobRemove(w http.ResponseWriter, r *http.Request) {
	s.routinesJobAction(w, r, "remove")
}
func (s *Server) routinesJobRun(w http.ResponseWriter, r *http.Request) {
	s.routinesJobAction(w, r, "run")
}

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	return dec.Decode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func cleanStringList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func newServeJobID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("job-%d", time.Now().UnixNano())
	}
	return "job-" + hex.EncodeToString(b)
}

func newServeSecret() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("secret-%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
