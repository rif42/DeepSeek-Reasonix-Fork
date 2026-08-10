package serve

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// spawnConfig identifies the parent for the /spawn-session endpoint: children
// are launched as separate `reasonix serve` processes on loopback ports above
// the parent's own port, so each gets its own controller and SSE stream. A
// zero base disables the endpoint (tests, embedded use).
type spawnConfig struct {
	base         string // parent listen address, e.g. "127.0.0.1:8787"
	host         string // loopback host children bind (always 127.0.0.1)
	basePort     int    // parent port; children scan upward from basePort+1
	idleShutdown time.Duration
	childStartAt int // first candidate port for children
}

func parseSpawnBase(addr string) (host string, port int, err error) {
	if strings.TrimSpace(addr) == "" {
		return "", 0, fmt.Errorf("spawn: empty listen address")
	}
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, fmt.Errorf("spawn: parse listen address %q: %w", addr, err)
	}
	port, err = strconv.Atoi(p)
	if err != nil {
		return "", 0, fmt.Errorf("spawn: parse listen port %q: %w", p, err)
	}
	if h == "" || h == "0.0.0.0" || h == "::" {
		h = "127.0.0.1" // children always bind loopback
	}
	return h, port, nil
}

// findFreePort returns the first free TCP port at or above from on host, or an
// error once attempts consecutive ports have all been taken.
func findFreePort(host string, from, attempts int) (int, error) {
	for i := 0; i < attempts; i++ {
		port := from + i
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			continue
		}
		_ = ln.Close()
		return port, nil
	}
	return 0, fmt.Errorf("spawn: no free port in %d..%d", from, from+attempts-1)
}

// childSpec describes one spawned child instance.
type childSpec struct {
	host         string
	port         int
	exe          string        // default: os.Executable()
	idleShutdown time.Duration // forwarded as --idle-shutdown; 0 = omitted
}

func (c childSpec) args() []string {
	args := []string{"serve", "--addr", net.JoinHostPort(c.host, strconv.Itoa(c.port))}
	if c.idleShutdown > 0 {
		args = append(args, "--idle-shutdown", c.idleShutdown.String())
	}
	return args
}

// spawnChild starts a detached `reasonix serve` child and returns its cmd so
// the caller can kill it if health checks fail. The child shares the parent's
// environment and config (same REASONIX_HOME / config.toml), and is marked
// with REASONIX_SPAWNED_CHILD so it can self-clean its registry entry.
func spawnChild(ctx context.Context, c childSpec) (*exec.Cmd, error) {
	exe := c.exe
	if exe == "" {
		var err error
		exe, err = os.Executable()
		if err != nil {
			return nil, fmt.Errorf("spawn: resolve own executable: %w", err)
		}
	}
	start := func(attr *syscall.SysProcAttr) (*exec.Cmd, error) {
		cmd := exec.CommandContext(ctx, exe, c.args()...)
		cmd.SysProcAttr = attr
		cmd.Env = append(os.Environ(), "REASONIX_SPAWNED_CHILD=1")
		// TEMP diagnosis: capture child output when REASONIX_SPAWN_CHILD_LOG is set.
		if logPath := os.Getenv("REASONIX_SPAWN_CHILD_LOG"); logPath != "" {
			if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
				cmd.Stdout = f
				cmd.Stderr = f
			}
		}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return cmd, nil
	}
	// Prefer a breakaway spawn so a kill-on-close parent job (supervisors,
	// launchers) cannot take the child down with the parent. A job without
	// JOB_OBJECT_LIMIT_BREAKAWAY_OK refuses that flag (ERROR_ACCESS_DENIED),
	// so fall back to a plain detached spawn — the child still starts, it
	// just stays inside the parent job.
	cmd, err := start(detachedProcAttr(true))
	if err != nil {
		cmd, err = start(detachedProcAttr(false))
	}
	if err != nil {
		return nil, fmt.Errorf("spawn %s: %w", exe, err)
	}
	return cmd, nil
}

// waitHealthy polls url+"/status" until it returns 200, giving up after timeout
// with 250 ms between probes. ctx bounds the whole wait.
func waitHealthy(ctx context.Context, url string, timeout, interval time.Duration) error {
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		resp, err := client.Get(url + "/status")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("spawn: child %s not healthy within %s", url, timeout)
		}
		time.Sleep(interval)
	}
}
