package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// shutdownTimeout bounds how long Serve waits for in-flight control-plane
// requests to drain after ctx is cancelled. Control calls are short (ps, stop,
// budget), so a few seconds is generous; past it the operator's Ctrl-C should
// win over a wedged handler rather than hang.
const shutdownTimeout = 5 * time.Second

// Serve binds the control plane to the Unix socket at socketPath and serves
// until ctx is cancelled, then drains in-flight requests within
// shutdownTimeout. It refuses to start if another daemon is already listening,
// and clears a stale socket left by a crashed one.
func Serve(ctx context.Context, d *Daemon, socketPath string) error {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return fmt.Errorf("creating socket dir: %w", err)
	}
	if alreadyListening(socketPath) {
		return fmt.Errorf("a daemon is already listening on %s", socketPath)
	}
	// Nothing answered, so any socket file here is stale from a crash and
	// blocks bind with "address already in use". Removing it is safe now that
	// we have confirmed no live daemon owns it.
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clearing stale socket: %w", err)
	}

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", socketPath, err)
	}

	srv := &http.Server{Handler: d.Handler()}
	go func() {
		<-ctx.Done()
		// Fresh context: the boot ctx is already cancelled, but the drain still
		// needs its own deadline to finish or give up.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving control plane: %w", err)
	}
	return nil
}

// alreadyListening reports whether a live daemon answers on socketPath. A
// successful dial means one is running; a refused or missing socket means it is
// safe to bind. It distinguishes a running daemon from a stale socket file.
func alreadyListening(socketPath string) bool {
	conn, err := net.DialTimeout("unix", socketPath, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
