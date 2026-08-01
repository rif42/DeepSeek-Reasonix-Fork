package routines

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseDeliveryTarget(t *testing.T) {
	cases := []struct {
		in      string
		kind    string
		plat    string
		chat    string
		thread  string
		wantErr bool
	}{
		{"", DeliverLocal, "", "", "", false},
		{"local", DeliverLocal, "", "", "", false},
		{"origin", DeliverOrigin, "", "", "", false},
		{"feishu", DeliverPlatform, "feishu", "", "", false},
		{"feishu:oc_123", DeliverPlatform, "feishu", "oc_123", "", false},
		{"qq:12345:678", DeliverPlatform, "qq", "12345", "678", false},
		{"a:b:c:d", "", "", "", "", true},
		{":", "", "", "", "", true},
	}
	for _, tc := range cases {
		got, err := ParseDeliveryTarget(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseDeliveryTarget(%q): expected error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseDeliveryTarget(%q): %v", tc.in, err)
			continue
		}
		if got.Kind != tc.kind || got.Platform != tc.plat || got.ChatID != tc.chat || got.ThreadID != tc.thread {
			t.Errorf("ParseDeliveryTarget(%q) = %+v", tc.in, got)
		}
	}
}

type fakeAdapter struct {
	platform string
	homeChat string
	gotChat  string
	gotText  string
	err      error
}

func (f *fakeAdapter) Platform() string   { return f.platform }
func (f *fakeAdapter) Name() string       { return "fake-" + f.platform }
func (f *fakeAdapter) HomeChatID() string { return f.homeChat }
func (f *fakeAdapter) Send(_ context.Context, chatID, _ string, content string) error {
	f.gotChat = chatID
	f.gotText = content
	return f.err
}

func testJobForDelivery() *Job {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	return &Job{ID: "job-1", Name: "delivery job", Prompt: "p", CreatedAt: now, UpdatedAt: now}
}

func TestRouterLocalDelivery(t *testing.T) {
	localDir := filepath.Join(t.TempDir(), "out")
	r := &DeliveryRouter{LocalDir: localDir}
	job := testJobForDelivery()
	if err := r.Deliver(context.Background(), job, "hello local"); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	files, err := os.ReadDir(filepath.Join(localDir, "deliveries", "job-1"))
	if err != nil || len(files) != 1 {
		t.Fatalf("local delivery not written: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(localDir, "deliveries", "job-1", files[0].Name()))
	if !strings.Contains(string(data), "hello local") {
		t.Fatalf("delivery content mismatch: %q", string(data))
	}
}

func TestRouterLocalWithoutDirFails(t *testing.T) {
	r := &DeliveryRouter{}
	if err := r.Deliver(context.Background(), testJobForDelivery(), "x"); err == nil {
		t.Fatal("expected error when LocalDir unset")
	}
}

func TestRouterPlatformDelivery(t *testing.T) {
	adapter := &fakeAdapter{platform: "feishu", homeChat: "home-1"}
	r := &DeliveryRouter{Adapters: map[string]DeliveryAdapter{"feishu": adapter}}
	job := testJobForDelivery()
	job.Deliver = "feishu:oc_123"
	if err := r.Deliver(context.Background(), job, "hi"); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if adapter.gotChat != "oc_123" || adapter.gotText != "hi" {
		t.Fatalf("adapter got chat=%q text=%q", adapter.gotChat, adapter.gotText)
	}
}

func TestRouterPlatformHomeFallback(t *testing.T) {
	adapter := &fakeAdapter{platform: "qq", homeChat: "home-1"}
	r := &DeliveryRouter{Adapters: map[string]DeliveryAdapter{"qq": adapter}}
	job := testJobForDelivery()
	job.Deliver = "qq" // bare platform -> home chat
	if err := r.Deliver(context.Background(), job, "hi"); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if adapter.gotChat != "home-1" {
		t.Fatalf("expected home chat, got %q", adapter.gotChat)
	}
}

func TestRouterUnknownPlatformFails(t *testing.T) {
	r := &DeliveryRouter{Adapters: map[string]DeliveryAdapter{}}
	job := testJobForDelivery()
	job.Deliver = "slack:channel"
	if err := r.Deliver(context.Background(), job, "hi"); err == nil {
		t.Fatal("expected error for unknown platform")
	}
}

func TestRouterOriginDelivery(t *testing.T) {
	adapter := &fakeAdapter{platform: "feishu"}
	r := &DeliveryRouter{Adapters: map[string]DeliveryAdapter{"feishu": adapter}}
	job := testJobForDelivery()
	job.Deliver = "origin"
	job.Origin = Origin{Platform: "feishu", ChatID: "oc_origin"}
	if err := r.Deliver(context.Background(), job, "hi"); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if adapter.gotChat != "oc_origin" {
		t.Fatalf("origin chat not used: %q", adapter.gotChat)
	}
}

func TestRouterOriginWithoutOriginFallsBackLocal(t *testing.T) {
	localDir := filepath.Join(t.TempDir(), "out")
	r := &DeliveryRouter{LocalDir: localDir}
	job := testJobForDelivery()
	job.Deliver = "origin" // no Origin recorded
	if err := r.Deliver(context.Background(), job, "hi"); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if _, err := os.Stat(filepath.Join(localDir, "deliveries", "job-1")); err != nil {
		t.Fatalf("expected local fallback, err %v", err)
	}
}

func TestRouterDeliverAll(t *testing.T) {
	a1 := &fakeAdapter{platform: "feishu", homeChat: "h1"}
	a2 := &fakeAdapter{platform: "qq", homeChat: "h2"}
	r := &DeliveryRouter{Adapters: map[string]DeliveryAdapter{"feishu": a1, "qq": a2}}
	job := testJobForDelivery()
	job.Deliver = "all"
	if err := r.Deliver(context.Background(), job, "hi"); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if a1.gotChat != "h1" || a2.gotChat != "h2" {
		t.Fatalf("all-delivery chats: %q %q", a1.gotChat, a2.gotChat)
	}
}

func TestRouterPartialFailureReturnsError(t *testing.T) {
	good := &fakeAdapter{platform: "feishu", homeChat: "h1"}
	bad := &fakeAdapter{platform: "qq", homeChat: "h2", err: errors.New("send failed")}
	r := &DeliveryRouter{Adapters: map[string]DeliveryAdapter{"feishu": good, "qq": bad}}
	job := testJobForDelivery()
	job.Deliver = "all"
	if err := r.Deliver(context.Background(), job, "hi"); err == nil {
		t.Fatal("expected delivery error from failing adapter")
	}
}

// TestSchedulerRecordsDeliveryError wires the router into the scheduler and
// verifies a failing platform delivery lands in the job's LastDeliveryError.
func TestSchedulerRecordsDeliveryError(t *testing.T) {
	adapter := &fakeAdapter{platform: "feishu", homeChat: "h1", err: errors.New("down")}
	router := &DeliveryRouter{Adapters: map[string]DeliveryAdapter{"feishu": adapter}}
	runner := &fakeRunner{response: "the result"}
	s, store, _ := newScheduler(t, runner, "")
	s.deliverer = router
	defer s.Stop()
	j := dueJob("job-1")
	j.Deliver = "feishu"
	if err := store.PutJob(j); err != nil {
		t.Fatal(err)
	}
	s.TickOnce(time.Now().Add(time.Minute))
	var job *Job
	waitFor(t, "delivery recorded", func() bool {
		cur, _ := store.GetJob("job-1")
		job = cur
		return job != nil && job.LastDeliveryError != ""
	})
	if job.LastStatus != "ok" {
		t.Fatalf("run should still be ok despite delivery failure: %+v", job)
	}
	if !strings.Contains(job.LastDeliveryError, "down") {
		t.Fatalf("delivery error not recorded: %q", job.LastDeliveryError)
	}
}
