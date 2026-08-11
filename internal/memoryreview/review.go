// Package memoryreview implements the background memory self-improvement loop
// ported from Hermes: after tool-heavy turns, a detached LLM pass reviews the
// session transcript and distills durable facts into the existing auto-memory
// store (memory.Store), deduplicated and revision-bumped.
//
// Cache-safety contract: the review runs through the standard headless boot
// path (routines.AgentRunner -> boot.Build), so its LLM call shares the
// byte-identical system-prompt prefix with interactive sessions (prompt-cache
// warmth) and it never rebuilds any live session's system prompt. Same-session
// visibility of newly written facts remains the memory.Queue tail-injection
// concern of the controller, not this package.
package memoryreview

import (
	"context"
	"fmt"
	"io"
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/memory"
)

// Fact is one distilled memory fact as produced by the review model. The
// fields mirror the auto-memory taxonomy (memory.Type / memory.FactScope) and
// the remember tool's arguments.
type Fact struct {
	Type        string `json:"type"`        // user | feedback | project | reference
	Scope       string `json:"scope"`       // project | global (default project)
	Name        string `json:"name"`        // kebab-case slug; the file stem
	Title       string `json:"title"`       // human-readable index label
	Description string `json:"description"` // one-line summary for the index and recall
	Body        string `json:"body"`        // the fact itself (Markdown)
}

// Result is the parsed outcome of one review pass.
type Result struct {
	Facts []Fact
}

// ApplyReport summarises what a review pass persisted.
type ApplyReport struct {
	Created int
	Updated int
	Skipped int
	Errors  []string
}

// Runner executes one headless review prompt and returns the model's final
// response text. *routines.AgentRunner (via RunPrompt) satisfies it; tests
// inject a fake. It intentionally avoids importing routines to keep boot free
// of a boot -> routines -> boot cycle.
type Runner interface {
	RunPrompt(ctx context.Context, prompt, model string) (string, error)
}

// Reviewer distills a session transcript into memory facts.
type Reviewer struct {
	// Runner executes the review LLM call headlessly (auto-approve, same boot
	// path as interactive sessions for prompt-cache warmth).
	Runner Runner
	// Store receives the distilled facts. A zero/disabled store makes Apply a
	// no-op.
	Store memory.Store
	// Model overrides the review model. Empty uses the configured default.
	Model string
	// MaxTranscriptChars caps the transcript fed to the review (0 = default
	// 40000).
	MaxTranscriptChars int
	// DryRun parses and reports the distilled facts without persisting them.
	DryRun bool

	// Stderr receives diagnostics. Nil = io.Discard.
	Stderr io.Writer
}

// DefaultMaxTranscriptChars bounds the review input by default.
const DefaultMaxTranscriptChars = 40000

func (rv *Reviewer) maxChars() int {
	if rv.MaxTranscriptChars <= 0 {
		return DefaultMaxTranscriptChars
	}
	return rv.MaxTranscriptChars
}

// Review loads the session at path, renders its transcript, runs the review
// pass, and applies the resulting facts to the store. The return value carries
// the parsed facts; use ApplyReport from Apply for persistence outcomes.
func (rv *Reviewer) Review(ctx context.Context, sessionPath string) (Result, ApplyReport, error) {
	sess, err := agent.LoadSession(sessionPath)
	if err != nil {
		return Result{}, ApplyReport{}, fmt.Errorf("memoryreview: load session %s: %w", sessionPath, err)
	}
	return rv.ReviewSession(ctx, sess)
}

// ReviewSession runs one review pass over an already-loaded session.
func (rv *Reviewer) ReviewSession(ctx context.Context, sess *agent.Session) (Result, ApplyReport, error) {
	if rv.Runner == nil {
		return Result{}, ApplyReport{}, fmt.Errorf("memoryreview: no runner configured")
	}
	transcript := RenderTranscript(sess, rv.maxChars())
	prompt := BuildPrompt(transcript, rv.Store.List())
	out, err := rv.Runner.RunPrompt(ctx, prompt, rv.Model)
	if err != nil {
		return Result{}, ApplyReport{}, fmt.Errorf("memoryreview: review run: %w", err)
	}
	facts, err := ParseFacts(out)
	if err != nil {
		return Result{}, ApplyReport{}, fmt.Errorf("memoryreview: parse review output: %w", err)
	}
	if rv.DryRun {
		return Result{Facts: facts}, ApplyReport{}, nil
	}
	report := rv.Apply(facts)
	return Result{Facts: facts}, report, nil
}

// Apply persists the facts with create/update/skip semantics:
//
//   - name matches an existing fact and the content is identical -> skipped
//   - name matches an existing fact and the content changed -> updated
//     (revision bump, existing scope preserved)
//   - otherwise -> created (RequireCreate, project scope by default)
//
// Facts without a usable name or body are skipped. A disabled store (zero
// Store) skips everything without error.
func (rv *Reviewer) Apply(facts []Fact) ApplyReport {
	var report ApplyReport
	if len(facts) == 0 {
		return report
	}
	if rv.Store.Dir == "" && rv.Store.GlobalDir == "" {
		report.Skipped = len(facts)
		return report
	}
	byName := map[string]memory.Memory{}
	byTitle := map[string]memory.Memory{}
	for _, m := range rv.Store.List() {
		byName[m.Name] = m
		if t := normalizedTitle(m.Title); t != "" {
			byTitle[t] = m
		}
	}
	for _, f := range facts {
		name := slugify(f.Name)
		body := strings.TrimSpace(f.Body)
		if name == "" || body == "" {
			report.Skipped++
			continue
		}
		m := memory.Memory{
			Name:        name,
			Title:       strings.TrimSpace(f.Title),
			Description: strings.TrimSpace(f.Description),
			Type:        memory.NormalizeType(f.Type),
			Scope:       memory.NormalizeFactScope(f.Scope),
			Body:        body,
		}
		existing, ok := byName[name]
		if !ok {
			if t := normalizedTitle(m.Title); t != "" {
				existing, ok = byTitle[t]
			}
		}
		if ok {
			// Keep the stable name the store resolved, preserve its scope, and
			// never allow an update to silently change project -> global.
			m.Name = existing.Name
			m.Scope = existing.Scope
			if existing.Description == m.Description && existing.Body == m.Body {
				report.Skipped++
				continue
			}
			if _, err := rv.Store.SaveWithOptions(m, memory.SaveOptions{}); err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", name, err))
				continue
			}
			report.Updated++
			continue
		}
		if _, err := rv.Store.SaveWithOptions(m, memory.SaveOptions{RequireCreate: true}); err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		report.Created++
	}
	return report
}

// slugify mirrors the store's slug rule (kebab-case, ASCII alnum runs joined
// by '-').
func slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// normalizedTitle lowercases the title keeping only letters and digits, the
// same normalization the store's recall identity uses.
func normalizedTitle(title string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}
