package routines

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// maxWebhookBody bounds the accepted payload size (defensive; GitHub events
// are a few KB).
const maxWebhookBody = 1 << 20 // 1 MiB

// maxRawRender caps the {__raw__} payload injected into a prompt.
const maxRawRender = 4096

// WebhookHandler receives HTTP webhook events and runs subscribed automations.
// It implements http.Handler for the route /webhooks/{slug}.
type WebhookHandler struct {
	Store        *Store
	Runner       Runner
	Deliverer    Deliverer
	DefaultModel string

	// RateLimit is the per-route request limit per minute (default 30).
	RateLimit int

	Logger func(format string, args ...any)
	Now    func() time.Time

	mu          sync.Mutex
	idempotent  map[string]time.Time // delivery key -> seen time (1h TTL)
	rateWindows map[string]*rateWindow
}

type rateWindow struct {
	start time.Time
	count int
}

// NewWebhookHandler builds a handler with defaults applied.
func NewWebhookHandler(store *Store) *WebhookHandler {
	return &WebhookHandler{
		Store:       store,
		RateLimit:   30,
		Logger:      func(string, ...any) {},
		Now:         time.Now,
		idempotent:  map[string]time.Time{},
		rateWindows: map[string]*rateWindow{},
	}
}

// ServeHTTP handles POST /webhooks/{slug}.
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	slug := strings.TrimPrefix(r.URL.Path, "/webhooks/")
	slug = strings.TrimSuffix(slug, "/")
	if slug == "" || strings.Contains(slug, "/") {
		http.Error(w, "unknown webhook route", http.StatusNotFound)
		return
	}

	sub, err := h.Store.GetWebhook(slug)
	if err != nil {
		h.logf("webhook %s: store: %v", slug, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if sub == nil {
		http.Error(w, "unknown webhook route", http.StatusNotFound)
		return
	}

	// Per-route rate limit before any body work.
	if !h.allowRate(slug) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody+1))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if len(body) > maxWebhookBody {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}

	// HMAC verification (when the subscription has a secret).
	if sub.Secret != "" {
		sig := r.Header.Get("X-Hub-Signature-256")
		if sig == "" {
			sig = r.Header.Get("X-Reasonix-Signature")
		}
		if sig == "" || !VerifyHMACSignature([]byte(sub.Secret), body, sig) {
			h.logf("webhook %s: bad signature", slug)
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
	}

	// Idempotency: GitHub sends X-GitHub-Delivery; fall back to a body hash.
	deliveryID := r.Header.Get("X-GitHub-Delivery")
	if deliveryID == "" {
		sum := sha256.Sum256(body)
		deliveryID = hex.EncodeToString(sum[:])
	}
	key := slug + ":" + deliveryID
	if !h.claimIdempotent(key) {
		w.WriteHeader(http.StatusOK)
		return
	}

	eventType := webhookEventType(r, body)
	if len(sub.Events) > 0 && !containsString(sub.Events, eventType) {
		h.logf("webhook %s: event %q ignored (subscribed: %v)", slug, eventType, sub.Events)
		w.WriteHeader(http.StatusOK)
		return
	}

	payload := map[string]any{}
	_ = json.Unmarshal(body, &payload)

	if sub.DeliverOnly {
		content := RenderPrompt(sub.Prompt, payload, eventType)
		if strings.TrimSpace(content) == "" {
			content = string(body)
		}
		h.deliver(sub, content)
		w.WriteHeader(http.StatusOK)
		return
	}

	prompt := RenderPrompt(sub.Prompt, payload, eventType)
	if strings.TrimSpace(prompt) == "" {
		http.Error(w, "rendered prompt is empty", http.StatusBadRequest)
		return
	}
	// Agent mode: fire the run asynchronously so the webhook returns fast.
	go h.runAgent(sub, prompt)
	w.WriteHeader(http.StatusAccepted)
}

// runAgent executes the webhook-triggered agent run and delivers the result.
func (h *WebhookHandler) runAgent(sub *WebhookSubscription, prompt string) {
	if h.Runner == nil {
		h.logf("webhook %s: no runner configured", sub.Slug)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	result, err := h.Runner.Run(ctx, RunOptions{JobID: "webhook-" + sub.Slug, Prompt: prompt, Model: h.DefaultModel})
	if err != nil {
		h.logf("webhook %s: agent run: %v", sub.Slug, err)
		return
	}
	h.deliver(sub, result.FinalResponse)
}

// deliver routes content through the deliverer using a synthetic job shaped by
// the subscription's Deliver setting (default "local").
func (h *WebhookHandler) deliver(sub *WebhookSubscription, content string) {
	if h.Deliverer == nil {
		h.logf("webhook %s: no deliverer configured", sub.Slug)
		return
	}
	deliver := strings.TrimSpace(sub.Deliver)
	if deliver == "" {
		deliver = DeliverLocal
	}
	job := &Job{ID: "webhook-" + sub.Slug, Name: sub.Slug, Deliver: deliver, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := h.Deliverer.Deliver(context.Background(), job, content); err != nil {
		h.logf("webhook %s: deliver: %v", sub.Slug, err)
	}
}

// webhookEventType extracts the event name from GitHub-style headers or the
// payload itself.
func webhookEventType(r *http.Request, body []byte) string {
	if v := r.Header.Get("X-GitHub-Event"); v != "" {
		return v
	}
	if v := r.Header.Get("X-Event-Key"); v != "" {
		return v
	}
	var doc struct {
		EventType string `json:"event_type"`
		Action    string `json:"action"`
	}
	if err := json.Unmarshal(body, &doc); err == nil {
		if doc.EventType != "" {
			return doc.EventType
		}
		return doc.Action
	}
	return ""
}

// VerifyHMACSignature verifies a "sha256=<hex>" HMAC signature over body with
// secret, in constant time.
func VerifyHMACSignature(secret, body []byte, signature string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(signature, prefix) {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(signature, prefix))
	if err != nil || len(got) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	want := mac.Sum(nil)
	return hmac.Equal(got, want)
}

// allowRate enforces the per-slug sliding minute window.
func (h *WebhookHandler) allowRate(slug string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.Now()
	win := h.rateWindows[slug]
	if win == nil || now.Sub(win.start) >= time.Minute {
		h.rateWindows[slug] = &rateWindow{start: now, count: 1}
		return true
	}
	win.count++
	return win.count <= h.RateLimit
}

// claimIdempotent records a delivery key for 1h; a repeat within the window is
// a no-op. Old keys are pruned opportunistically.
func (h *WebhookHandler) claimIdempotent(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.Now()
	if _, seen := h.idempotent[key]; seen {
		return false
	}
	h.idempotent[key] = now
	for k, t := range h.idempotent {
		if now.Sub(t) > time.Hour {
			delete(h.idempotent, k)
		}
	}
	return true
}

// RenderPrompt substitutes {event.field} dot-notation references, {event_type}
// and {__raw__} in a webhook prompt template. Missing fields render empty.
func RenderPrompt(template string, payload map[string]any, eventType string) string {
	if !strings.Contains(template, "{") {
		return template
	}
	raw, _ := json.MarshalIndent(payload, "", "  ")
	rawStr := string(raw)
	if len(rawStr) > maxRawRender {
		rawStr = rawStr[:maxRawRender] + "…"
	}
	var b strings.Builder
	for i := 0; i < len(template); {
		if template[i] != '{' {
			b.WriteByte(template[i])
			i++
			continue
		}
		end := strings.IndexByte(template[i:], '}')
		if end < 0 {
			b.WriteString(template[i:])
			break
		}
		token := template[i+1 : i+end]
		i += end + 1
		switch token {
		case "event_type":
			b.WriteString(eventType)
		case "__raw__":
			b.WriteString(rawStr)
		default:
			if v, ok := lookupPath(payload, token); ok {
				fmt.Fprint(&b, v)
			}
		}
	}
	return b.String()
}

// lookupPath walks a dot-notation path into a nested payload map.
func lookupPath(payload map[string]any, path string) (any, bool) {
	parts := strings.Split(path, ".")
	var cur any = payload
	for _, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func containsString(list []string, s string) bool {
	return slices.Contains(list, s)
}

func (h *WebhookHandler) logf(format string, args ...any) {
	if h.Logger != nil {
		h.Logger(format, args...)
	}
}
