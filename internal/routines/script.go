package routines

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// SilenceMarker is the token a routine's output uses to suppress delivery
// (mirrors Hermes' [SILENT] convention): the job ran, but there is nothing
// worth delivering.
const SilenceMarker = "[SILENT]"

// IsSilenceResponse reports whether content carries the silence marker.
func IsSilenceResponse(content string) bool {
	return strings.Contains(content, SilenceMarker)
}

// RunScript executes a pre-run script and returns its stdout. The script must
// be an absolute path, or relative to workdir (default: current directory).
// A non-existent path is rejected up front (path-traversal guard: relative
// paths may not escape workdir). .sh/.bash scripts run via bash; everything
// else is executed directly.
func RunScript(ctx context.Context, scriptPath, workdir string) (string, error) {
	if strings.TrimSpace(scriptPath) == "" {
		return "", errors.New("script path is empty")
	}
	resolved, err := resolveScriptPath(scriptPath, workdir)
	if err != nil {
		return "", err
	}
	cmd := scriptCommand(ctx, resolved)
	if workdir != "" {
		if st, err := os.Stat(workdir); err == nil && st.IsDir() {
			cmd.Dir = workdir
		}
	}
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("script %s: %w", scriptPath, err)
	}
	return out.String(), nil
}

// scriptCommand builds the exec.Cmd for a script path.
func scriptCommand(ctx context.Context, path string) *exec.Cmd {
	ext := strings.ToLower(filepath.Ext(path))
	if (ext == ".sh" || ext == ".bash") && runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "bash", path)
	}
	return exec.CommandContext(ctx, path)
}

// resolveScriptPath makes scriptPath absolute and verifies it exists. Relative
// paths resolve against workdir and must stay inside it.
func resolveScriptPath(scriptPath, workdir string) (string, error) {
	if !filepath.IsAbs(scriptPath) {
		base := workdir
		if base == "" {
			base = "."
		}
		absBase, err := filepath.Abs(base)
		if err != nil {
			return "", fmt.Errorf("resolve script workdir: %w", err)
		}
		resolved := filepath.Clean(filepath.Join(absBase, scriptPath))
		if !strings.HasPrefix(resolved, absBase+string(filepath.Separator)) && resolved != absBase {
			return "", fmt.Errorf("script %q escapes workdir %q", scriptPath, absBase)
		}
		scriptPath = resolved
	}
	if _, err := os.Stat(scriptPath); err != nil {
		return "", fmt.Errorf("script %q not found", scriptPath)
	}
	return scriptPath, nil
}

// defaultScriptTimeout bounds script execution when the caller provides no
// context deadline.
const defaultScriptTimeout = 10 * time.Minute

// RunScriptWithTimeout is RunScript with an explicit timeout applied when ctx
// has no deadline yet.
func RunScriptWithTimeout(ctx context.Context, scriptPath, workdir string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = defaultScriptTimeout
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	return RunScript(ctx, scriptPath, workdir)
}
