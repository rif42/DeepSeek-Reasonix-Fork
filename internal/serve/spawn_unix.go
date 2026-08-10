//go:build !windows

package serve

import "syscall"

// detachedProcAttr is a no-op on non-Windows: exec.Cmd children already have
// no controlling terminal when spawned from a daemonized parent, and there is
// no job-object concept. breakaway is accepted for signature parity.
func detachedProcAttr(_ bool) *syscall.SysProcAttr { return &syscall.SysProcAttr{} }
