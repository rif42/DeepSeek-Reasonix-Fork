package routines

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/headless"
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
	res, err := headless.Run(ctx, boot.Options{
		Model:         opts.Model,
		MaxSteps:      r.MaxSteps,
		RequireKey:    true,
		WorkspaceRoot: r.WorkspaceRoot,
		SessionDir:    r.SessionDir,
		Stderr:        r.Stderr,
	}, opts.Prompt)
	if err != nil {
		return RunResult{}, fmt.Errorf("routines: agent run: %w", err)
	}
	return RunResult{FinalResponse: res.FinalResponse}, nil
}

// RunPrompt runs one headless turn with the given prompt and returns the final
// response text. It satisfies memoryreview.Runner for the background review
// pass (same cache-warm boot path).
func (r *AgentRunner) RunPrompt(ctx context.Context, prompt, model string) (string, error) {
	res, err := r.Run(ctx, RunOptions{Prompt: prompt, Model: strings.TrimSpace(model)})
	if err != nil {
		return "", err
	}
	return res.FinalResponse, nil
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
