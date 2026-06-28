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

	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/history"
	"github.com/okedeji/agentcage/internal/runtime"
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

	// We own the socket now, so no other daemon is serving and any
	// daemon-labeled containers or networks are a crashed predecessor's orphans,
	// safe to remove before we start accepting runs. Best-effort: a sweep error
	// is logged, not fatal.
	if err := runtime.SweepDaemonOrphans(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "warning: reconciliation sweep: %v\n", err)
	}

	// Open the durable run history and reconcile any run still marked running: at
	// startup that is a run whose daemon died under it, so it is crashed. Best
	// effort, like the sweep above. History that will not open leaves d.hist nil
	// and the daemon serves without it rather than refusing to start.
	if path, err := history.DefaultPath(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: run history path: %v\n", err)
	} else if store, err := history.Open(path); err != nil {
		fmt.Fprintf(os.Stderr, "warning: opening run history: %v\n", err)
	} else {
		d.hist = store
		defer func() { _ = store.Close() }()
		if n, err := store.ReconcileRunning(nowFunc()); err != nil {
			fmt.Fprintf(os.Stderr, "warning: reconciling run history: %v\n", err)
		} else if n > 0 {
			fmt.Fprintf(os.Stderr, "reconciled %d crashed run(s) from a previous daemon\n", n)
		}
	}

	// A Prometheus scrape endpoint on its own TCP listener, only when the operator
	// asked for one. Best-effort: a metrics listener that will not bind warns and
	// the daemon serves runs without it.
	if cfg, err := config.Load(); err == nil && cfg.Telemetry.MetricsAddr != "" {
		if stop := d.startMetrics(cfg.Telemetry.MetricsAddr); stop != nil {
			defer stop()
		}
	}

	srv := &http.Server{Handler: d.Handler()}
	go func() {
		// Either an operator signal (ctx) or an in-process shutdown request (the
		// control plane's /shutdown) brings the daemon down the same way.
		select {
		case <-ctx.Done():
		case <-d.shutdown:
		}
		// Close the front doors first so external MCP traffic stops before the
		// runs behind them are released, then drain the control plane.
		d.closeFronts()
		// Fresh context: the boot ctx is already cancelled, but the drain still
		// needs its own deadline to finish or give up.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	// On the way down, release every run still held so its detached sub-agents
	// and networks come down with the daemon rather than leaking to the next
	// startup sweep.
	serveErr := srv.Serve(ln)
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return errors.Join(fmt.Errorf("serving control plane: %w", serveErr), d.releaseAll())
	}
	return d.releaseAll()
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
