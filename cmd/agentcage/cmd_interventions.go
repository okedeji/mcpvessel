package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	pb "github.com/okedeji/agentcage/api/proto"
	"github.com/okedeji/agentcage/internal/config"
)

func cmdInterventions(args []string) {
	if len(args) > 0 && args[0] == "resolve" {
		cmdInterventionsResolve(args[1:])
		return
	}

	fs := flag.NewFlagSet("interventions", flag.ExitOnError)
	fs.Usage = printInterventionsUsage
	statusFilter := fs.String("status", "pending", "filter by status: pending, resolved, timed_out")
	assessmentID := fs.String("assessment", "", "filter by assessment ID")
	id := fs.String("id", "", "intervention ID to show details for")
	_ = fs.Parse(args)

	cfg := config.Defaults()
	if resolved := config.Resolve(""); resolved != "" {
		override, err := config.Load(resolved)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: loading config: %v\n", err)
			os.Exit(1)
		}
		cfg = config.Merge(cfg, override)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := dialOrchestrator(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = conn.Close() }()

	client := pb.NewInterventionServiceClient(conn)

	if *id != "" {
		showIntervention(ctx, client, *id)
		return
	}

	listInterventions(ctx, client, *statusFilter, *assessmentID)
}

func showIntervention(ctx context.Context, client pb.InterventionServiceClient, id string) {
	resp, err := client.GetIntervention(ctx, &pb.GetInterventionRequest{InterventionId: id})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	item := resp.GetIntervention()
	fmt.Printf("Intervention %s\n", item.GetInterventionId())
	fmt.Printf("  Type:        %s\n", interventionTypeLabel(item.GetType()))
	fmt.Printf("  Status:      %s\n", item.GetStatus())
	fmt.Printf("  Priority:    %s\n", item.GetPriority())
	if item.GetCageId() != "" {
		fmt.Printf("  Cage:        %s\n", item.GetCageId())
	}
	if item.GetAssessmentId() != "" {
		fmt.Printf("  Assessment:  %s\n", item.GetAssessmentId())
	}
	fmt.Printf("  Description: %s\n", item.GetDescription())
	if item.GetCreatedAt() != nil {
		fmt.Printf("  Created:     %s\n", item.GetCreatedAt().AsTime().Format(time.RFC3339))
	}
	if item.GetResolvedAt() != nil {
		fmt.Printf("  Resolved:    %s\n", item.GetResolvedAt().AsTime().Format(time.RFC3339))
	}
}

func listInterventions(ctx context.Context, client pb.InterventionServiceClient, statusFilter, assessmentID string) {
	var pbStatus pb.InterventionStatus
	switch statusFilter {
	case "pending":
		pbStatus = pb.InterventionStatus_INTERVENTION_STATUS_PENDING
	case "resolved":
		pbStatus = pb.InterventionStatus_INTERVENTION_STATUS_RESOLVED
	case "timed_out":
		pbStatus = pb.InterventionStatus_INTERVENTION_STATUS_TIMED_OUT
	default:
		fmt.Fprintf(os.Stderr, "error: unknown status %q (valid: pending, resolved, timed_out)\n", statusFilter)
		os.Exit(1)
	}

	req := &pb.ListInterventionsRequest{StatusFilter: pbStatus}
	if assessmentID != "" {
		req.AssessmentIdFilter = assessmentID
	}

	resp, err := client.ListInterventions(ctx, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	items := resp.GetInterventions()
	if len(items) == 0 {
		fmt.Printf("No %s interventions.\n", statusFilter)
		return
	}

	for _, item := range items {
		scope := "cage=" + item.GetCageId()
		if item.GetType() == pb.InterventionType_INTERVENTION_TYPE_REPORT_REVIEW {
			scope = "assessment=" + item.GetAssessmentId()
		}
		created := ""
		if item.GetCreatedAt() != nil {
			created = item.GetCreatedAt().AsTime().Format(time.RFC3339)
		}
		fmt.Printf("  %s  %s  type=%-14s  %s  %s\n",
			item.GetInterventionId(),
			scope,
			interventionTypeLabel(item.GetType()),
			item.GetDescription(),
			created,
		)
	}
}

func interventionTypeLabel(t pb.InterventionType) string {
	switch t {
	case pb.InterventionType_INTERVENTION_TYPE_TRIPWIRE_ESCALATION:
		return "tripwire"
	case pb.InterventionType_INTERVENTION_TYPE_PAYLOAD_REVIEW:
		return "payload_review"
	case pb.InterventionType_INTERVENTION_TYPE_REPORT_REVIEW:
		return "report_review"
	case pb.InterventionType_INTERVENTION_TYPE_POLICY_VIOLATION:
		return "policy_violation"
	case pb.InterventionType_INTERVENTION_TYPE_AGENT_HOLD:
		return "agent_hold"
	case pb.InterventionType_INTERVENTION_TYPE_PLAN_APPROVAL:
		return "plan_approval"
	default:
		return "unknown"
	}
}

func cmdInterventionsResolve(args []string) {
	fs := flag.NewFlagSet("interventions resolve", flag.ExitOnError)
	id := fs.String("id", "", "intervention ID (required)")
	action := fs.String("action", "", "action: resume, kill, allow, block, retry, skip, approve, reject, retest, modify")
	rationale := fs.String("rationale", "", "reason for the decision")
	feedback := fs.String("feedback", "", "operator revisions for plan_approval modify action")
	_ = fs.Parse(args)

	if *id == "" || *action == "" {
		fmt.Fprintln(os.Stderr, "usage: agentcage interventions resolve --id <id> --action <action> [--rationale reason] [--feedback revisions]")
		fmt.Fprintln(os.Stderr, "\nActions:")
		fmt.Fprintln(os.Stderr, "  resume, kill, allow, block     cage and budget interventions")
		fmt.Fprintln(os.Stderr, "  approve, reject, retest        report review interventions")
		fmt.Fprintln(os.Stderr, "  approve, reject, modify        plan approval interventions (modify needs --feedback)")
		os.Exit(1)
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := dialOrchestrator(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = conn.Close() }()

	client := pb.NewInterventionServiceClient(conn)

	// Same action verbs map to different RPCs depending on intervention
	// type ("approve" means review-approve for report_review and
	// plan-approve for plan_approval). Fetch the intervention first so
	// we can dispatch correctly.
	info, err := client.GetIntervention(ctx, &pb.GetInterventionRequest{InterventionId: *id})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	switch info.GetIntervention().GetType() {
	case pb.InterventionType_INTERVENTION_TYPE_PLAN_APPROVAL:
		resolvePlan(ctx, client, *id, *action, *rationale, *feedback)
	case pb.InterventionType_INTERVENTION_TYPE_REPORT_REVIEW:
		resolveReview(ctx, client, *id, *action, *rationale)
	default:
		switch *action {
		case "resume", "kill", "allow", "block":
			resolveCage(ctx, client, *id, *action, *rationale)
		default:
			fmt.Fprintf(os.Stderr, "error: unknown action %q for intervention type %s\n", *action, interventionTypeLabel(info.GetIntervention().GetType()))
			os.Exit(1)
		}
	}
}

func resolvePlan(ctx context.Context, client pb.InterventionServiceClient, id, action, rationale, feedback string) {
	var decision pb.PlanDecision
	switch action {
	case "approve":
		decision = pb.PlanDecision_PLAN_DECISION_APPROVE
	case "reject":
		decision = pb.PlanDecision_PLAN_DECISION_REJECT
	case "modify":
		decision = pb.PlanDecision_PLAN_DECISION_MODIFY
		if feedback == "" {
			fmt.Fprintln(os.Stderr, "error: --feedback is required for plan modify")
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "error: invalid plan-approval action %q (expected approve, reject, or modify)\n", action)
		os.Exit(1)
	}

	if _, err := client.ResolvePlanApproval(ctx, &pb.ResolvePlanApprovalRequest{
		InterventionId: id,
		Decision:       decision,
		Rationale:      rationale,
		Feedback:       feedback,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Plan-approval intervention %s resolved with decision=%s\n", id, action)
}

func resolveCage(ctx context.Context, client pb.InterventionServiceClient, id, action, rationale string) {
	var pbAction pb.InterventionAction
	switch action {
	case "resume":
		pbAction = pb.InterventionAction_INTERVENTION_ACTION_RESUME
	case "kill":
		pbAction = pb.InterventionAction_INTERVENTION_ACTION_KILL
	case "allow":
		pbAction = pb.InterventionAction_INTERVENTION_ACTION_ALLOW
	case "block":
		pbAction = pb.InterventionAction_INTERVENTION_ACTION_BLOCK
	}

	if _, err := client.ResolveCageIntervention(ctx, &pb.ResolveCageInterventionRequest{
		InterventionId: id,
		Action:         pbAction,
		Rationale:      rationale,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Intervention %s resolved with action=%s\n", id, action)
}

func resolveReview(ctx context.Context, client pb.InterventionServiceClient, id, action, rationale string) {
	var decision pb.ReviewDecision
	switch action {
	case "approve":
		decision = pb.ReviewDecision_REVIEW_DECISION_APPROVE
	case "reject":
		decision = pb.ReviewDecision_REVIEW_DECISION_REJECT
	case "retest":
		decision = pb.ReviewDecision_REVIEW_DECISION_REQUEST_RETEST
	}

	if _, err := client.ResolveAssessmentReview(ctx, &pb.ResolveAssessmentReviewRequest{
		InterventionId: id,
		Decision:       decision,
		Rationale:      rationale,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Review intervention %s resolved with decision=%s\n", id, action)
}

func printInterventionsUsage() {
	fmt.Fprintf(os.Stderr, `usage: agentcage interventions [flags]
       agentcage interventions --id <intervention-id>
       agentcage interventions resolve --id <id> --action <action> [--rationale reason]

List, inspect, or resolve interventions.

Examples:
  agentcage interventions
  agentcage interventions --status resolved
  agentcage interventions --assessment <assessment-id>
  agentcage interventions --id <intervention-id>
  agentcage interventions resolve --id <id> --action resume
  agentcage interventions resolve --id <id> --action approve --rationale "findings confirmed"

Actions:
  resume, kill, allow, block     cage interventions (tripwire, payload)
  approve, reject, retest        report review interventions

Flags:
  --status       filter by status: pending, resolved, timed_out (default: pending)
  --assessment   filter by assessment ID
  --id           intervention ID to show details for
`)
}
