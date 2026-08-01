// Package headless runs one unattended agent turn through the standard boot
// path. It exists as a leaf over boot.Build so both the routines engine and
// the boot-time memory-review wiring can share it without an import cycle:
// routines -> headless -> boot and boot -> headless are both acyclic.
package headless

import (
	"context"
	"strings"
	"sync"

	"reasonix/internal/boot"
	"reasonix/internal/control"
	"reasonix/internal/event"
)

// Result carries the captured final answer of the run.
type Result struct {
	FinalResponse string
}

// Run builds a headless controller via boot.Build — the same assembly as
// `reasonix run`, so the LLM call shares the byte-identical system-prompt
// prefix with interactive sessions (prompt-cache warmth) — executes one turn
// under auto-approval, and returns the captured final response text.
func Run(ctx context.Context, opts boot.Options, prompt string) (Result, error) {
	sink := &captureSink{}
	opts.Sink = sink
	opts.HeadlessApprovalMode = control.ToolApprovalAuto
	ctrl, err := boot.Build(ctx, opts)
	if err != nil {
		return Result{}, err
	}
	defer ctrl.Close()
	ctrl.ApplyHeadlessApprovalMode(control.ToolApprovalAuto)
	if err := ctrl.Run(ctx, prompt); err != nil {
		return Result{}, err
	}
	return Result{FinalResponse: sink.finalResponse()}, nil
}

// captureSink records the assistant's final answer (the Message event carries
// the complete turn text).
type captureSink struct {
	mu       sync.Mutex
	response strings.Builder
}

func (c *captureSink) Emit(e event.Event) {
	if e.Kind != event.Message {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.response.Reset()
	c.response.WriteString(e.Text)
}

func (c *captureSink) finalResponse() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.TrimSpace(c.response.String())
}
