package control

import (
	"sync/atomic"
	"testing"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/event"
	"reasonix/internal/provider"
	"reasonix/internal/tool"
)

// turnDoneSink closes done when the controller emits TurnDone (the nudge runs
// immediately after in finishGuardedTurn, so done also brackets the nudge).
type turnDoneSink struct{ done chan struct{} }

func (s turnDoneSink) Emit(e event.Event) {
	if e.Kind == event.TurnDone {
		select {
		case <-s.done:
		default:
			close(s.done)
		}
	}
}

// TestMemoryReviewGate pins the pure nudge decision: interval counting,
// min-turns postponement (no reset), and reset-on-fire.
func TestMemoryReviewGate(t *testing.T) {
	if fire, next := memoryReviewGate(0, 4, 10, 5); fire || next != 5 {
		t.Fatalf("disabled gate fired or mutated counter: fire=%v next=%d", fire, next)
	}
	if fire, next := memoryReviewGate(10, 4, 3, 2); fire || next != 2 {
		t.Fatalf("min-turns gate wrong: fire=%v next=%d", fire, next)
	}
	fire, next := memoryReviewGate(3, 0, 1, 0)
	if fire || next != 1 {
		t.Fatalf("count 1 wrong: fire=%v next=%d", fire, next)
	}
	fire, next = memoryReviewGate(3, 0, 2, 1)
	if fire || next != 2 {
		t.Fatalf("count 2 wrong: fire=%v next=%d", fire, next)
	}
	fire, next = memoryReviewGate(3, 0, 3, 2)
	if !fire || next != 0 {
		t.Fatalf("count 3 should fire and reset: fire=%v next=%d", fire, next)
	}
}

// memoryReviewController builds a controller whose agent emits tool calls, so
// the nudge path (Submit -> finishGuardedTurn -> maybeNudgeMemoryReview) is
// exercised with real history.
func memoryReviewController(turns [][]provider.Chunk, cfg *MemoryReviewConfig, sink event.Sink) *Controller {
	reg := tool.NewRegistry()
	reg.Add(&recordingWriter{})
	prov := &scriptedTurns{turns: turns}
	ag := agent.New(prov, reg, agent.NewSession(""), agent.Options{}, event.Discard)
	if sink == nil {
		sink = event.Discard
	}
	return New(Options{Runner: ag, Executor: ag, Sink: sink, MemoryReview: cfg})
}

// toolTurn models one user turn as the scripted provider sees it: the agent
// streams ONE model response per provider call, executes its tool call, then
// streams again for the follow-up text. So a tool-using turn is two scripted
// entries (tool-call chunks, then the final text) — mirroring approval_e2e.
func toolTurn(id, path, text string) [][]provider.Chunk {
	return [][]provider.Chunk{
		toolCallTurn(id, "write_file", `{"path":"`+path+`"}`),
		textTurn(text),
	}
}

// TestMemoryReviewNudgeFiresAfterToolTurn: one tool-using turn with
// NudgeInterval=1 fires the review exactly once, on a detached goroutine.
func TestMemoryReviewNudgeFiresAfterToolTurn(t *testing.T) {
	fired := make(chan struct{})
	var fires atomic.Int32
	done := make(chan struct{})
	c := memoryReviewController(
		append(toolTurn("c1", "a.txt", "Done."), [][]provider.Chunk{}...),
		&MemoryReviewConfig{Enabled: true, NudgeInterval: 1, MinTurns: 1, Fire: func() {
			fires.Add(1)
			select {
			case <-fired:
			default:
				close(fired)
			}
		}},
		turnDoneSink{done: done},
	)
	c.Submit("do the thing")
	waitDone(t, done)
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("memory review nudge never fired")
	}
	if fires.Load() != 1 {
		t.Fatalf("fired %d times, want 1", fires.Load())
	}
}

// TestMemoryReviewNudgeRespectsInterval: with NudgeInterval=2 the review fires
// only on the second tool-using turn.
func TestMemoryReviewNudgeRespectsInterval(t *testing.T) {
	fired := make(chan struct{})
	var fires atomic.Int32
	c := memoryReviewController(
		append(append(toolTurn("c1", "a.txt", "First."), toolTurn("c2", "b.txt", "Second.")...), [][]provider.Chunk{}...),
		&MemoryReviewConfig{Enabled: true, NudgeInterval: 2, MinTurns: 1, Fire: func() {
			if fires.Add(1) == 1 {
				close(fired)
			}
		}},
		nil,
	)
	done1 := make(chan struct{})
	c.sink = turnDoneSink{done: done1}
	c.Submit("first")
	waitDone(t, done1)
	time.Sleep(100 * time.Millisecond)
	if fires.Load() != 0 {
		t.Fatalf("fired after first turn, want 0 (interval 2)")
	}
	done2 := make(chan struct{})
	c.sink = turnDoneSink{done: done2}
	c.Submit("second")
	waitDone(t, done2)
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("review never fired on second turn")
	}
}

// TestMemoryReviewDisabled: Enabled=false never fires even on tool turns.
func TestMemoryReviewDisabled(t *testing.T) {
	var fires atomic.Int32
	done := make(chan struct{})
	c := memoryReviewController(
		append(toolTurn("c1", "a.txt", "Done."), [][]provider.Chunk{}...),
		&MemoryReviewConfig{Enabled: false, NudgeInterval: 1, MinTurns: 0, Fire: func() { fires.Add(1) }},
		turnDoneSink{done: done},
	)
	c.Submit("do the thing")
	waitDone(t, done)
	time.Sleep(100 * time.Millisecond)
	if fires.Load() != 0 {
		t.Fatalf("disabled review fired %d times", fires.Load())
	}
}

// TestMemoryReviewNoToolsNoFire: a turn without tool calls must not nudge.
func TestMemoryReviewNoToolsNoFire(t *testing.T) {
	var fires atomic.Int32
	done := make(chan struct{})
	c := memoryReviewController(
		[][]provider.Chunk{textTurn("Plain answer.")},
		&MemoryReviewConfig{Enabled: true, NudgeInterval: 1, MinTurns: 0, Fire: func() { fires.Add(1) }},
		turnDoneSink{done: done},
	)
	c.Submit("just answer")
	waitDone(t, done)
	time.Sleep(100 * time.Millisecond)
	if fires.Load() != 0 {
		t.Fatalf("no-tool turn fired the review %d times", fires.Load())
	}
}

func waitDone(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("turn never completed")
	}
}
