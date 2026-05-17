package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	pb "github.com/okedeji/agentcage/api/proto"
	"github.com/okedeji/agentcage/internal/config"
	"github.com/okedeji/agentcage/internal/plan"
	"google.golang.org/grpc"
)

const cmdRunTimeout = 10 * time.Minute

func cmdRun(args []string) {
	if len(args) == 0 {
		printRunUsage()
		os.Exit(1)
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	setupCtx, setupCancel := context.WithTimeout(sigCtx, cmdRunTimeout)
	defer setupCancel()

	rf, fs := parseRunFlags(args)
	explicit := explicitFlags(fs)

	jsonErrors := rf.format == "json"
	exitErr := func(msg string, err error) {
		if jsonErrors {
			emitJSONError(msg, err)
		} else {
			fmt.Fprintf(os.Stderr, "error: %s: %v\n", msg, err)
		}
		os.Exit(1)
	}

	cfg := config.Defaults()
	if resolved := config.Resolve(""); resolved != "" {
		override, loadErr := config.Load(resolved)
		if loadErr != nil {
			exitErr("loading operator config", loadErr)
		}
		cfg = config.Merge(cfg, override)
	}

	p := plan.BasePlanFromConfig(cfg)
	if rf.plan != "" {
		loaded, err := plan.Load(rf.plan)
		if err != nil {
			exitErr("loading plan", err)
		}
		p = plan.Merge(p, loaded)
	}

	override, err := plan.FlagsToOverride(explicit, plan.RawFlags{
		Agent:            rf.agent,
		Target:           rf.target,
		Ports:            []string(rf.ports),
		Paths:            []string(rf.paths),
		SkipPaths:        []string(rf.skipPaths),
		TokenBudget:      rf.tokenBudget,
		MaxDuration:      rf.maxDuration,
		MaxTotalCages:    rf.maxTotalCages,
		MaxIterations:    rf.maxIterations,
		Context:          rf.context,
		Focus:            []string(rf.focus),
		Skip:             []string(rf.skip),
		Endpoints:        []string(rf.endpoints),
		APISpecs:         []string(rf.apiSpecs),
		KnownWeaknesses:  []string(rf.knownWeaknesses),
		RequirePoC:       rf.requirePoC,
		HeadlessXSS:      rf.headlessXSS,
		Notify:           rf.notify,
		NotifyOnFinding:  rf.notifyOnFinding,
		NotifyOnComplete: rf.notifyOnComplete,
		Follow:           rf.follow,
		Format:           rf.format,
		Name:             rf.name,
		Tags:             []string(rf.tags),
		CustomerID:       rf.customerID,
	})
	if err != nil {
		exitErr("parsing flags", err)
	}
	p = plan.Merge(p, override)

	plan.ResolveDefaults(p, cfg)
	plan.ApplyDefaults(p)
	if err := plan.Validate(p); err != nil {
		exitErr("validating plan", err)
	}
	if err := plan.EnforceConfigCeilings(p, cfg); err != nil {
		exitErr("enforcing operator limits", err)
	}

	if setupCtx.Err() != nil {
		exitErr("setup", setupCtx.Err())
	}

	bundleRef, err := prepareBundle(setupCtx, p.Agent)
	if err != nil {
		exitErr("preparing bundle", err)
	}

	conn, err := dialOrchestrator(setupCtx, cfg)
	if err != nil {
		exitErr("connecting to orchestrator", err)
	}
	defer func() { _ = conn.Close() }()

	req, err := buildCreateAssessmentRequest(p, bundleRef)
	if err != nil {
		exitErr("building request", err)
	}

	createCtx, createCancel := context.WithTimeout(setupCtx, 30*time.Second)
	defer createCancel()

	client := pb.NewAssessmentServiceClient(conn)
	resp, err := client.CreateAssessment(createCtx, req, grpc.WaitForReady(true))
	if err != nil {
		exitErr("creating assessment", err)
	}

	info := resp.GetAssessment()
	printAssessmentSummary(info, p, bundleRef)

	if plan.BoolVal(p.Output.Follow) {
		followAssessment(sigCtx, conn, info.GetAssessmentId(), p.Output.Format)
	}
}

func emitJSONError(msg string, err error) {
	payload := struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
	}{Error: msg, Detail: err.Error()}
	enc, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		fmt.Fprintf(os.Stderr, "{\"error\":%q,\"detail\":\"marshal failed\"}\n", msg)
		return
	}
	fmt.Fprintln(os.Stderr, string(enc))
}

func printAssessmentSummary(info *pb.AssessmentInfo, p *plan.Plan, bundleRef string) {
	fmt.Println("\nAssessment started.")
	fmt.Printf("  ID:         %s\n", info.GetAssessmentId())
	if p.Name != "" {
		fmt.Printf("  Name:       %s\n", p.Name)
	}
	shortRef := bundleRef
	if len(shortRef) > 12 {
		shortRef = shortRef[:12]
	}
	fmt.Printf("  Agent:      %s (sha256:%s)\n", p.Agent, shortRef)
	fmt.Printf("  Target:     %s\n", strings.Join(p.Target.Hosts, ", "))
	if p.Budget.Tokens > 0 || p.Budget.MaxDuration != "" {
		parts := []string{}
		if p.Budget.Tokens > 0 {
			parts = append(parts, fmt.Sprintf("%d tokens", p.Budget.Tokens))
		}
		if p.Budget.MaxDuration != "" {
			parts = append(parts, p.Budget.MaxDuration)
		}
		fmt.Printf("  Budget:     %s\n", strings.Join(parts, " / "))
	}
	if p.Limits.MaxTotalCages > 0 {
		fmt.Printf("  Cages:      up to %d concurrent\n", p.Limits.MaxTotalCages)
	}
	if !plan.BoolVal(p.Output.Follow) {
		fmt.Printf("\nUse 'agentcage assessments --id %s' to monitor.\n", info.GetAssessmentId())
	}
}
