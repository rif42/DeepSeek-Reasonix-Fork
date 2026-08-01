package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/pflag"

	"reasonix/internal/agent"
	"reasonix/internal/config"
	"reasonix/internal/i18n"
	"reasonix/internal/memory"
	"reasonix/internal/memoryreview"
	"reasonix/internal/routines"
)

// memoryCommand implements `reasonix memory` — the on-demand trigger for the
// background memory review loop. (Note: /memory inside the interactive TUI is
// handled by chatTUI.showMemory in memory.go; this is the CLI entry point.)
func memoryCommand(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: reasonix memory review [--session PATH] [--model NAME] [--dir ROOT] [--no-apply]")
		return 2
	}
	switch args[0] {
	case "review":
		return memoryReviewCommand(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown memory subcommand %q (expected review)\n", args[0])
		return 2
	}
}

// memoryReviewCommand runs one review pass over a session transcript and
// persists the distilled facts to auto-memory.
func memoryReviewCommand(args []string) int {
	fs := pflag.NewFlagSet("memory review", pflag.ContinueOnError)
	fs.SetInterspersed(true)
	session := fs.String("session", "", "session file to review (default: most recent session in the CLI session dir)")
	model := fs.String("model", "", "review model (default: config default_model)")
	dir := fs.String("dir", "", "project root; config, sandbox and file tools resolve from here")
	noApply := fs.Bool("no-apply", false, "distill and print facts without writing them to memory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if rc := chdirTo(*dir); rc != 0 {
		return rc
	}
	workspaceRoot, err := workspaceRootForDir(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 1
	}
	cfg, _ := config.Load()

	sessionPath := strings.TrimSpace(*session)
	if sessionPath == "" {
		sessions, err := agent.ListSessions(resolveCLISessionDir())
		if err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, "list sessions:", err)
			return 1
		}
		if len(sessions) == 0 {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, "no sessions to review; run an interactive session first, or pass --session")
			return 1
		}
		latest := sessions[0]
		for _, s := range sessions[1:] {
			if s.LastActivityAt.After(latest.LastActivityAt) {
				latest = s
			}
		}
		sessionPath = latest.Path
	}
	if abs, err := filepath.Abs(sessionPath); err == nil {
		sessionPath = abs
	}

	runner := &routines.AgentRunner{WorkspaceRoot: workspaceRoot, MaxSteps: 1, Stderr: os.Stderr}
	st := memory.StoreFor(config.MemoryUserDir(), workspaceRoot)
	rv := &memoryreview.Reviewer{
		Runner:             runner,
		Store:              st,
		Model:              strings.TrimSpace(*model),
		MaxTranscriptChars: cfg.Memory.ReviewMaxTranscriptChars,
		DryRun:             *noApply,
		Stderr:             os.Stderr,
	}
	fmt.Fprintf(os.Stderr, "reviewing session %s...\n", sessionPath)
	res, report, err := rv.Review(context.Background(), sessionPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 1
	}
	if *noApply {
		fmt.Printf("dry run: distilled %d fact(s), not written\n", len(res.Facts))
		for _, f := range res.Facts {
			fmt.Printf("  - [%s/%s] %s: %s\n", f.Type, f.Scope, f.Name, f.Description)
		}
		return 0
	}
	fmt.Printf("memory review complete: created=%d updated=%d skipped=%d\n", report.Created, report.Updated, report.Skipped)
	for _, f := range res.Facts {
		fmt.Printf("  - [%s/%s] %s: %s\n", f.Type, f.Scope, f.Name, f.Description)
	}
	for _, e := range report.Errors {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, e)
	}
	return 0
}
