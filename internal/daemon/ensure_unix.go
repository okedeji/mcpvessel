//go:build !windows

package daemon

import "syscall"

// detachAttrs puts the spawned daemon in its own session; a Ctrl-C on the
// launching terminal must not kill it.
func detachAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
