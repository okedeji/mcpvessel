package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-logr/logr"

	pb "github.com/okedeji/agentcage/api/proto"
	"github.com/okedeji/agentcage/internal/assessment"
	"github.com/okedeji/agentcage/internal/cage"
	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/embedded"
	"github.com/okedeji/agentcage/internal/enforcement"
	"github.com/okedeji/agentcage/internal/findings"
	"github.com/okedeji/agentcage/internal/fleet"
	agentgrpc "github.com/okedeji/agentcage/internal/grpc"
	"github.com/okedeji/agentcage/internal/identity"
	"github.com/okedeji/agentcage/internal/intervention"
	proxylog "github.com/okedeji/agentcage/internal/log"
	"github.com/okedeji/agentcage/internal/ui"
)

func cmdInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	configFile := fs.String("config", "", "path to config YAML override file")
	grpcAddr := fs.String("grpc-addr", "", "override gRPC listen address (e.g. 0.0.0.0:9090)")
	secretsFile := fs.String("secrets", "", "path to secrets file (KEY=VALUE lines, seeded into Vault on first boot)")
	debug := fs.Bool("debug", false, "show structured logs on stderr in addition to log file")
	verboseFlag := fs.Bool("verbose", false, "show step-by-step startup progress")
	detach := fs.Bool("detach", false, "run in background")
	_ = fs.Parse(args)

	if *detach {
		detachProcess(append([]string{"init"}, args...))
	}

	ui.SetVerbose(*verboseFlag || *debug)

	if err := runInit(*configFile, *grpcAddr, *secretsFile, *debug); err != nil {
		ui.Fail("%v", err)
		os.Exit(1)
	}
}

func runInit(configFile, grpcAddr, secretsFile string, debug bool) (initErr error) {
	defaultPath := config.DefaultPath()
	created, err := config.WriteDefaults(defaultPath)
	if err != nil {
		return fmt.Errorf("writing default config: %w", err)
	}
	if created {
		ui.Step("Config written to %s", defaultPath)
	}
	cfg := config.Defaults()
	if resolved := config.Resolve(configFile); resolved != "" {
		override, loadErr := config.Load(resolved)
		if loadErr != nil {
			return fmt.Errorf("loading config %s: %w", resolved, loadErr)
		}
		cfg = config.Merge(cfg, override)
	}

	logPath := filepath.Join(embedded.LogDir(), "orchestrator.log")
	var log logr.Logger
	if debug {
		log, err = proxylog.NewFileAndStderr(logPath)
	} else {
		log, err = proxylog.NewFile(logPath)
	}
	if err != nil {
		return fmt.Errorf("creating logger: %w", err)
	}
	log = log.WithValues("component", "agentcage")

	ui.Header(version)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startTime := time.Now()
	var progress *ui.ProgressLine
	if !ui.IsVerbose() {
		progress = ui.Progress("Starting")
	}
	defer func() {
		if initErr != nil && progress != nil {
			progress.Fail()
		}
	}()

	// Falco rules go to disk before the daemon starts. Otherwise it
	// misses the first batch of cage events.
	alertHandler, err := writeFalcoRules(cfg, log)
	if err != nil {
		return err
	}

	embeddedMgr := embedded.NewManager(cfg, log, version)
	if err := embeddedMgr.Download(ctx); err != nil {
		return fmt.Errorf("downloading dependencies: %w", err)
	}
	if err := embeddedMgr.Start(ctx); err != nil {
		return fmt.Errorf("starting local services: %w", err)
	}

	spireSocket := resolveSpireSocket(cfg)
	trustDomain := resolveTrustDomain(cfg)

	otelShutdown, err := setupTelemetry(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer otelShutdown()

	opaEngine, err := buildPolicyEngine(cfg)
	if err != nil {
		return err
	}

	// Identity and secrets must resolve before database and NATS because
	// external service URLs (with embedded credentials) live in Vault.
	svidIssuer, secretFetcher, secretReader, identityCleanup, err := connectIdentityAndSecrets(ctx, cfg, embeddedMgr, spireSocket, log)
	if err != nil {
		return err
	}

	if secretsFile != "" {
		if err := seedSecrets(ctx, secretReader, secretsFile); err != nil {
			identityCleanup()
			return fmt.Errorf("seeding secrets: %w", err)
		}
	}

	if valErr := validateRequiredSecrets(ctx, secretReader, cfg); valErr != nil {
		identityCleanup()
		return valErr
	}

	natsURL, err := resolveNATSURL(ctx, cfg, secretReader)
	if err != nil {
		identityCleanup()
		return err
	}

	db, err := connectDatabase(ctx, cfg, secretReader, log)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	findingsBus, findingStore, findingsCoordinator, err := connectFindingsBus(ctx, cfg, natsURL, spireSocket, trustDomain, db, log)
	if err != nil {
		return err
	}
	defer findingsBus.Close()

	temporalClient, temporalNamespace, err := connectTemporal(ctx, cfg, secretReader, spireSocket, trustDomain, log)
	if err != nil {
		return err
	}
	defer temporalClient.Close()

	iStore, notifier, alertDispatcher := setupNotifications(db, cfg, log)
	scopeValidator := enforcement.NewScopeValidator(cfg)
	cageValidator := buildCageValidator(cfg, opaEngine, scopeValidator, alertDispatcher)

	fleetSetup, err := setupFleet(ctx, cfg, embeddedMgr, secretReader, alertDispatcher, log)
	if err != nil {
		return err
	}
	if fleetSetup.autoscaler != nil {
		autoscalerLog := log.WithValues("component", "autoscaler")
		go func() {
			if err := fleetSetup.autoscaler.Run(ctx); err != nil {
				autoscalerLog.Error(err, "autoscaler stopped, triggering orchestrator shutdown")
			} else {
				autoscalerLog.Info("autoscaler stopped")
			}
			if ctx.Err() == nil {
				cancel()
			}
		}()
	}

	configServer := config.NewServer(cfg)

	llmClient, tokenMeter, llmAPIKey, err := buildLLMClient(ctx, cfg, configServer, secretReader, alertDispatcher, log)
	if err != nil {
		return err
	}

	var judgeAPIKey string
	if secretReader != nil && cfg.JudgeEndpoint() != "" {
		judgeAPIKey, err = identity.ReadSecretValue(ctx, secretReader, identity.PathJudgeKey)
		if err != nil {
			return fmt.Errorf("reading judge API key from Vault: %w", err)
		}
	}

	cageSvc := cage.NewService(temporalClient, cageValidator, db, func() string { return configServer.GetConfig(ctx).LLM.Endpoint }, llmAPIKey, judgeAPIKey, natsURL, cfg.InterventionHoldsEnabled(), cage.TimeoutsFromConfig(cfg.Timeouts), cfg.InterventionTimeout())
	fleetSvc := fleet.NewService(fleetSetup.pool, fleetSetup.demand, fleetSetup.provisioner, log.WithValues("component", "fleet"))
	var fleetSignaler assessment.FleetSignaler
	if fleetSetup.autoscaler != nil {
		fleetSignaler = fleetSetup.autoscaler
	}
	assessmentSvc := assessment.NewService(temporalClient, db, fleetSignaler, cfg)

	iQueue := intervention.NewQueue(iStore, notifier, log.WithValues("component", "intervention-queue"))
	iSvc := intervention.NewService(iQueue, temporalClient, log.WithValues("component", "intervention-service"))

	cageRuntime, err := setupCageRuntime(ctx, cfg, db, log)
	if err != nil {
		return err
	}

	payloadHoldHandler := cage.NewPayloadHoldHandler(cage.PayloadHoldConfig{
		Enqueuer:        &interventionQueueAdapter{q: iQueue},
		InterventionTTL: cfg.InterventionTimeout(),
		Log:             log,
	})
	iSvc.SetPayloadHoldResolver(payloadHoldHandler)

	agentHoldListener := cage.NewAgentHoldListener(cage.AgentHoldListenerConfig{
		Enqueuer:        &interventionQueueAdapter{q: iQueue},
		InterventionTTL: cfg.InterventionTimeout(),
		Log:             log,
	})
	iSvc.SetAgentHoldResolver(agentHoldListener)

	cageLogDir := filepath.Join(embedded.DataDir(), "cage-logs")
	fileSink, err := cage.NewFileSink(cageLogDir)
	if err != nil {
		return fmt.Errorf("creating cage log sink: %w", err)
	}
	defer fileSink.Close()

	sinks := []cage.LogSink{fileSink}
	if nb, ok := findingsBus.(*findings.NATSBus); ok {
		sinks = append(sinks, cage.NewNATSLogSink(nb.Conn()))
	}
	if cfg.Infrastructure.IsExternalOTel() {
		sinks = append(sinks, cage.NewOTelLogSink())
	}
	var logSink cage.LogSink = fileSink
	if len(sinks) > 1 {
		logSink = cage.NewMultiSink(sinks...)
	}
	logCollector := cage.NewVsockCollector(log.WithValues("component", "vsock-collector"), logSink)

	cageActivityImpl := cage.NewActivityImpl(cage.ActivityImplConfig{
		Provisioner:       cageRuntime.provisioner,
		Rootfs:            cageRuntime.rootfs,
		BundleStoreDir:    filepath.Join(embedded.DataDir(), "bundles"),
		Network:           cageRuntime.network,
		Validator:         scopeValidator,
		AlertHandler:      alertHandler,
		AlertNotifier:     alertDispatcher,
		FalcoReader:       cageRuntime.falcoReader,
		FleetPool:         fleet.NewCagePoolAdapter(fleetSetup.pool),
		AuditStore:        cageRuntime.auditStore,
		Identity:          svidIssuer,
		Secrets:           secretFetcher,
		InterventionQueue: &interventionQueueAdapter{q: iQueue},
		PayloadHolds:      payloadHoldHandler,
		AgentHolds:        agentHoldListener,
		LogCollector:      logCollector,
		FindingsBus:       findingsBus,
		TokenMeter:        tokenMeter,
		CageService:       cageSvc,
		LogDir:            cageLogDir,
		Log:               log,
	})

	assessmentActivityImpl := assessment.NewActivityImpl(assessment.ActivityImplConfig{
		Cages:         cageSvc,
		Findings:      findingStore,
		Bus:           findingsBus,
		Coordinator:   findingsCoordinator,
		Fleet:         fleetSignaler,
		Assessments:   assessmentSvc,
		Tokens:        tokenMeter,
		LLMClient:     llmClient,
		ConfigServer:  configServer,
		Alerter:       alertDispatcher,
		Interventions: &interventionQueueAdapter{q: iQueue},
		ReviewTimeout: cfg.Assessment.ReviewTimeout,
		Log:           log,
	})

	configYAML, _ := config.Marshal(cfg)

	var endpoints *pb.ServiceEndpoints
	if cfg.Infrastructure.IsMultiMachine() {
		addr := cfg.Infrastructure.AdvertiseAddress
		endpoints = &pb.ServiceEndpoints{
			SpireServer: fmt.Sprintf("%s:%s", addr, embedded.SPIREServerPort()),
			NomadServer: fmt.Sprintf("http://%s:%s", addr, embedded.NomadPort()),
		}
	}

	grpcServer, err := buildGRPCServer(ctx, cfg, agentgrpc.Services{
		Cages:         cageSvc,
		Assessments:   assessmentSvc,
		Interventions: iSvc,
		Fleet:         fleetSvc,
		Findings:      findingStore,
		Audit:         cageRuntime.auditStore,
		Pack: agentgrpc.PackConfig{
			BundleStoreDir:   filepath.Join(embedded.DataDir(), "bundles"),
			SDKTarball:       filepath.Join(embedded.BinDir(), "agentcage-sdk.tgz"),
			AgentcageVersion: version,
		},
		SecretReader:     secretReader,
		ConfigServer:     configServer,
		CageLogDir:       cageLogDir,
		NATSConn:         natsConnForGRPC(findingsBus),
		ServiceLogDir:    embedded.LogDir(),
		ConfigYAML:       configYAML,
		ServiceEndpoints: endpoints,
		Cancel:           cancel,
		Version:          version,
	}, log)
	if err != nil {
		return err
	}

	cageWorker, assessmentWorker := buildTemporalWorkers(
		ctx, cancel, temporalClient,
		fleetSetup.pool.TotalCageSlots(),
		cageActivityImpl, assessmentActivityImpl, log,
	)

	// Workers must be polling before gRPC accepts traffic. The readiness
	// probe inside startTemporalWorkers closes the race.
	if err := startTemporalWorkers(ctx, temporalClient, temporalNamespace, cageWorker, assessmentWorker, log); err != nil {
		return err
	}

	enforcerLog := log.WithValues("component", "timeout-enforcer")
	pollInterval := cfg.InterventionPollInterval()
	timeoutEnforcer := intervention.NewTimeoutEnforcer(iQueue, temporalClient, notifier, pollInterval, cfg.InterventionWarningThreshold(), enforcerLog)
	enforcerLog.Info("timeout enforcer started", "interval", pollInterval)
	go func() {
		// If the enforcer dies, timed-out interventions stop firing.
		// Cancel everything so the operator notices.
		if err := timeoutEnforcer.Run(ctx); err != nil {
			enforcerLog.Error(err, "stopped, triggering orchestrator shutdown")
		} else {
			enforcerLog.Info("stopped")
		}
		if ctx.Err() == nil {
			cancel()
		}
	}()

	deps := shutdownDeps{
		grpcServer:       grpcServer,
		cageWorker:       cageWorker,
		assessmentWorker: assessmentWorker,
		identityCleanup:  identityCleanup,
		alertDispatcher:  alertDispatcher,
		embeddedMgr:      embeddedMgr,
	}

	if grpcAddr == "" {
		grpcAddr = cfg.GRPCListenAddr()
	}
	lis, _, err := startGRPCListener(grpcAddr, cfg, log)
	if err != nil {
		return err
	}
	serveGRPC(grpcServer, lis, cancel, log)

	if err := waitForGRPCReady(ctx, cfg, grpcAddr); err != nil {
		shutdownSequence(cancel, deps, nil, log)
		return fmt.Errorf("waiting for gRPC server: %w", err)
	}

	pidFile := filepath.Join(embedded.RunDir(), "agentcage.pid")
	if err := writePIDFile(pidFile); err != nil {
		// `agentcage stop` and systemd both read this file. If we can't
		// write it, we'd rather refuse to start than start unstoppable.
		shutdownSequence(cancel, deps, nil, log)
		return fmt.Errorf("pid file: %w", err)
	}
	defer func() {
		if rmErr := os.Remove(pidFile); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			log.Error(rmErr, "removing pid file on shutdown", "path", pidFile)
		}
	}()

	if progress != nil {
		progress.Done()
		fmt.Println()
	} else {
		elapsed := time.Since(startTime).Truncate(time.Second)
		ui.ReadyWithElapsed(elapsed)
	}
	ui.Info("gRPC", lis.Addr().String())
	ui.Info("Logs", "agentcage logs orchestrator")
	ui.Info("Data", embedded.DataDir())
	fmt.Println()
	ui.Step("Press Ctrl+C to stop.")

	sigCh := waitForShutdown(ctx, log)

	shutdownSequence(cancel, deps, sigCh, log)

	return nil
}

func validateRequiredSecrets(ctx context.Context, reader identity.SecretReader, cfg *config.Config) error {
	type required struct {
		path      string
		label     string
		condition bool
	}
	checks := []required{
		{identity.PathLLMKey, "orchestrator llm-api-key", cfg.LLM.Endpoint != ""},
		{identity.PathNATSURL, "orchestrator nats-url", cfg.Infrastructure.IsExternalNATS()},
		{identity.PathPostgresURL, "orchestrator postgres-url", cfg.Infrastructure.IsExternalPostgres()},
		{identity.PathJudgeKey, "orchestrator judge-api-key", cfg.JudgeEndpoint() != ""},
	}
	// Temporal API key is only needed for Temporal Cloud. Self-hosted
	// Temporal authenticates via mTLS (SPIRE) or runs without auth.
	// The key is read opportunistically in connectTemporal; not
	// required here.
	if cfg.Infrastructure.IsExternalNomad() {
		checks = append(checks, required{identity.PathNomadToken, "orchestrator nomad-token", true})
	}

	var needed []string
	for _, c := range checks {
		if c.condition {
			needed = append(needed, c.label)
		}
	}

	if reader == nil {
		if len(needed) > 0 {
			return fmt.Errorf("vault not available but required secrets are configured: %v\nimport secrets with: agentcage vault import --from-file secrets.env", needed)
		}
		return nil
	}

	var missing []string
	for _, c := range checks {
		if !c.condition {
			continue
		}
		val, err := identity.ReadSecretValue(ctx, reader, c.path)
		if err != nil || val == "" {
			missing = append(missing, c.label)
		}
	}

	if len(missing) == 0 {
		return nil
	}

	var b strings.Builder
	b.WriteString("required secrets missing from Vault:\n")
	for _, m := range missing {
		fmt.Fprintf(&b, "  %s\n", m)
	}
	b.WriteString("\nSeed on boot: agentcage init --secrets secrets.env\n")
	b.WriteString("\nExample secrets.env:\n")
	b.WriteString("  AGENTCAGE_LLM_API_KEY=sk-...\n")
	b.WriteString("  AGENTCAGE_JUDGE_API_KEY=sk-... # only if config.judge.endpoint is set\n")
	return errors.New(b.String())
}

func seedSecrets(ctx context.Context, reader identity.SecretReader, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening secrets file %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var seeded int
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		vaultPath, known := identity.EnvToVaultPath[key]
		if !known {
			continue
		}

		if err := reader.WriteSecret(ctx, vaultPath, map[string]any{"value": value}); err != nil {
			return fmt.Errorf("writing %s: %w", key, err)
		}
		seeded++
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading secrets file: %w", err)
	}
	if seeded == 0 {
		ui.Warn("No recognized secrets found in %s", path)
	} else {
		ui.Step("Seeding secrets (%d keys)", seeded)
	}
	return nil
}
