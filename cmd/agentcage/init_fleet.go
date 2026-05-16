package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	"github.com/go-logr/logr"

	"github.com/okedeji/agentcage/internal/alert"
	"github.com/okedeji/agentcage/internal/cage"
	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/embedded"
	"github.com/okedeji/agentcage/internal/fleet"
	"github.com/okedeji/agentcage/internal/identity"
)

type fleetSetup struct {
	pool         *fleet.PoolManager
	demand       *fleet.DemandLedger
	provisioner  fleet.HostProvisioner
	autoscaler   *fleet.Autoscaler
	scheduler    fleet.Scheduler
	validatorRes fleet.CageResources
}

// Autoscaler is constructed here but started in runInit so its
// cancel-on-death hookup shares context with the rest of shutdown.
func setupFleet(ctx context.Context, cfg *config.Config, embeddedMgr *embedded.Manager, secrets identity.SecretReader, alertDispatcher *alert.Dispatcher, log logr.Logger) (*fleetSetup, error) {
	pool := fleet.NewPoolManager()
	demand := fleet.NewDemandLedger()

	// config.Defaults() always populates these three keys.
	cageRes := func(name string) fleet.CageResources {
		c := cfg.Cages[name]
		return fleet.CageResources{VCPUs: c.MaxVCPUs, MemoryMB: c.MaxMemoryMB}
	}
	validatorRes := cageRes("validator")
	discoveryRes := cageRes("discovery")
	escalationRes := cageRes("exploitation")

	if err := fleet.InitPool(pool, cfg.Fleet.Hosts, validatorRes, discoveryRes, escalationRes); err != nil {
		return nil, fmt.Errorf("initializing fleet pool: %w", err)
	}
	status := pool.GetFleetStatus()
	totalSlots := int32(0)
	for _, p := range status.Pools {
		totalSlots += p.CageSlotsTotal
	}
	log.Info("fleet pool initialized", "hosts", status.TotalHosts, "total_slots", totalSlots)

	provisioner := buildHostProvisioner(ctx, cfg, secrets, log)

	// Autoscaler is only useful when the provisioner can create hosts.
	// In local mode (single machine), skip it entirely.
	var autoscaler *fleet.Autoscaler
	if _, isLocal := provisioner.(*fleet.LocalHostProvisioner); !isLocal {
		autoscalerCfg := fleet.AutoscalerConfig{
			PollInterval:         30 * time.Second,
			MinBuffer:            0,
			MaxBuffer:            1,
			DefaultCageResources: validatorRes,
		}
		if cfg.Fleet.Autoscaler != nil {
			autoscalerCfg.MinBuffer = cfg.Fleet.Autoscaler.MinWarmHosts
			autoscalerCfg.MaxBuffer = cfg.Fleet.Autoscaler.MaxHosts
			autoscalerCfg.ProvisioningTimeout = cfg.Fleet.Autoscaler.ProvisioningTimeout
			autoscalerCfg.EmergencyProvisionCount = cfg.Fleet.Autoscaler.EmergencyProvisionCount
		}
		autoscaler = fleet.NewAutoscaler(pool, demand, provisioner, alertDispatcher, autoscalerCfg, log.WithValues("component", "autoscaler"))
	}

	scheduler := buildScheduler(ctx, cfg, embeddedMgr, pool, secrets, log)

	return &fleetSetup{
		pool:         pool,
		demand:       demand,
		provisioner:  provisioner,
		autoscaler:   autoscaler,
		scheduler:    scheduler,
		validatorRes: validatorRes,
	}, nil
}

func buildScheduler(ctx context.Context, cfg *config.Config, embeddedMgr *embedded.Manager, pool *fleet.PoolManager, secrets identity.SecretReader, log logr.Logger) fleet.Scheduler {
	// External Nomad: operator runs their own cluster.
	if cfg.Infrastructure.IsExternalNomad() {
		nomadCfg := cfg.Infrastructure.Nomad

		if reason := cage.CheckNomadHealth(ctx, nomadCfg.Address); reason != "" {
			log.Error(nil, "external Nomad not reachable, falling back to simple scheduler", "reason", reason)
			return fleet.NewSimpleScheduler(pool)
		}

		var token string
		if secrets != nil {
			token, _ = identity.ReadSecretValue(ctx, secrets, identity.PathNomadToken)
		}
		var tlsCfg *tls.Config
		if nomadCfg.TLS != nil && nomadCfg.TLS.CertFile != "" {
			cert, err := tls.LoadX509KeyPair(nomadCfg.TLS.CertFile, nomadCfg.TLS.KeyFile)
			if err != nil {
				log.Error(err, "loading Nomad TLS cert, falling back to system CA")
			} else {
				tlsCfg = &tls.Config{
					MinVersion:   tls.VersionTLS12,
					Certificates: []tls.Certificate{cert},
				}
			}
		}
		log.Info("scheduler: nomad (external)", "addr", nomadCfg.Address)
		return fleet.NewNomadScheduler(pool, fleet.NomadSchedulerConfig{
			Address: nomadCfg.Address,
			Token:   token,
			TLS:     tlsCfg,
		})
	}

	// Embedded Nomad: started by the service manager. No auth needed.
	if n := embeddedMgr.EmbeddedNomad(); n != nil {
		log.Info("scheduler: nomad (embedded)", "addr", n.Address())
		return fleet.NewNomadScheduler(pool, fleet.NomadSchedulerConfig{
			Address: n.Address(),
		})
	}

	// No Nomad available: first-available bin-packing.
	log.Info("scheduler: simple (no Nomad configured)")
	return fleet.NewSimpleScheduler(pool)
}

func buildHostProvisioner(ctx context.Context, cfg *config.Config, secrets identity.SecretReader, log logr.Logger) fleet.HostProvisioner {
	pc := cfg.Fleet.Provisioner
	if pc != nil && pc.WebhookURL != "" {
		var apiKey string
		if secrets != nil {
			apiKey, _ = identity.ReadSecretValue(ctx, secrets, identity.PathFleetKey)
		}
		log.Info("fleet provisioner: webhook", "url", pc.WebhookURL)
		return fleet.NewWebhookProvisioner(pc.WebhookURL, apiKey, pc.Timeout, log)
	}
	log.Info("fleet provisioner: local (single machine, no scaling)")
	return fleet.NewLocalHostProvisioner(log)
}

