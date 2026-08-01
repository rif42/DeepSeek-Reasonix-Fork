package routines

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"reasonix/internal/filelock"
	"reasonix/internal/fileutil"
)

// Store persists jobs and webhook subscriptions as two JSON files in one
// directory, mirroring Hermes' jobs.json / webhook_subscriptions.json layout.
// Every mutation runs under a cross-process advisory lock and writes atomically
// (temp file + rename), so a crash never leaves a truncated store and two
// scheduler processes cannot corrupt each other.
type Store struct {
	dir          string
	jobsPath     string
	webhooksPath string
	mu           sync.Mutex
	now          func() time.Time // clock seam for tests
}

// NewStore returns a Store rooted at dir, creating it if needed. dir is
// typically <reasonix-home>/routines.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("routines: create store dir: %w", err)
	}
	return &Store{
		dir:          dir,
		jobsPath:     filepath.Join(dir, "jobs.json"),
		webhooksPath: filepath.Join(dir, "webhook_subscriptions.json"),
		now:          time.Now,
	}, nil
}

// SetClock overrides the store's clock (test seam).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// JobFile returns the path of the jobs file.
func (s *Store) JobFile() string { return s.jobsPath }

// WebhooksFile returns the path of the webhook subscriptions file.
func (s *Store) WebhooksFile() string { return s.webhooksPath }

// Dir returns the store's root directory.
func (s *Store) Dir() string { return s.dir }

type jobsDoc struct {
	Jobs      []*Job    `json:"jobs"`
	UpdatedAt time.Time `json:"updated_at"`
}

type webhooksDoc struct {
	Webhooks  []*WebhookSubscription `json:"webhook_subscriptions"`
	UpdatedAt time.Time              `json:"updated_at"`
}

// ListJobs returns all jobs, sorted by creation time (stable order).
func (s *Store) ListJobs() ([]*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.readJobsLocked()
	if err != nil {
		return nil, err
	}
	out := make([]*Job, len(doc.Jobs))
	for i, j := range doc.Jobs {
		out[i] = j.Clone()
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// GetJob returns one job by ID, or nil when absent.
func (s *Store) GetJob(id string) (*Job, error) {
	jobs, err := s.ListJobs()
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		if j.ID == id {
			return j, nil
		}
	}
	return nil, nil
}

// PutJob inserts or replaces the job with the same ID. An empty ID is rejected.
func (s *Store) PutJob(job *Job) error {
	if job == nil || job.ID == "" {
		return errors.New("routines: job id is required")
	}
	return s.mutateJobs(func(jobs []*Job) ([]*Job, error) {
		replaced := false
		for i, j := range jobs {
			if j.ID == job.ID {
				jobs[i] = job.Clone()
				replaced = true
				break
			}
		}
		if !replaced {
			jobs = append(jobs, job.Clone())
		}
		return jobs, nil
	})
}

// UpdateJob applies fn to the stored job with the given ID under the store
// lock and persists the result. fn may return the job unchanged to skip a
// write. It returns the updated job, or nil when the ID does not exist.
func (s *Store) UpdateJob(id string, fn func(*Job) error) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.readJobsLocked()
	if err != nil {
		return nil, err
	}
	for i, j := range doc.Jobs {
		if j.ID != id {
			continue
		}
		clone := j.Clone()
		if err := fn(clone); err != nil {
			return nil, err
		}
		clone.UpdatedAt = s.now().UTC()
		doc.Jobs[i] = clone
		if err := s.writeJobsLocked(doc); err != nil {
			return nil, err
		}
		return clone.Clone(), nil
	}
	return nil, nil
}

// DeleteJob removes a job by ID. It reports whether the job existed.
func (s *Store) DeleteJob(id string) (bool, error) {
	return s.mutateJobsBool(func(jobs []*Job) ([]*Job, bool, error) {
		for i, j := range jobs {
			if j.ID == id {
				return append(jobs[:i], jobs[i+1:]...), true, nil
			}
		}
		return jobs, false, nil
	})
}

// ListWebhooks returns all webhook subscriptions.
func (s *Store) ListWebhooks() ([]*WebhookSubscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.readWebhooksLocked()
	if err != nil {
		return nil, err
	}
	out := make([]*WebhookSubscription, len(doc.Webhooks))
	for i, w := range doc.Webhooks {
		out[i] = w.Clone()
	}
	return out, nil
}

// GetWebhook returns one subscription by slug, or nil when absent.
func (s *Store) GetWebhook(slug string) (*WebhookSubscription, error) {
	ws, err := s.ListWebhooks()
	if err != nil {
		return nil, err
	}
	for _, w := range ws {
		if w.Slug == slug {
			return w, nil
		}
	}
	return nil, nil
}

// PutWebhook inserts or replaces the subscription with the same slug.
func (s *Store) PutWebhook(w *WebhookSubscription) error {
	if w == nil || w.Slug == "" {
		return errors.New("routines: webhook slug is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.readWebhooksLocked()
	if err != nil {
		return err
	}
	replaced := false
	for i, existing := range doc.Webhooks {
		if existing.Slug == w.Slug {
			doc.Webhooks[i] = w.Clone()
			replaced = true
			break
		}
	}
	if !replaced {
		doc.Webhooks = append(doc.Webhooks, w.Clone())
	}
	return s.writeWebhooksLocked(doc)
}

// DeleteWebhook removes a subscription by slug. It reports whether it existed.
func (s *Store) DeleteWebhook(slug string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.readWebhooksLocked()
	if err != nil {
		return false, err
	}
	found := false
	out := doc.Webhooks[:0]
	for _, w := range doc.Webhooks {
		if w.Slug == slug {
			found = true
			continue
		}
		out = append(out, w)
	}
	if !found {
		return false, nil
	}
	doc.Webhooks = out
	return true, s.writeWebhooksLocked(doc)
}

func (s *Store) readJobsLocked() (jobsDoc, error) {
	return readDoc[jobsDoc](s.jobsPath)
}

func (s *Store) readWebhooksLocked() (webhooksDoc, error) {
	return readDoc[webhooksDoc](s.webhooksPath)
}

func readDoc[T any](path string) (T, error) {
	var doc T
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return doc, nil
	}
	if err != nil {
		return doc, fmt.Errorf("routines: read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return doc, fmt.Errorf("routines: parse %s: %w", path, err)
	}
	return doc, nil
}

func (s *Store) writeJobsLocked(doc jobsDoc) error {
	doc.UpdatedAt = s.now().UTC()
	return writeDoc(s.jobsPath, doc)
}

func (s *Store) writeWebhooksLocked(doc webhooksDoc) error {
	doc.UpdatedAt = s.now().UTC()
	return writeDoc(s.webhooksPath, doc)
}

func writeDoc(path string, doc any) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("routines: encode %s: %w", path, err)
	}
	data = append(data, '\n')
	if err := fileutil.AtomicWriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("routines: write %s: %w", path, err)
	}
	return nil
}

// mutateJobs reads, transforms, and persists the jobs list under the store
// mutex AND a cross-process file lock. fn returns the new list; an error
// aborts the write.
func (s *Store) mutateJobs(fn func([]*Job) ([]*Job, error)) error {
	_, err := s.mutateJobsBool(func(jobs []*Job) ([]*Job, bool, error) {
		out, err := fn(jobs)
		if err != nil {
			return nil, false, err
		}
		return out, true, nil
	})
	return err
}

func (s *Store) mutateJobsBool(fn func([]*Job) ([]*Job, bool, error)) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	release, err := filelock.Acquire(context.Background(), filepath.Join(s.dir, ".jobs.lock"))
	if err != nil {
		return false, fmt.Errorf("routines: lock jobs store: %w", err)
	}
	defer release()
	doc, err := s.readJobsLocked()
	if err != nil {
		return false, err
	}
	out, changed, err := fn(doc.Jobs)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	doc.Jobs = out
	if err := s.writeJobsLocked(doc); err != nil {
		return false, err
	}
	return true, nil
}
