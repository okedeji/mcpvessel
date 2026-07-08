package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/okedeji/agentcage/internal/env"
)

// Command is the subcommand that runs the daemon: the name Ensure spawns and
// the CLI registers, kept in one place so they cannot drift.
const Command = "daemon"

// startTimeout bounds the wait for a freshly spawned daemon to bind its
// socket. Binding is near-instant; this only outlasts process startup.
const startTimeout = 5 * time.Second

// stopTimeout bounds the wait for a daemon to stop answering. Graceful
// shutdown releases held runs first, so this is more generous than startup.
const stopTimeout = 30 * time.Second

// Stop asks a running daemon to shut down and waits until it stops answering.
// It reports whether a daemon was actually running. The shutdown ack can race
// the socket closing, so success is confirmed by polling, not by the request's
// result.
func Stop(ctx context.Context) (stopped bool, err error) {
	socket, err := SocketPath()
	if err != nil {
		return false, err
	}
	c := Dial(socket)
	if !answers(ctx, c) {
		return false, nil
	}
	_ = c.Shutdown(ctx)

	deadline := time.Now().Add(stopTimeout)
	for {
		if !answers(ctx, c) {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, fmt.Errorf("daemon did not stop within %s; check ~/.agentcage/daemon.log", stopTimeout)
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Ensure returns a client for the daemon, starting one if none is listening.
// The daemon is spawned detached, its output appended to
// ~/.agentcage/daemon.log.
func Ensure(ctx context.Context) (*Client, error) {
	socket, err := SocketPath()
	if err != nil {
		return nil, err
	}
	c := Dial(socket)
	if answers(ctx, c) {
		return c, nil
	}

	if err := spawn(); err != nil {
		return nil, err
	}

	deadline := time.Now().Add(startTimeout)
	for {
		if answers(ctx, c) {
			return c, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("daemon did not come up within %s; check ~/.agentcage/daemon.log", startTimeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// answers reports whether a daemon is listening and responding.
func answers(ctx context.Context, c *Client) bool {
	pingCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	_, err := c.Version(pingCtx)
	return err == nil
}

// spawn starts the daemon detached so it survives the caller exiting. It runs
// the same binary and inherits the environment, so the version and
// AGENTCAGE_HOME (socket and store paths) match.
func spawn() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating the agentcage binary: %w", err)
	}
	home, err := env.HomeDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return fmt.Errorf("creating agentcage home: %w", err)
	}
	logFile, err := os.OpenFile(filepath.Join(home, "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("opening daemon log: %w", err)
	}
	defer func() { _ = logFile.Close() }()

	cmd := exec.Command(exe, Command)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = detachAttrs()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting the daemon: %w", err)
	}
	// Release so the daemon is reparented and keeps running after we return.
	return cmd.Process.Release()
}
