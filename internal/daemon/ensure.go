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

// Command is the subcommand that runs the daemon, the name Ensure spawns and the
// CLI registers, kept in one place so the launch side and the command side
// cannot drift.
const Command = "daemon"

// startTimeout bounds the wait for a freshly spawned daemon to bind its socket.
// Binding is near-instant, so this only outlasts process startup; a hang fails
// fast with a pointer at the log rather than waiting forever.
const startTimeout = 5 * time.Second

// stopTimeout bounds the wait for a daemon to stop answering after a shutdown
// request. Graceful shutdown releases held runs first, which can take a moment
// per run, so this is more generous than startup.
const stopTimeout = 30 * time.Second

// Stop asks a running daemon to shut down and waits until it stops answering. It
// is a no-op when no daemon is running. The shutdown ack can race the socket
// closing, so success is confirmed by polling, not by the request's result.
func Stop(ctx context.Context) error {
	socket, err := SocketPath()
	if err != nil {
		return err
	}
	c := Dial(socket)
	if !answers(ctx, c) {
		return nil
	}
	_ = c.Shutdown(ctx)

	deadline := time.Now().Add(stopTimeout)
	for {
		if !answers(ctx, c) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("daemon did not stop within %s; check ~/.agentcage/daemon.log", stopTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Ensure returns a client for the daemon, starting it if it is not already
// listening. The daemon is spawned detached so it outlives the caller, with its
// output appended to ~/.agentcage/daemon.log. Commands that need the daemon
// call this so an operator never has to start it by hand.
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

// answers reports whether a daemon is listening and responding, the cheap
// version round-trip Ensure uses to decide whether to spawn one.
func answers(ctx context.Context, c *Client) bool {
	pingCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	_, err := c.Version(pingCtx)
	return err == nil
}

// spawn starts the daemon as a detached background process that survives the
// caller exiting. It runs the same binary, so the daemon is always the
// installed version, and inherits the environment so AGENTCAGE_HOME (and thus
// the socket and store paths) match.
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
