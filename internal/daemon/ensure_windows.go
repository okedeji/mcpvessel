//go:build windows

package daemon

import "syscall"

// detachAttrs is a no-op on Windows, where the agentcage runtime is not
// supported.
func detachAttrs() *syscall.SysProcAttr {
	return nil
}
