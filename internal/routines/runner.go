package routines

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"

	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
)

// AgentRunner runs one job headlessly through the standard Reasonix boot
// path — the same assembly `reasonix run` uses — with a capturing sink and a
// headless approval gate, so an unattended routine can never wedge on a prompt
// no one is there to answer.
type AgentRunner struct {
	// WorkspaceRoot is the project root for the run (config, sandbox, tools).
	WorkspaceRoot string
	// MaxSteps caps tool-call rounds (0 = automatic).
	MaxSteps int
	// SessionDir overrides where routine transcripts are written. Empty uses
	// the default shared session directory.
	SessionDir string
	// Stderr receives diagnostics and plugin stderr. Nil = os.Stderr.
	Stderr io.Writer
}

// Run executes one headless agent turn and returns the final response text.
func (r *AgentRunner) Run(ctx context.Context, opts RunOptions) (RunResult, error) {
	sink := &captureSink{}
	ctrl, err := boot.Build(ctx, boot.Options{
		Model:                opts.Model,
		MaxSteps:             r.MaxSteps,
		RequireKey:           true,
		Sink:                 sink,
		WorkspaceRoot:        r.WorkspaceRoot,
		SessionDir:           r.SessionDir,
		Stderr:               r.Stderr,
		HeadlessApprovalMode: control.ToolApprovalAuto,
	})
	if err != nil {
		return RunResult{}, fmt.Errorf("routines: boot agent: %w", err)
	}
	defer ctrl.Close()
	ctrl.ApplyHeadlessApprovalMode(control.ToolApprovalAuto)
	if err := ctrl.Run(ctx, opts.Prompt); err != nil {
		return RunResult{}, fmt.Errorf("routines: agent run: %w", err)
	}
	return RunResult{FinalResponse: sink.finalResponse()}, nil
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

// RoutinesDataDir returns the directory where routine state lives:
// <reasonix-home>/routines.
func RoutinesDataDir() string {
	home := config.ReasonixHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, "routines")
}
