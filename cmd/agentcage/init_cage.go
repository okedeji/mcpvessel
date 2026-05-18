package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/go-logr/logr"

	"github.com/okedeji/agentcage/internal/audit"
	"github.com/okedeji/agentcage/internal/cage"
	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/embedded"
	"github.com/okedeji/agentcage/internal/enforcement"
	"github.com/okedeji/agentcage/internal/intervention"
)

type cageRuntimeSetup struct {
	provisioner cage.VMProvisioner
	isolated    bool
	rootfs      *cage.RootfsBuilder
	network     enforcement.NetworkEnforcer
	auditStore  *audit.PGStore
	falcoReader *cage.FalcoAlertReader
}

// Provisioner runs first because its isolated flag decides whether
// we need NFTables and a real rootfs or neither.
func setupCageRuntime(ctx context.Context, cfg *config.Config, db *sql.DB, log logr.Logger) (*cageRuntimeSetup, error) {
	binDir := embedded.BinDir()
	log.Info("embedded bin dir", "path", binDir)

	firecrackerBin := cfg.CageRuntime.FirecrackerBin
	if firecrackerBin == "" {
		firecrackerBin = filepath.Join(binDir, "firecracker")
	}
	kernelBin := cfg.CageRuntime.KernelPath
	if kernelBin == "" {
		kernelBin = filepath.Join(binDir, "vmlinux")
	}

	provisioner, _, err := cage.BuildProvisioner(ctx, cage.HostRuntimeConfig{
		FirecrackerBin: firecrackerBin,
		KernelPath:     kernelBin,
	}, log)
	if err != nil {
		return nil, fmt.Errorf("setting up cage provisioner: %w", err)
	}

	network := enforcement.NewNFTablesEnforcer(log)
	auditStore := audit.NewPGStore(db)

	rootfs, err := buildRootfs(ctx, true, log)
	if err != nil {
		return nil, err
	}

	falcoReader, err := openFalcoReader(ctx, cfg, log)
	if err != nil {
		return nil, err
	}

	return &cageRuntimeSetup{
		provisioner: provisioner,
		isolated:    true,
		rootfs:      rootfs,
		network:     network,
		auditStore:  auditStore,
		falcoReader: falcoReader,
	}, nil
}

// Base image is only required when running real Firecracker.
func buildRootfs(ctx context.Context, isolated bool, log logr.Logger) (*cage.RootfsBuilder, error) {
	baseRootfs := filepath.Join(embedded.VMDir(), "cage-rootfs.img")
	rootfsWorkDir := filepath.Join(embedded.DataDir(), "rootfs-work")
	if err := os.MkdirAll(rootfsWorkDir, 0755); err != nil {
		return nil, fmt.Errorf("creating rootfs work directory: %w", err)
	}
	if isolated {
		if reason := cage.CheckBaseRootfs(baseRootfs); reason != "" {
			return nil, fmt.Errorf("base rootfs not usable (%s): cages cannot be assembled without it", reason)
		}
		log.Info("base rootfs OK", "path", baseRootfs)
	}
	builder := cage.NewRootfsBuilder(baseRootfs, rootfsWorkDir, version)
	if err := builder.SweepStale(ctx, log); err != nil {
		log.Error(err, "sweeping stale rootfs state, continuing")
	}
	return builder, nil
}

// Must run before the Falco daemon starts.
func writeFalcoRules(cfg *config.Config, log logr.Logger) (cage.AlertHandler, error) {
	rules, tripwires := enforcement.GenerateFalcoRules(cfg.Monitoring)
	handler := enforcement.NewFalcoHandlerFromGenerated(tripwires)
	alertHandler := enforcement.NewFalcoAlertAdapter(handler)

	rulesDir := filepath.Join(embedded.RunDir(), "falco", "rules.d")
	if err := enforcement.WriteFalcoRules(rules, rulesDir); err != nil {
		return nil, fmt.Errorf("writing Falco rules: %w", err)
	}
	log.Info("Falco rules written", "dir", rulesDir)
	return alertHandler, nil
}

// Falco is the only behavioral tripwire, so missing Falco is
// fatal in strict posture.
func openFalcoReader(ctx context.Context, cfg *config.Config, log logr.Logger) (*cage.FalcoAlertReader, error) {
	alertFile := filepath.Join(embedded.RunDir(), "falco", "alerts.jsonl")

	// Falco was just started by the embedded manager. Give it up to
	// 10s to create the alert file before declaring failure.
	var found bool
	for attempts := 0; attempts < 20; attempts++ {
		if _, err := os.Stat(alertFile); err == nil {
			found = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !found {
		return nil, fmt.Errorf("falco not usable (alert file not created): behavioral tripwires are required for cage isolation")
	}

	log.Info("Falco alert reader configured", "file", alertFile)
	return cage.NewFalcoAlertReader(alertFile, log), nil
}


// interventionQueueAdapter bridges cage.InterventionEnqueuer (int params)
// and intervention.Queue (typed params) so the cage package does not
// import intervention.
type interventionQueueAdapter struct {
	q *intervention.Queue
}

func (a *interventionQueueAdapter) Enqueue(ctx context.Context, reqType cage.InterventionType, priority cage.InterventionPriority, cageID, assessmentID, description string, contextData []byte, timeout time.Duration) (string, error) {
	req, err := a.q.Enqueue(ctx, intervention.Type(reqType), intervention.Priority(priority), cageID, assessmentID, description, contextData, timeout)
	if err != nil {
		return "", err
	}
	return req.ID, nil
}
