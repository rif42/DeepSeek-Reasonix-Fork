package routines

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestVerifyHMACSignature(t *testing.T) {
	secret := []byte("s3cret")
	body := []byte(`{"action":"opened"}`)
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	good := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !VerifyHMACSignature(secret, body, good) {
		t.Fatal("valid signature rejected")
	}
	if VerifyHMACSignature(secret, body, "sha256="+strings.Repeat("0", 64)) {
		t.Fatal("invalid signature accepted")
	}
	if VerifyHMACSignature(secret, body, "md5=abc") {
		t.Fatal("non-sha256 signature accepted")
	}
	if VerifyHMACSignature(secret, body, "") {
		t.Fatal("empty signature accepted")
	}
}

func TestRenderPrompt(t *testing.T) {
	payload := map[string]any{
		"pull_request": map[string]any{"number": 42, "title": "Fix auth"},
		"repository":   map[string]any{"name": "demo"},
	}
	out := RenderPrompt("PR #{pull_request.number}: {pull_request.title} in {repository.name} [{event_type}]", payload, "pull_request")
	want := "PR #42: Fix auth in demo [pull_request]"
	if out != want {
		t.Fatalf("RenderPrompt = %q, want %q", out, want)
	}
	// Missing field renders empty.
	out = RenderPrompt("x={missing.field} y={pull_request.title}", payload, "")
	if out != "x= y=Fix auth" {
		t.Fatalf("missing-field render = %q", out)
	}
	// __raw__ contains the payload.
	out = RenderPrompt("payload: {__raw__}", payload, "")
	if !strings.Contains(out, `"pull_request"`) {
		t.Fatalf("__raw__ missing payload: %q", out)
	}
}

// webhookTestServer wires a handler over a temp store with a fake runner and
// an optional local deliverer.
func webhookTestServer(t *testing.T, runner Runner, deliverer Deliverer) (*WebhookHandler, *Store) {
	t.Helper()
	store := newTestStore(t)
	h := NewWebhookHandler(store)
	h.Runner = runner
	h.Deliverer = deliverer
	return h, store
}

func signBody(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func postWebhook(t *testing.T, h http.Handler, path, body, sig, delivery string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	if delivery != "" {
		req.Header.Set("X-GitHub-Delivery", delivery)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestWebhookAgentMode(t *testing.T) {
	runner := &fakeRunner{response: "reviewed"}
	h, store := webhookTestServer(t, runner, nil)
	if err := store.PutWebhook(&WebhookSubscription{Slug: "pr", Prompt: "Review {pull_request.title}"}); err != nil {
		t.Fatal(err)
	}
	rec := postWebhook(t, h, "/webhooks/pr", `{"pull_request":{"title":"fix"}}`, "", "d-1")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	waitFor(t, "agent run", func() bool { return runner.calls() == 1 })
	if !strings.Contains(runner.prompts[0], "Review fix") {
		t.Fatalf("prompt not rendered from payload: %q", runner.prompts[0])
	}
}

func TestWebhookSecretVerification(t *testing.T) {
	runner := &fakeRunner{response: "x"}
	h, store := webhookTestServer(t, runner, nil)
	body := `{"action":"opened"}`
	if err := store.PutWebhook(&WebhookSubscription{Slug: "sec", Prompt: "p", Secret: "sekret"}); err != nil {
		t.Fatal(err)
	}
	// Missing signature -> 401.
	if rec := postWebhook(t, h, "/webhooks/sec", body, "", "d-1"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing signature status = %d, want 401", rec.Code)
	}
	// Bad signature -> 401.
	if rec := postWebhook(t, h, "/webhooks/sec", body, "sha256="+strings.Repeat("0", 64), "d-1"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature status = %d, want 401", rec.Code)
	}
	// Valid signature -> 202.
	rec := postWebhook(t, h, "/webhooks/sec", body, signBody("sekret", body), "d-2")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("valid signature status = %d, want 202", rec.Code)
	}
	waitFor(t, "agent run", func() bool { return runner.calls() == 1 })
}

func TestWebhookUnknownRoute(t *testing.T) {
	h, _ := webhookTestServer(t, nil, nil)
	if rec := postWebhook(t, h, "/webhooks/nope", "{}", "", "d-1"); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestWebhookEventFilter(t *testing.T) {
	runner := &fakeRunner{response: "x"}
	h, store := webhookTestServer(t, runner, nil)
	if err := store.PutWebhook(&WebhookSubscription{Slug: "issues", Prompt: "p", Events: []string{"issues"}}); err != nil {
		t.Fatal(err)
	}
	// Event is pull_request but subscription wants issues -> ignored, no run.
	rec := postWebhook(t, h, "/webhooks/issues", `{"action":"opened"}`, "", "d-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("ignored event status = %d, want 200", rec.Code)
	}
	time.Sleep(50 * time.Millisecond)
	if runner.calls() != 0 {
		t.Fatalf("filtered event ran the agent: %d calls", runner.calls())
	}
}

func TestWebhookIdempotency(t *testing.T) {
	runner := &fakeRunner{response: "x"}
	h, store := webhookTestServer(t, runner, nil)
	if err := store.PutWebhook(&WebhookSubscription{Slug: "pr", Prompt: "p"}); err != nil {
		t.Fatal(err)
	}
	body := `{"action":"opened"}`
	postWebhook(t, h, "/webhooks/pr", body, "", "delivery-abc")
	waitFor(t, "agent run", func() bool { return runner.calls() == 1 })
	// Same delivery id again -> no-op.
	postWebhook(t, h, "/webhooks/pr", body, "", "delivery-abc")
	time.Sleep(50 * time.Millisecond)
	if runner.calls() != 1 {
		t.Fatalf("duplicate delivery ran again: %d calls", runner.calls())
	}
	// Different delivery id -> runs.
	postWebhook(t, h, "/webhooks/pr", body, "", "delivery-xyz")
	waitFor(t, "second run", func() bool { return runner.calls() == 2 })
}

func TestWebhookDeliverOnly(t *testing.T) {
	localDir := filepath.Join(t.TempDir(), "out")
	router := &DeliveryRouter{LocalDir: localDir}
	h, store := webhookTestServer(t, nil, router)
	if err := store.PutWebhook(&WebhookSubscription{
		Slug:        "alert",
		Prompt:      "Alert: {alert.name} severity={alert.severity}",
		DeliverOnly: true,
		Deliver:     "local",
	}); err != nil {
		t.Fatal(err)
	}
	rec := postWebhook(t, h, "/webhooks/alert", `{"alert":{"name":"cpu","severity":"high"}}`, "", "d-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	waitFor(t, "local delivery", func() bool {
		_, err := readDeliveryFile(localDir, "webhook-alert")
		return err == nil
	})
	data, err := readDeliveryFile(localDir, "webhook-alert")
	if err != nil || !strings.Contains(data, "Alert: cpu severity=high") {
		t.Fatalf("deliver_only content wrong: %q, %v", data, err)
	}
}

func readDeliveryFile(localDir, jobID string) (string, error) {
	entries, err := filepath.Glob(filepath.Join(localDir, "deliveries", jobID, "*.md"))
	if err != nil || len(entries) == 0 {
		return "", err
	}
	data, err := os.ReadFile(entries[len(entries)-1])
	return string(data), err
}

func TestWebhookRateLimit(t *testing.T) {
	runner := &fakeRunner{response: "x"}
	h, store := webhookTestServer(t, runner, nil)
	h.RateLimit = 2
	if err := store.PutWebhook(&WebhookSubscription{Slug: "pr", Prompt: "p"}); err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	for i := range 4 {
		rec := postWebhook(t, h, "/webhooks/pr", `{"action":"opened"}`, "", "d-"+string(rune('a'+i)))
		if rec.Code == http.StatusAccepted {
			accepted.Add(1)
		}
		if rec.Code == http.StatusTooManyRequests {
			break
		}
	}
	waitFor(t, "agent runs", func() bool { return runner.calls() >= 2 })
	if accepted.Load() != 2 {
		t.Fatalf("accepted = %d, want 2 (then 429)", accepted.Load())
	}
}
