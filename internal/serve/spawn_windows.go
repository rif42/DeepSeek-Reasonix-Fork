//go:build windows

package serve

import "syscall"

// detachedProcAttr runs a child without a console window of its own, detached
// from the parent's console, in its own process group (so Ctrl+C in the parent
// never reaches children). With breakaway, the child also escapes a parent job
// object that permits it (JOB_OBJECT_LIMIT_BREAKAWAY_OK) so a kill-on-close
// job cannot take the child down with the parent. syscall.DETACHED_PROCESS is
// not exported; 0x8 is its value, 0x200 is CREATE_NEW_PROCESS_GROUP, and
// 0x01000000 is CREATE_BREAKAWAY_FROM_JOB.
func detachedProcAttr(breakaway bool) *syscall.SysProcAttr {
	flags := uint32(syscall.CREATE_NEW_PROCESS_GROUP) | 0x8
	if breakaway {
		flags |= 0x01000000
	}
	return &syscall.SysProcAttr{CreationFlags: flags}
}
