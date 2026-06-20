//go:build !windows

package daemon

import "syscall"

// detachAttrs puts the spawned daemon in its own session, so a Ctrl-C on the
// terminal that started it does not also kill the daemon it just launched.
func detachAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
