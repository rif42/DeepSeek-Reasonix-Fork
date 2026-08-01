package serve

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"reasonix/internal/config"
	"reasonix/internal/control"
)

// routinesTestServer builds a serve Server over a fake controller and returns
// an httptest server. The routines hub lazily opens a store under the isolated
// REASONIX_HOME that TestMain sets up.
func routinesTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	bc := NewBroadcaster()
	ctrl := control.New(control.Options{Runner: fakeRunner{got: make(chan string, 1)}, Sink: bc})
	srv := httptest.NewServer(New(ctrl, bc, config.ServeConfig{}).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func routinesJSON(t *testing.T, method, url, body string) (int, map[string]any) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("{}")
	} else {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if resp.StatusCode < 500 {
		_ = json.NewDecoder(resp.Body).Decode(&out)
	}
	return resp.StatusCode, out
}

func TestRoutinesCRUD(t *testing.T) {
	srv := routinesTestServer(t)

	// Empty index.
	code, idx := routinesJSON(t, http.MethodGet, srv.URL+"/routines", "")
	if code != http.StatusOK {
		t.Fatalf("GET /routines = %d", code)
	}
	if jobs, _ := idx["jobs"].([]any); len(jobs) != 0 {
		t.Fatalf("expected no jobs, got %v", jobs)
	}

	// Create a job.
	code, job := routinesJSON(t, http.MethodPost, srv.URL+"/routines/jobs",
		`{"schedule":"every 30m","prompt":"check ci","name":"CI","deliver":"local"}`)
	if code != http.StatusOK {
		t.Fatalf("create job = %d, %v", code, job)
	}
	id, _ := job["id"].(string)
	if id == "" {
		t.Fatalf("job id missing: %v", job)
	}
	if job["next_run_at"] == nil {
		t.Fatalf("next_run_at not set: %v", job)
	}

	// Index now lists it.
	_, idx = routinesJSON(t, http.MethodGet, srv.URL+"/routines", "")
	if jobs, _ := idx["jobs"].([]any); len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %v", jobs)
	}

	// Pause then resume.
	code, _ = routinesJSON(t, http.MethodPost, srv.URL+"/routines/jobs/"+id+"/pause", "")
	if code != http.StatusOK {
		t.Fatalf("pause = %d", code)
	}
	_, idx = routinesJSON(t, http.MethodGet, srv.URL+"/routines", "")
	if jobs, _ := idx["jobs"].([]any); len(jobs) == 1 {
		state := jobs[0].(map[string]any)["state"]
		if state != "paused" {
			t.Fatalf("state after pause = %v", state)
		}
	}
	code, _ = routinesJSON(t, http.MethodPost, srv.URL+"/routines/jobs/"+id+"/resume", "")
	if code != http.StatusOK {
		t.Fatalf("resume = %d", code)
	}

	// Webhook subscribe.
	code, wh := routinesJSON(t, http.MethodPost, srv.URL+"/routines/webhooks",
		`{"slug":"pr","prompt":"review {pull_request.title}","events":["pull_request"],"deliver":"local"}`)
	if code != http.StatusOK {
		t.Fatalf("subscribe = %d, %v", code, wh)
	}
	if wh["generated"] != true {
		t.Fatalf("secret should be auto-generated: %v", wh)
	}
	if wh["secret"] == "" {
		t.Fatalf("generated secret missing: %v", wh)
	}

	// The index view must NOT leak the secret.
	_, idx = routinesJSON(t, http.MethodGet, srv.URL+"/routines", "")
	webhooks, _ := idx["webhooks"].([]any)
	if len(webhooks) != 1 {
		t.Fatalf("expected 1 webhook, got %v", webhooks)
	}
	if _, leaked := webhooks[0].(map[string]any)["secret"]; leaked {
		t.Fatalf("webhook secret leaked into index view: %v", webhooks[0])
	}

	// Remove webhook then job.
	code, _ = routinesJSON(t, http.MethodPost, srv.URL+"/routines/webhooks/pr/remove", "")
	if code != http.StatusOK {
		t.Fatalf("remove webhook = %d", code)
	}
	code, _ = routinesJSON(t, http.MethodPost, srv.URL+"/routines/jobs/"+id+"/remove", "")
	if code != http.StatusOK {
		t.Fatalf("remove job = %d", code)
	}
	_, idx = routinesJSON(t, http.MethodGet, srv.URL+"/routines", "")
	if jobs, _ := idx["jobs"].([]any); len(jobs) != 0 {
		t.Fatalf("job not removed: %v", jobs)
	}
}

func TestRoutinesCreateValidation(t *testing.T) {
	srv := routinesTestServer(t)
	cases := []struct {
		body string
		want int
	}{
		{`{"schedule":"","prompt":"hi"}`, http.StatusBadRequest},
		{`{"schedule":"every 30m","prompt":""}`, http.StatusBadRequest},
		{`{"schedule":"banana","prompt":"hi"}`, http.StatusBadRequest},
		{`{"schedule":"every 30m","prompt":"hi"}`, http.StatusOK},
	}
	for i, tc := range cases {
		code, _ := routinesJSON(t, http.MethodPost, srv.URL+"/routines/jobs", tc.body)
		if code != tc.want {
			t.Errorf("case %d: status = %d, want %d", i, code, tc.want)
		}
	}
}

func TestRoutinesMissingJobReturns404(t *testing.T) {
	srv := routinesTestServer(t)
	for _, path := range []string{
		"/routines/jobs/nope/pause",
		"/routines/jobs/nope/resume",
		"/routines/jobs/nope/remove",
		"/routines/webhooks/nope/remove",
	} {
		code, _ := routinesJSON(t, http.MethodPost, srv.URL+path, "")
		if code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404", path, code)
		}
	}
}

func TestRoutinesJobRunDispatches(t *testing.T) {
	srv := routinesTestServer(t)
	_, job := routinesJSON(t, http.MethodPost, srv.URL+"/routines/jobs",
		`{"schedule":"every 30m","prompt":"hi"}`)
	id, _ := job["id"].(string)
	if id == "" {
		t.Fatal("job not created")
	}
	code, out := routinesJSON(t, http.MethodPost, srv.URL+"/routines/jobs/"+id+"/run", "")
	if code != http.StatusOK {
		t.Fatalf("run = %d, %v", code, out)
	}
	if out["dispatched"] != id {
		t.Fatalf("run did not dispatch: %v", out)
	}
}

func TestRoutinesWebhookFixedSecret(t *testing.T) {
	srv := routinesTestServer(t)
	code, wh := routinesJSON(t, http.MethodPost, srv.URL+"/routines/webhooks",
		`{"slug":"alert","prompt":"p","secret":"my-secret","deliver_only":true}`)
	if code != http.StatusOK {
		t.Fatalf("subscribe = %d", code)
	}
	if wh["secret"] != "my-secret" || wh["generated"] != false {
		t.Fatalf("fixed secret not honored: %v", wh)
	}
}
