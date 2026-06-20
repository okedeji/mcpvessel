//go:build windows

package daemon

import "syscall"

// detachAttrs is a no-op on Windows, where the agentcage runtime is not yet
// supported; the daemon is a macOS and Linux concern for now.
func detachAttrs() *syscall.SysProcAttr {
	return nil
}
