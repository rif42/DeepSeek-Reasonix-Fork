package memoryreview

import (
	"context"
	"strings"
	"testing"

	"reasonix/internal/agent"
	"reasonix/internal/memory"
	"reasonix/internal/provider"
)

// fakeRunner returns a canned review response.
type fakeRunner struct {
	out    string
	prompt string
	model  string
}

func (f *fakeRunner) RunPrompt(_ context.Context, prompt, model string) (string, error) {
	f.prompt = prompt
	f.model = model
	return f.out, nil
}

func TestParseFacts(t *testing.T) {
	body := `{"facts":[{"type":"feedback","scope":"project","name":"prefers-tabs","title":"Prefers tabs","description":"Indentation preference","body":"Uses tabs."}]}`
	cases := []struct {
		name    string
		out     string
		want    int
		wantErr bool
	}{
		{"bare JSON", body, 1, false},
		{"fenced JSON", "```json\n" + body + "\n```", 1, false},
		{"fence with trailing prose", "```\n" + body + "\n```\nDone.", 1, false},
		{"empty object", `{"facts":[]}`, 0, false},
		{"no JSON", "I could not distill any facts.", 0, true},
		{"invalid JSON", `{"facts":[{"name":}`, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			facts, err := ParseFacts(tc.out)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %d facts", len(facts))
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(facts) != tc.want {
				t.Fatalf("got %d facts, want %d", len(facts), tc.want)
			}
		})
	}
}

func TestRenderTranscript(t *testing.T) {
	sess := agent.NewSession("system prompt")
	sess.Messages = []provider.Message{
		{Role: provider.RoleSystem, Content: "system"},
		{Role: provider.RoleUser, Content: "hello"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{Name: "write_file", Arguments: `{"path":"a.txt"}`}}},
		{Role: provider.RoleTool, Content: "ok"},
		{Role: provider.RoleAssistant, Content: "done"},
	}
	out := RenderTranscript(sess, 0)
	for _, want := range []string{"[user] hello", "[assistant tool call] write_file", "[tool result] ok", "[assistant] done"} {
		if !strings.Contains(out, want) {
			t.Fatalf("transcript missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "system prompt") || strings.Contains(out, "[system]") {
		t.Fatalf("system message leaked into transcript:\n%s", out)
	}
	// Truncation cap applies: the transcript portion is capped, with the
	// truncation marker appended.
	out = RenderTranscript(sess, 20)
	if !strings.HasSuffix(out, "[transcript truncated]") {
		t.Fatalf("truncation marker missing: %q", out)
	}
	if len(out) > 60 {
		t.Fatalf("truncation cap not applied: len=%d", len(out))
	}
}

func TestApplyCreateUpdateSkip(t *testing.T) {
	st := memory.StoreFor(t.TempDir(), t.TempDir())
	rv := &Reviewer{Store: st}

	base := Fact{Type: "project", Scope: "project", Name: "currency-cron", Title: "Currency cron", Description: "Scheduled currency fetch", Body: "Runs daily at 19:00 Bali time."}

	// Create.
	r := rv.Apply([]Fact{base})
	if r.Created != 1 || r.Updated != 0 || r.Skipped != 0 {
		t.Fatalf("create report wrong: %+v", r)
	}
	mems := st.List()
	if len(mems) != 1 {
		t.Fatalf("store has %d memories, want 1", len(mems))
	}
	got := mems[0]
	if got.Name != "currency-cron" || got.Revision != 1 || got.Type != memory.TypeProject || got.Scope != memory.FactScopeProject {
		t.Fatalf("created memory wrong: %+v", got)
	}

	// Identical content -> skip.
	r = rv.Apply([]Fact{base})
	if r.Skipped != 1 || r.Updated != 0 || r.Created != 0 {
		t.Fatalf("skip report wrong: %+v", r)
	}

	// Changed body -> update, revision bumps, scope preserved.
	upd := base
	upd.Body = "Runs daily at 19:00 Bali time; writes currencies.md."
	upd.Scope = "global" // must NOT upgrade an existing project fact
	r = rv.Apply([]Fact{upd})
	if r.Updated != 1 || r.Created != 0 || r.Skipped != 0 {
		t.Fatalf("update report wrong: %+v", r)
	}
	mems = st.List()
	if len(mems) != 1 {
		t.Fatalf("store has %d memories after update, want 1", len(mems))
	}
	got = mems[0]
	if got.Revision != 2 || got.Scope != memory.FactScopeProject || !strings.Contains(got.Body, "currencies.md") {
		t.Fatalf("updated memory wrong: %+v", got)
	}

	// Invalid facts are skipped, not errors.
	r = rv.Apply([]Fact{{Name: "", Body: "x"}, {Name: "no-body", Body: "  "}})
	if r.Skipped != 2 || len(r.Errors) != 0 {
		t.Fatalf("invalid-fact report wrong: %+v", r)
	}
}

func TestApplyDisabledStore(t *testing.T) {
	rv := &Reviewer{Store: memory.Store{}} // zero store = disabled
	r := rv.Apply([]Fact{{Name: "x", Body: "y"}})
	if r.Skipped != 1 || len(r.Errors) != 0 {
		t.Fatalf("disabled store should skip everything: %+v", r)
	}
}

func TestReviewSessionAppliesFacts(t *testing.T) {
	sess := agent.NewSession("sys")
	sess.Messages = []provider.Message{
		{Role: provider.RoleUser, Content: "please prefer english replies"},
		{Role: provider.RoleAssistant, Content: "sure"},
	}
	fr := &fakeRunner{out: `{"facts":[{"type":"user","scope":"global","name":"prefers-english","title":"Prefers English","description":"Language preference","body":"User prefers English replies."}]}`}
	st := memory.StoreFor(t.TempDir(), t.TempDir())
	rv := &Reviewer{Runner: fr, Store: st}
	res, report, err := rv.ReviewSession(context.Background(), sess)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if len(res.Facts) != 1 || report.Created != 1 {
		t.Fatalf("result wrong: facts=%d created=%d", len(res.Facts), report.Created)
	}
	if !strings.Contains(fr.prompt, "[user] please prefer english replies") {
		t.Fatalf("prompt missing transcript")
	}
	mems := st.List()
	if len(mems) != 1 || mems[0].Scope != memory.FactScopeGlobal || mems[0].Type != memory.TypeUser {
		t.Fatalf("persisted fact wrong: %+v", mems)
	}
}

func TestReviewSessionDryRun(t *testing.T) {
	sess := agent.NewSession("sys")
	sess.Messages = []provider.Message{{Role: provider.RoleUser, Content: "hi"}}
	fr := &fakeRunner{out: `{"facts":[{"type":"project","name":"dry-fact","title":"Dry","description":"d","body":"b"}]}`}
	st := memory.StoreFor(t.TempDir(), t.TempDir())
	rv := &Reviewer{Runner: fr, Store: st, DryRun: true}
	res, report, err := rv.ReviewSession(context.Background(), sess)
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if len(res.Facts) != 1 {
		t.Fatalf("dry run lost facts: %d", len(res.Facts))
	}
	if report.Created != 0 || len(st.List()) != 0 {
		t.Fatalf("dry run wrote to the store: report=%+v store=%d", report, len(st.List()))
	}
}

func TestReviewSessionBadOutput(t *testing.T) {
	sess := agent.NewSession("sys")
	fr := &fakeRunner{out: "no json here"}
	rv := &Reviewer{Runner: fr, Store: memory.StoreFor(t.TempDir(), t.TempDir())}
	if _, _, err := rv.ReviewSession(context.Background(), sess); err == nil {
		t.Fatal("expected error for non-JSON review output")
	}
}
