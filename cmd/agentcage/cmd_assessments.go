package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	pb "github.com/okedeji/agentcage/api/proto"
	"github.com/okedeji/agentcage/internal/config"
	"google.golang.org/grpc"
)

func cmdAssessments(args []string) {
	if len(args) > 0 && args[0] == "cancel" {
		cmdAssessmentsCancel(args[1:])
		return
	}
	if len(args) > 0 && args[0] == "finish" {
		cmdAssessmentsFinish(args[1:])
		return
	}
	if len(args) > 0 && args[0] == "plan" {
		cmdAssessmentsPlan(args[1:])
		return
	}

	fs := flag.NewFlagSet("assessments", flag.ExitOnError)
	fs.Usage = printAssessmentsUsage
	id := fs.String("id", "", "assessment ID to show details for")
	follow := fs.Bool("follow", false, "follow live progress")
	followShort := fs.Bool("f", false, "follow live progress (short)")
	format := fs.String("format", "", "output format: json")
	statusFilter := fs.String("status", "", "filter by status: discovery, awaiting_plan_approval, exploitation, validation, pending_review, approved, rejected, unreviewed, plan_unapproved, failed")
	limit := fs.Int("limit", 50, "max results to return")
	_ = fs.Parse(args)

	if *followShort {
		*follow = true
	}

	cfg := config.Defaults()
	if resolved := config.Resolve(""); resolved != "" {
		override, err := config.Load(resolved)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: loading config: %v\n", err)
			os.Exit(1)
		}
		cfg = config.Merge(cfg, override)
	}

	ctx, cancel := context.WithTimeout(context.Background(), followHardTimeout)
	defer cancel()

	conn, err := dialOrchestrator(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = conn.Close() }()

	client := pb.NewAssessmentServiceClient(conn)

	if *id != "" {
		if *statusFilter != "" {
			fmt.Fprintln(os.Stderr, "error: --status cannot be used with --id")
			os.Exit(1)
		}
		if *follow {
			printCatchUpSummary(ctx, client, *id)
			followAssessment(ctx, conn, *id, *format)
		} else {
			showAssessment(ctx, conn, *id)
		}
		return
	}

	listAssessments(ctx, client, *statusFilter, int32(*limit))
}

func showAssessment(ctx context.Context, conn *grpc.ClientConn, id string) {
	aClient := pb.NewAssessmentServiceClient(conn)
	cClient := pb.NewCageServiceClient(conn)
	fClient := pb.NewFindingsServiceClient(conn)
	iClient := pb.NewInterventionServiceClient(conn)

	resp, err := aClient.GetAssessment(ctx, &pb.GetAssessmentRequest{AssessmentId: id})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	info := resp.GetAssessment()
	stats := info.GetStats()

	// Header
	fmt.Printf("Assessment %s\n", info.GetAssessmentId())
	fmt.Printf("  Status:    %s\n", friendlyStatus(info.GetStatus().String()))
	if scope := info.GetConfig().GetScope(); scope != nil && scope.GetHost() != "" {
		fmt.Printf("  Target:    %s\n", scope.GetHost())
	}
	fmt.Printf("  Customer:  %s\n", info.GetCustomerId())
	if info.GetCreatedAt() != nil {
		fmt.Printf("  Created:   %s\n", info.GetCreatedAt().AsTime().Format(time.RFC3339))
		elapsed := time.Since(info.GetCreatedAt().AsTime()).Round(time.Second)
		fmt.Printf("  Duration:  %s elapsed\n", elapsed)
	}

	// Budget
	if stats != nil {
		fmt.Println()
		fmt.Println("Budget:")
		tokenBudget := info.GetConfig().GetTotalTokenBudget()
		if tokenBudget > 0 {
			pct := float64(stats.GetTokensConsumed()) / float64(tokenBudget) * 100
			fmt.Printf("  Tokens:    %d / %d (%.0f%%)\n", stats.GetTokensConsumed(), tokenBudget, pct)
		} else {
			fmt.Printf("  Tokens:    %d consumed\n", stats.GetTokensConsumed())
		}
	}

	// Cages
	fmt.Println()
	fmt.Println("Cages:")
	if stats != nil {
		fmt.Printf("  Total: %d  |  Active: %d\n", stats.GetTotalCages(), stats.GetActiveCages())
	}

	cageResp, cageErr := cClient.ListCagesByAssessment(ctx, &pb.ListCagesByAssessmentRequest{AssessmentId: id})
	if cageErr == nil && len(cageResp.GetCageIds()) > 0 {
		for _, cageID := range cageResp.GetCageIds() {
			getCtx, getCancel := context.WithTimeout(ctx, 3*time.Second)
			cr, err := cClient.GetCage(getCtx, &pb.GetCageRequest{CageId: cageID})
			getCancel()
			if err != nil {
				fmt.Printf("  %s  (unavailable)\n", cageID)
				continue
			}
			cage := cr.GetCage()
			cType := friendlyCageType(cage.GetType().String())
			cState := strings.TrimPrefix(cage.GetState().String(), "CAGE_STATE_")
			cState = strings.ToLower(cState)
			fmt.Printf("  %s  %-10s  %s\n", cageID, cType, cState)
		}
	} else if stats == nil || stats.GetTotalCages() == 0 {
		fmt.Println("  (none)")
	}

	// Findings
	fmt.Println()
	fmt.Println("Findings:")
	if stats != nil {
		fmt.Printf("  %d candidate  |  %d validated  |  %d rejected\n",
			stats.GetFindingsCandidate(), stats.GetFindingsValidated(), stats.GetFindingsRejected())
	}
	findingsResp, fErr := fClient.ListFindings(ctx, &pb.ListFindingsRequest{AssessmentId: id, Limit: 20})
	if fErr == nil {
		for _, f := range findingsResp.GetFindings() {
			sev := friendlySeverity(f.GetSeverity().String())
			status := strings.TrimPrefix(f.GetStatus().String(), "FINDING_STATUS_")
			status = strings.ToLower(status)
			fmt.Printf("  %-8s %-40s (%s)\n", sev, f.GetTitle(), status)
		}
	}
	if stats != nil && stats.GetFindingsCandidate()+stats.GetFindingsValidated()+stats.GetFindingsRejected() == 0 {
		fmt.Println("  (none)")
	}

	// Interventions — fetched once and inspected twice: once to render
	// a Plan summary if a plan_approval is pending, then again below
	// for the full pending list.
	iResp, iErr := iClient.ListInterventions(ctx, &pb.ListInterventionsRequest{AssessmentIdFilter: id})

	printPlanSummary(iResp, iErr, id)

	fmt.Println()
	fmt.Println("Interventions:")
	pending := 0
	if iErr == nil {
		for _, iv := range iResp.GetInterventions() {
			if strings.Contains(iv.GetStatus().String(), "PENDING") {
				pending++
				fmt.Printf("  [%s] %s\n", iv.GetPriority().String(), iv.GetDescription())
				printInterventionCommand(iv)
			}
		}
	}
	if pending == 0 {
		fmt.Println("  (none pending)")
	}

	// Footer
	if !isTerminal(info.GetStatus()) {
		fmt.Printf("\nFollow live: agentcage assessments --id %s --follow\n", id)
	} else {
		fmt.Printf("\nReport: agentcage report --assessment %s\n", id)
	}
}

func printCatchUpSummary(ctx context.Context, client pb.AssessmentServiceClient, id string) {
	resp, err := client.GetAssessment(ctx, &pb.GetAssessmentRequest{AssessmentId: id})
	if err != nil {
		return
	}
	info := resp.GetAssessment()
	stats := info.GetStats()

	status := friendlyStatus(info.GetStatus().String())
	fmt.Printf("Assessment %s — %s\n", id, status)
	if stats != nil {
		fmt.Printf("  Cages: %d total, %d active  |  Findings: %d candidate, %d validated\n",
			stats.GetTotalCages(), stats.GetActiveCages(),
			stats.GetFindingsCandidate(), stats.GetFindingsValidated())
		if stats.GetTokensConsumed() > 0 {
			fmt.Printf("  Budget: %dk tokens\n", stats.GetTokensConsumed()/1000)
		}
	}
	if info.GetCreatedAt() != nil {
		elapsed := time.Since(info.GetCreatedAt().AsTime()).Round(time.Second)
		fmt.Printf("  Duration: %s\n", elapsed)
	}
	fmt.Println("\nReconnected. Following live...")
}

func listAssessments(ctx context.Context, client pb.AssessmentServiceClient, statusFilter string, limit int32) {
	req := &pb.ListAssessmentsRequest{Limit: limit}

	if statusFilter != "" {
		s, ok := parseAssessmentStatusFilter(statusFilter)
		if !ok {
			fmt.Fprintf(os.Stderr, "error: unknown status %q (valid: discovery, awaiting_plan_approval, exploitation, validation, pending_review, approved, rejected, unreviewed, plan_unapproved, failed)\n", statusFilter)
			os.Exit(1)
		}
		req.StatusFilter = s
	}

	resp, err := client.ListAssessments(ctx, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	items := resp.GetAssessments()
	if len(items) == 0 {
		if statusFilter != "" {
			fmt.Printf("No %s assessments.\n", statusFilter)
		} else {
			fmt.Println("No assessments.")
		}
		return
	}

	for _, info := range items {
		target := ""
		if scope := info.GetConfig().GetScope(); scope != nil {
			target = scope.GetHost()
		}
		created := ""
		if info.GetCreatedAt() != nil {
			created = info.GetCreatedAt().AsTime().Format(time.RFC3339)
		}
		fmt.Printf("  %s  %-16s  %-15s  %s\n",
			info.GetAssessmentId(),
			friendlyStatus(info.GetStatus().String()),
			target,
			created,
		)
	}
}

func parseAssessmentStatusFilter(s string) (pb.AssessmentStatus, bool) {
	switch s {
	case "discovery":
		return pb.AssessmentStatus_ASSESSMENT_STATUS_DISCOVERY, true
	case "awaiting_plan_approval":
		return pb.AssessmentStatus_ASSESSMENT_STATUS_AWAITING_PLAN_APPROVAL, true
	case "exploitation":
		return pb.AssessmentStatus_ASSESSMENT_STATUS_EXPLOITATION, true
	case "validation":
		return pb.AssessmentStatus_ASSESSMENT_STATUS_VALIDATION, true
	case "pending_review":
		return pb.AssessmentStatus_ASSESSMENT_STATUS_PENDING_REVIEW, true
	case "approved":
		return pb.AssessmentStatus_ASSESSMENT_STATUS_APPROVED, true
	case "rejected":
		return pb.AssessmentStatus_ASSESSMENT_STATUS_REJECTED, true
	case "unreviewed":
		return pb.AssessmentStatus_ASSESSMENT_STATUS_UNREVIEWED, true
	case "plan_unapproved":
		return pb.AssessmentStatus_ASSESSMENT_STATUS_PLAN_UNAPPROVED, true
	case "failed":
		return pb.AssessmentStatus_ASSESSMENT_STATUS_FAILED, true
	default:
		return pb.AssessmentStatus_ASSESSMENT_STATUS_UNSPECIFIED, false
	}
}

func cmdAssessmentsCancel(args []string) {
	fs := flag.NewFlagSet("assessments cancel", flag.ExitOnError)
	all := fs.Bool("all", false, "cancel all running assessments")
	_ = fs.Parse(args)

	if !*all && fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentcage assessments cancel <id>")
		fmt.Fprintln(os.Stderr, "       agentcage assessments cancel --all")
		os.Exit(1)
	}

	cfg := config.Defaults()
	if resolved := config.Resolve(""); resolved != "" {
		if override, err := config.Load(resolved); err == nil {
			cfg = config.Merge(cfg, override)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := dialOrchestrator(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = conn.Close() }()

	client := pb.NewAssessmentServiceClient(conn)

	var ids []string
	if *all {
		resp, err := client.ListAssessments(ctx, &pb.ListAssessmentsRequest{Limit: 500})
		if err != nil {
			fmt.Fprintf(os.Stderr, "error listing assessments: %v\n", err)
			os.Exit(1)
		}
		for _, info := range resp.GetAssessments() {
			s := info.GetStatus()
			if s == pb.AssessmentStatus_ASSESSMENT_STATUS_APPROVED ||
				s == pb.AssessmentStatus_ASSESSMENT_STATUS_REJECTED ||
				s == pb.AssessmentStatus_ASSESSMENT_STATUS_UNREVIEWED ||
				s == pb.AssessmentStatus_ASSESSMENT_STATUS_FAILED {
				continue
			}
			ids = append(ids, info.GetAssessmentId())
		}
		if len(ids) == 0 {
			fmt.Println("No running assessments to cancel.")
			return
		}
		fmt.Printf("Cancel %d running assessment(s)? [y/N] ", len(ids))
	} else {
		ids = append(ids, fs.Arg(0))
		fmt.Printf("Cancel assessment %s? [y/N] ", ids[0])
	}

	var answer string
	_, _ = fmt.Scanln(&answer)
	if answer != "y" && answer != "Y" {
		fmt.Println("Aborted.")
		return
	}

	for _, id := range ids {
		_, err := client.CancelAssessment(ctx, &pb.CancelAssessmentRequest{AssessmentId: id})
		if err != nil {
			if strings.Contains(err.Error(), "workflow not found") {
				fmt.Printf("  ✓  %s (already completed)\n", id)
				continue
			}
			fmt.Fprintf(os.Stderr, "  ✗  %s: %v\n", id, err)
			continue
		}
		fmt.Printf("  ✓  cancelled %s\n", id)
	}
}

func cmdAssessmentsFinish(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentcage assessments finish <id>")
		os.Exit(1)
	}
	assessmentID := args[0]

	cfg := config.Defaults()
	if resolved := config.Resolve(""); resolved != "" {
		if override, err := config.Load(resolved); err == nil {
			cfg = config.Merge(cfg, override)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := dialOrchestrator(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = conn.Close() }()

	client := pb.NewAssessmentServiceClient(conn)
	if _, err := client.FinishAssessment(ctx, &pb.FinishAssessmentRequest{AssessmentId: assessmentID}); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Assessment %s finishing — stopping exploitation, proceeding to validation and report.\n", assessmentID)
}

func printAssessmentsUsage() {
	fmt.Fprintf(os.Stderr, `usage: agentcage assessments [flags]
       agentcage assessments cancel <id>
       agentcage assessments cancel --all
       agentcage assessments finish <id>

List, inspect, cancel, or finish assessments.

Commands:
  cancel <id>     Kill the assessment immediately (no report generated)
  cancel --all    Kill all running assessments
  finish <id>     Stop testing, validate findings, generate report
  plan <id>       Show the pending exploitation plan awaiting approval

Examples:
  agentcage assessments
  agentcage assessments --status discovery
  agentcage assessments --id <assessment-id>
  agentcage assessments --id <assessment-id> --follow
  agentcage assessments cancel <assessment-id>
  agentcage assessments finish <assessment-id>
  agentcage assessments plan <assessment-id>

Flags:
  --id          assessment ID to show details for
  --follow, -f  follow live progress (requires --id)
  --status      filter by status
  --limit       max results to return (default 50)
`)
}

// cmdAssessmentsPlan renders the orchestrator-generated exploitation
// plan that's currently pending operator approval. Pulls the
// plan_approval intervention's context_data, decodes the PlanProposal,
// and pretty-prints it.
func cmdAssessmentsPlan(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: agentcage assessments plan <id>")
		os.Exit(1)
	}
	assessmentID := args[0]

	cfg := config.Defaults()
	if resolved := config.Resolve(""); resolved != "" {
		if override, err := config.Load(resolved); err == nil {
			cfg = config.Merge(cfg, override)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := dialOrchestrator(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = conn.Close() }()

	iClient := pb.NewInterventionServiceClient(conn)
	resp, err := iClient.ListInterventions(ctx, &pb.ListInterventionsRequest{
		AssessmentIdFilter: assessmentID,
		TypeFilter:         pb.InterventionType_INTERVENTION_TYPE_PLAN_APPROVAL,
		StatusFilter:       pb.InterventionStatus_INTERVENTION_STATUS_PENDING,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	items := resp.GetInterventions()
	if len(items) == 0 {
		fmt.Fprintf(os.Stderr, "no pending plan-approval intervention for assessment %s (status may be %s, %s, or already past the gate)\n",
			assessmentID, "discovery", "awaiting_plan_approval")
		os.Exit(1)
	}
	// In practice there is only ever one pending plan_approval per
	// assessment because the workflow regenerates on modify rather than
	// stacking — but render whichever is most recent if more appear.
	item := items[0]

	proposal, err := decodePlanProposal(item.GetContextData())
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: plan proposal payload corrupted: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Assessment: %s\n", assessmentID)
	fmt.Printf("Intervention: %s\n", item.GetInterventionId())
	fmt.Println()
	fmt.Println("Goal:")
	for _, line := range strings.Split(strings.TrimSpace(proposal.Goal), "\n") {
		fmt.Printf("  %s\n", line)
	}
	if s := strings.TrimSpace(proposal.Summary); s != "" {
		fmt.Println()
		fmt.Println("Discovery summary:")
		for _, line := range strings.Split(s, "\n") {
			fmt.Printf("  %s\n", line)
		}
	}
	fmt.Println()
	fmt.Printf("Proposed actions (%d cages, ~%d tokens):\n", proposal.EstimatedCages, proposal.EstimatedTokens)
	if len(proposal.Actions) == 0 {
		fmt.Println("  (none — nothing actionable from discovery)")
	}
	for _, a := range proposal.Actions {
		endpoint := a.Scope.Host
		if len(a.Scope.Paths) > 0 {
			endpoint = a.Scope.Host + a.Scope.Paths[0]
		}
		fmt.Printf("  %-12s  %-30s  %s\n", a.VulnClass, endpoint, a.Objective)
	}
	if n := strings.TrimSpace(proposal.Notes); n != "" {
		fmt.Println()
		fmt.Println("Notes:")
		for _, line := range strings.Split(n, "\n") {
			fmt.Printf("  %s\n", line)
		}
	}
	fmt.Println()
	fmt.Println("Resolve with:")
	fmt.Printf("  agentcage interventions resolve --id %s --action approve\n", item.GetInterventionId())
	fmt.Printf("  agentcage interventions resolve --id %s --action modify --feedback \"...\"\n", item.GetInterventionId())
	fmt.Printf("  agentcage interventions resolve --id %s --action reject --rationale \"...\"\n", item.GetInterventionId())
}

// planProposal mirrors the assessment.PlanProposal shape written into
// the plan_approval intervention's context_data. Kept local so cmd/
// does not import internal/assessment.
type planProposal struct {
	Goal    string `json:"goal"`
	Summary string `json:"summary"`
	Actions []struct {
		Type  string `json:"type"`
		Scope struct {
			Host  string   `json:"host"`
			Ports []string `json:"ports,omitempty"`
			Paths []string `json:"paths,omitempty"`
		} `json:"scope"`
		VulnClass string `json:"vuln_class"`
		Objective string `json:"objective"`
	} `json:"actions"`
	EstimatedCages  int32  `json:"estimated_cages"`
	EstimatedTokens int64  `json:"estimated_tokens"`
	Notes           string `json:"notes"`
}

func decodePlanProposal(data []byte) (planProposal, error) {
	var p planProposal
	if err := json.Unmarshal(data, &p); err != nil {
		return planProposal{}, err
	}
	return p, nil
}

// printPlanSummary renders a compact one-section view of the pending
// plan_approval (if any) in the assessment detail page. Stays terse
// on purpose — the full proposal lives behind `assessments plan <id>`.
func printPlanSummary(iResp *pb.ListInterventionsResponse, iErr error, assessmentID string) {
	if iErr != nil || iResp == nil {
		return
	}
	for _, iv := range iResp.GetInterventions() {
		if iv.GetType() != pb.InterventionType_INTERVENTION_TYPE_PLAN_APPROVAL {
			continue
		}
		if !strings.Contains(iv.GetStatus().String(), "PENDING") {
			continue
		}
		proposal, err := decodePlanProposal(iv.GetContextData())
		if err != nil {
			return
		}
		fmt.Println()
		fmt.Println("Plan (awaiting approval):")
		if goal := strings.TrimSpace(proposal.Goal); goal != "" {
			fmt.Printf("  Goal:    %s\n", firstLineOrFull(goal, 200))
		}
		fmt.Printf("  Actions: %d cages, ~%d tokens\n", proposal.EstimatedCages, proposal.EstimatedTokens)
		fmt.Printf("  View:    agentcage assessments plan %s\n", assessmentID)
		return
	}
}

// firstLineOrFull collapses multi-line goal text into one line for
// the assessment summary; falls back to the whole string when short.
func firstLineOrFull(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > max {
		return s[:max-1] + "…"
	}
	return s
}
