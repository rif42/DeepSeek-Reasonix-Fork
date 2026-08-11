package routines

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"reasonix/internal/bot"
)

// DeliveryTarget is one resolved delivery destination.
type DeliveryTarget struct {
	Kind     string // "local" | "origin" | "platform"
	Platform string // for Kind == platform
	ChatID   string
	ThreadID string
}

// DeliveryTarget kinds.
const (
	DeliverLocal    = "local"
	DeliverOrigin   = "origin"
	DeliverPlatform = "platform"
)

// ParseDeliveryTarget parses one deliver value:
//
//	"local"                    -> local file only
//	"origin"                   -> the job's originating chat
//	"platform"                 -> that platform's home chat
//	"platform:chat"            -> explicit chat
//	"platform:chat:thread"     -> explicit chat + thread
func ParseDeliveryTarget(s string) (DeliveryTarget, error) {
	s = strings.TrimSpace(s)
	switch s {
	case "", DeliverLocal:
		return DeliveryTarget{Kind: DeliverLocal}, nil
	case DeliverOrigin:
		return DeliveryTarget{Kind: DeliverOrigin}, nil
	default:
		parts := strings.Split(s, ":")
		if len(parts) < 1 || len(parts) > 3 {
			return DeliveryTarget{}, fmt.Errorf("delivery target %q: expected platform[:chat[:thread]]", s)
		}
		t := DeliveryTarget{Kind: DeliverPlatform, Platform: strings.TrimSpace(parts[0])}
		if t.Platform == "" {
			return DeliveryTarget{}, fmt.Errorf("delivery target %q: empty platform", s)
		}
		if len(parts) >= 2 {
			t.ChatID = strings.TrimSpace(parts[1])
		}
		if len(parts) >= 3 {
			t.ThreadID = strings.TrimSpace(parts[2])
		}
		return t, nil
	}
}

// DeliveryAdapter is the light outbound contract a platform must satisfy for
// routine delivery. The service layer adapts the bot gateway's heavier Adapter
// (see BotAdapterShim).
type DeliveryAdapter interface {
	Platform() string
	Name() string
	// HomeChatID is the adapter's default chat for bare "platform" targets.
	HomeChatID() string
	Send(ctx context.Context, chatID, threadID, content string) error
}

// BotAdapterShim adapts a bot gateway Adapter to the routines delivery
// contract. HomeChat is the adapter's default chat ("" = only explicit
// targets work).
type BotAdapterShim struct {
	Adapter  bot.Adapter
	HomeChat string
}

func (b BotAdapterShim) Platform() string { return string(b.Adapter.Platform()) }
func (b BotAdapterShim) Name() string     { return b.Adapter.Name() }
func (b BotAdapterShim) HomeChatID() string {
	return b.HomeChat
}

func (b BotAdapterShim) Send(ctx context.Context, chatID, threadID, content string) error {
	res, err := b.Adapter.Send(ctx, bot.OutboundMessage{
		ChatID: chatID,
		Text:   content,
	})
	if err != nil {
		return err
	}
	return res.Err
}

// DeliveryRouter routes a job's content to its delivery target(s). It
// implements the scheduler's Deliverer interface. Adapters are looked up by
// platform name; unknown platforms are a delivery error.
type DeliveryRouter struct {
	// LocalDir is where "local" deliveries are written.
	LocalDir string
	// Adapters maps platform name -> delivery adapter.
	Adapters map[string]DeliveryAdapter
	// Logger receives delivery diagnostics (optional).
	Logger func(format string, args ...any)
}

// Deliver sends content to every target implied by job.Deliver. An empty
// Deliver means "local". It returns the first delivery error encountered.
func (r *DeliveryRouter) Deliver(ctx context.Context, job *Job, content string) error {
	if job == nil {
		return errors.New("deliver: nil job")
	}
	targets, err := r.resolveTargets(job)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return nil
	}
	var firstErr error
	for _, t := range targets {
		if err := r.deliverOne(ctx, job, t, content); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			r.logf("routines: deliver %s -> %s: %v", job.ID, t.describe(), err)
		}
	}
	return firstErr
}

func (r *DeliveryRouter) resolveTargets(job *Job) ([]DeliveryTarget, error) {
	raw := strings.TrimSpace(job.Deliver)
	if raw == "" || raw == DeliverLocal {
		return []DeliveryTarget{{Kind: DeliverLocal}}, nil
	}
	if raw == "all" {
		var out []DeliveryTarget
		for _, a := range r.Adapters {
			if a == nil {
				continue
			}
			if home := a.HomeChatID(); home != "" {
				out = append(out, DeliveryTarget{Kind: DeliverPlatform, Platform: a.Platform(), ChatID: home})
			}
		}
		if len(out) == 0 {
			return nil, errors.New("deliver=all but no adapter has a home chat configured")
		}
		return out, nil
	}
	t, err := ParseDeliveryTarget(raw)
	if err != nil {
		return nil, err
	}
	if t.Kind == DeliverOrigin {
		if job.Origin.Platform == "" || job.Origin.ChatID == "" {
			// No origin recorded: fall back to a local delivery rather than
			// dropping the result silently.
			return []DeliveryTarget{{Kind: DeliverLocal}}, nil
		}
		return []DeliveryTarget{{
			Kind:     DeliverPlatform,
			Platform: job.Origin.Platform,
			ChatID:   job.Origin.ChatID,
			ThreadID: job.Origin.ThreadID,
		}}, nil
	}
	return []DeliveryTarget{t}, nil
}

func (r *DeliveryRouter) deliverOne(ctx context.Context, job *Job, t DeliveryTarget, content string) error {
	switch t.Kind {
	case DeliverLocal:
		return r.deliverLocal(job, content)
	case DeliverPlatform:
		adapter := r.Adapters[t.Platform]
		if adapter == nil {
			return fmt.Errorf("no delivery adapter for platform %q", t.Platform)
		}
		chatID := t.ChatID
		if chatID == "" {
			chatID = adapter.HomeChatID()
		}
		if chatID == "" {
			return fmt.Errorf("platform %q: no chat id and no home chat configured", t.Platform)
		}
		return adapter.Send(ctx, chatID, t.ThreadID, content)
	default:
		return fmt.Errorf("unknown delivery kind %q", t.Kind)
	}
}

// deliverLocal saves content to LocalDir/deliveries/<jobID>/<timestamp>.md.
func (r *DeliveryRouter) deliverLocal(job *Job, content string) error {
	if r.LocalDir == "" {
		return errors.New("local delivery requested but no LocalDir configured")
	}
	dir := filepath.Join(r.LocalDir, "deliveries", job.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create delivery dir: %w", err)
	}
	path := filepath.Join(dir, time.Now().UTC().Format("20060102-150405.000")+".md")
	doc := fmt.Sprintf("# %s\n\n%s\n", job.Name, content)
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		return fmt.Errorf("write delivery file: %w", err)
	}
	return nil
}

func (r *DeliveryRouter) logf(format string, args ...any) {
	if r.Logger != nil {
		r.Logger(format, args...)
	}
}

func (t DeliveryTarget) describe() string {
	switch t.Kind {
	case DeliverLocal:
		return "local"
	case DeliverOrigin:
		return "origin"
	default:
		s := t.Platform
		if t.ChatID != "" {
			s += ":" + t.ChatID
		}
		if t.ThreadID != "" {
			s += ":" + t.ThreadID
		}
		return s
	}
}
