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

	"github.com/okedeji/mcpvessel/internal/config"
	"github.com/okedeji/mcpvessel/internal/history"
	"github.com/okedeji/mcpvessel/internal/runtime"
)

// shutdownTimeout bounds the drain of in-flight control-plane requests after
// ctx is cancelled. Control calls are short; past this the operator's Ctrl-C
// wins over a wedged handler.
const shutdownTimeout = 5 * time.Second

// Serve binds the control plane to the Unix socket at socketPath and serves
// until ctx is cancelled. It refuses to start if another daemon is already
// listening, and clears a stale socket left by a crashed one.
func Serve(ctx context.Context, d *Daemon, socketPath string) error {
	if err := checkSocketPathLen(socketPath); err != nil {
		return err
	}
	// 0700: the control socket drives runs, serves, and shutdown, so its
	// directory is owner-only to keep other local users off the path to it.
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return fmt.Errorf("creating socket dir: %w", err)
	}
	// Re-tighten in case the dir predates this (MkdirAll no-ops on an existing
	// 0755 dir from an older version), so other local users cannot traverse to
	// the socket.
	if err := os.Chmod(filepath.Dir(socketPath), 0o700); err != nil {
		return fmt.Errorf("tightening socket dir perms: %w", err)
	}
	if alreadyListening(socketPath) {
		return fmt.Errorf("a daemon is already listening on %s", socketPath)
	}
	// Nothing answered, so any socket file here is a crash leftover that would
	// block bind with "address already in use". Safe to remove: no live daemon
	// owns it.
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clearing stale socket: %w", err)
	}

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", socketPath, err)
	}
	// Lock the socket to its owner before serving a single request: any local
	// user who could connect could drive the daemon (POST /run, /serve,
	// /shutdown). 0600 here plus the 0700 dir above keep other users out.
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("restricting control socket permissions: %w", err)
	}

	// We own the socket, so any daemon-labeled containers or networks are a
	// crashed predecessor's orphans, safe to remove before accepting runs.
	// Best-effort: a sweep error is logged, not fatal.
	if err := runtime.SweepDaemonOrphans(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "warning: reconciliation sweep: %v\n", err)
	}

	// Any run still marked running at startup had its daemon die under it:
	// reconcile it to crashed. A history that will not open leaves d.hist nil
	// and the daemon serves without it.
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

	// Prometheus scrape endpoint, best-effort: a listener that will not bind
	// warns and the daemon serves runs without it.
	if cfg, err := config.Load(); err == nil {
		if addr := cfg.Telemetry.EffectiveMetricsAddr(); addr != "" {
			if stop := d.startMetrics(addr); stop != nil {
				defer stop()
			}
		}
		// Re-serve the caged docs server if the operator opted in during init, so
		// the client's registered URL keeps working across restarts. In a
		// goroutine because it pulls a bundle, and best-effort: a failure warns
		// and the daemon serves everything else regardless.
		if cfg.Docs.Enabled {
			go func() {
				if err := d.ensureDocsServed(ctx); err != nil {
					fmt.Fprintf(os.Stderr, "warning: serving mcpvessel-docs: %v\n", err)
				}
			}()
		}
	}

	srv := &http.Server{Handler: d.Handler()}
	go func() {
		// An operator signal (ctx) or the control plane's /shutdown bring the
		// daemon down the same way.
		select {
		case <-ctx.Done():
		case <-d.shutdown:
		}
		// Front doors close first: external MCP traffic stops before the runs
		// behind them are released.
		d.closeFronts()
		// Fresh context: ctx is already cancelled, and the drain needs its own
		// deadline.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	// On the way down, release every held run so its detached sub-agents and
	// networks do not leak to the next startup sweep.
	serveErr := srv.Serve(ln)
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return errors.Join(fmt.Errorf("serving control plane: %w", serveErr), d.releaseAll())
	}
	return d.releaseAll()
}

// maxSocketPathLen is the conservative cap on a Unix socket path: macOS allows
// 104 bytes in sun_path, Linux 108.
const maxSocketPathLen = 104

// checkSocketPathLen rejects a socket path the OS could not bind, turning the
// kernel's cryptic "invalid argument" into a clear cause and fix. Bites only
// when VESSEL_HOME points somewhere deep.
func checkSocketPathLen(path string) error {
	if len(path) >= maxSocketPathLen {
		return fmt.Errorf("control socket path is %d bytes, over this OS's %d-byte limit (%s); set VESSEL_HOME to a shorter directory",
			len(path), maxSocketPathLen, path)
	}
	return nil
}

// alreadyListening reports whether a live daemon answers on socketPath,
// distinguishing a running daemon from a stale socket file.
func alreadyListening(socketPath string) bool {
	conn, err := net.DialTimeout("unix", socketPath, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
