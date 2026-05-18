package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	pb "github.com/okedeji/agentcage/api/proto"
	"google.golang.org/grpc"
)

const (
	followMaxConsecutiveErrors = 20
	followBaseBackoff          = 3 * time.Second
	followMaxBackoff           = 30 * time.Second
	followPollInterval         = 3 * time.Second
	followStaleTimeout         = 30 * time.Minute
	followHardTimeout          = 5 * time.Hour
)

type followState struct {
	lastStatus        string
	lastChange        time.Time
	startTime         time.Time
	seenCages         map[string]string // cage_id -> last state
	seenFindings      map[string]bool
	seenInterventions map[string]string // intervention_id -> last status
	jsonMode          bool
}

func followAssessment(parentCtx context.Context, conn *grpc.ClientConn, assessmentID, format string) {
	ctx, cancel := context.WithTimeout(parentCtx, followHardTimeout)
	defer cancel()

	aClient := pb.NewAssessmentServiceClient(conn)
	cClient := pb.NewCageServiceClient(conn)
	fClient := pb.NewFindingsServiceClient(conn)
	iClient := pb.NewInterventionServiceClient(conn)

	s := &followState{
		startTime:         time.Now(),
		lastChange:        time.Now(),
		seenCages:         make(map[string]string),
		seenFindings:      make(map[string]bool),
		seenInterventions: make(map[string]string),
		jsonMode:          format == "json",
	}

	fmt.Printf("\nFollowing assessment %s... (Ctrl+C to detach)\n\n", assessmentID)

	var consecutiveErrors int

	for {
		pollCtx, pollCancel := context.WithTimeout(ctx, 5*time.Second)
		resp, err := aClient.GetAssessment(pollCtx, &pb.GetAssessmentRequest{AssessmentId: assessmentID})
		pollCancel()

		if ctx.Err() != nil {
			printDetachMessage(assessmentID)
			return
		}

		if err != nil {
			consecutiveErrors++
			if consecutiveErrors >= followMaxConsecutiveErrors {
				fmt.Fprintf(os.Stderr, "\nToo many poll errors. Detaching.\n")
				printDetachMessage(assessmentID)
				return
			}
			backoff := min(followBaseBackoff*time.Duration(1<<min(consecutiveErrors-1, 4)), followMaxBackoff)
			if !sleepCtx(ctx, backoff) {
				printDetachMessage(assessmentID)
				return
			}
			continue
		}
		consecutiveErrors = 0

		info := resp.GetAssessment()

		// Phase changes
		status := info.GetStatus().String()
		if status != s.lastStatus {
			emitTimestamp(s, "Phase: %s", friendlyStatus(status))
			s.lastStatus = status
			s.lastChange = time.Now()
		}

		// Cage events
		pollCages(ctx, cClient, assessmentID, s)

		// Finding events
		stats := info.GetStats()
		if stats != nil {
			totalFindings := stats.GetFindingsCandidate() + stats.GetFindingsValidated() + stats.GetFindingsRejected()
			if int(totalFindings) > len(s.seenFindings) {
				pollFindings(ctx, fClient, assessmentID, s)
			}
		}

		// Intervention events
		pollInterventions(ctx, iClient, assessmentID, s)

		// Terminal states
		if isTerminal(info.GetStatus()) {
			printTerminalSummary(info, s, assessmentID)
			return
		}

		if time.Since(s.lastChange) > followStaleTimeout {
			fmt.Fprintf(os.Stderr, "\nNo changes for %s. Detaching.\n", followStaleTimeout)
			printDetachMessage(assessmentID)
			return
		}

		if !sleepCtx(ctx, followPollInterval) {
			printDetachMessage(assessmentID)
			return
		}
	}
}

func pollCages(ctx context.Context, client pb.CageServiceClient, assessmentID string, s *followState) {
	pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	listResp, err := client.ListCagesByAssessment(pollCtx, &pb.ListCagesByAssessmentRequest{AssessmentId: assessmentID})
	if err != nil {
		return
	}

	for _, cageID := range listResp.GetCageIds() {
		getCtx, getCancel := context.WithTimeout(ctx, 3*time.Second)
		cageResp, err := client.GetCage(getCtx, &pb.GetCageRequest{CageId: cageID})
		getCancel()
		if err != nil {
			continue
		}

		cage := cageResp.GetCage()
		state := cage.GetState().String()
		prevState, seen := s.seenCages[cageID]
		cageType := friendlyCageType(cage.GetType().String())

		if !seen {
			emitTimestamp(s, "Cage %s (%s) started", cageID, cageType)
			fmt.Printf("           Logs: agentcage logs cage %s\n", cageID)
			s.seenCages[cageID] = state
			s.lastChange = time.Now()
		} else if state != prevState {
			switch {
			case strings.Contains(state, "COMPLETED"):
				emitTimestamp(s, "Cage %s completed", cageID)
			case strings.Contains(state, "FAILED"):
				errMsg := cage.GetError()
				if errMsg != "" {
					emitTimestamp(s, "Cage %s failed: %s", cageID, errMsg)
				} else {
					emitTimestamp(s, "Cage %s failed", cageID)
				}
			case strings.Contains(state, "PAUSED"):
				emitTimestamp(s, "Cage %s paused (intervention required)", cageID)
			}
			s.seenCages[cageID] = state
			s.lastChange = time.Now()
		}
	}
}

func pollFindings(ctx context.Context, client pb.FindingsServiceClient, assessmentID string, s *followState) {
	pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	resp, err := client.ListFindings(pollCtx, &pb.ListFindingsRequest{AssessmentId: assessmentID})
	if err != nil {
		return
	}

	for _, f := range resp.GetFindings() {
		fID := f.GetFindingId()
		if s.seenFindings[fID] {
			continue
		}
		s.seenFindings[fID] = true
		s.lastChange = time.Now()

		severity := friendlySeverity(f.GetSeverity().String())
		cageID := f.GetCageId()
		status := f.GetStatus().String()

		if strings.Contains(status, "VALIDATED") {
			cwe := f.GetCwe()
			cvss := f.GetCvssScore()
			if cwe != "" && cvss > 0 {
				emitTimestamp(s, "Finding validated: %s %s (%s, CVSS %.1f)", severity, f.GetTitle(), cwe, cvss)
			} else {
				emitTimestamp(s, "Finding validated: %s %s", severity, f.GetTitle())
			}
		} else {
			emitTimestamp(s, "Finding: %s %s (cage %s)", severity, f.GetTitle(), cageID)
		}
	}
}

func pollInterventions(ctx context.Context, client pb.InterventionServiceClient, assessmentID string, s *followState) {
	pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	resp, err := client.ListInterventions(pollCtx, &pb.ListInterventionsRequest{AssessmentIdFilter: assessmentID})
	if err != nil {
		return
	}

	for _, iv := range resp.GetInterventions() {
		id := iv.GetInterventionId()
		status := iv.GetStatus().String()
		prevStatus, seen := s.seenInterventions[id]

		if !seen {
			s.seenInterventions[id] = status
			s.lastChange = time.Now()
			emitTimestamp(s, "Intervention: %s", iv.GetDescription())
			printInterventionCommand(iv)
		} else if status != prevStatus && strings.Contains(status, "RESOLVED") {
			s.seenInterventions[id] = status
			s.lastChange = time.Now()
			emitTimestamp(s, "Intervention resolved: %s", iv.GetDescription())
		}
	}
}

func printInterventionCommand(iv *pb.InterventionInfo) {
	id := iv.GetInterventionId()
	switch {
	case strings.Contains(iv.GetType().String(), "TRIPWIRE"):
		fmt.Printf("           Run: agentcage interventions resolve --id %s --action resume  (or --action kill)\n", id)
	case strings.Contains(iv.GetType().String(), "REPORT_REVIEW"):
		fmt.Printf("           Run: agentcage interventions resolve --id %s --action approve\n", id)
	default:
		fmt.Printf("           Run: agentcage interventions\n")
	}
}

func isTerminal(status pb.AssessmentStatus) bool {
	switch status {
	case pb.AssessmentStatus_ASSESSMENT_STATUS_APPROVED,
		pb.AssessmentStatus_ASSESSMENT_STATUS_REJECTED,
		pb.AssessmentStatus_ASSESSMENT_STATUS_UNREVIEWED,
		pb.AssessmentStatus_ASSESSMENT_STATUS_FAILED,
		pb.AssessmentStatus_ASSESSMENT_STATUS_PENDING_REVIEW:
		return true
	}
	return false
}

func printTerminalSummary(info *pb.AssessmentInfo, s *followState, assessmentID string) {
	stats := info.GetStats()
	duration := time.Since(s.startTime).Round(time.Second)
	status := info.GetStatus()

	fmt.Println()
	switch status {
	case pb.AssessmentStatus_ASSESSMENT_STATUS_APPROVED:
		fmt.Println("Assessment approved.")
	case pb.AssessmentStatus_ASSESSMENT_STATUS_REJECTED:
		fmt.Println("Assessment rejected.")
	case pb.AssessmentStatus_ASSESSMENT_STATUS_UNREVIEWED:
		fmt.Println("Assessment unreviewed (review window elapsed without an operator decision).")
	case pb.AssessmentStatus_ASSESSMENT_STATUS_FAILED:
		fmt.Println("Assessment failed.")
	case pb.AssessmentStatus_ASSESSMENT_STATUS_PENDING_REVIEW:
		fmt.Println("Assessment awaiting review.")
	}

	if stats != nil {
		fmt.Printf("  Duration:  %s\n", duration)
		fmt.Printf("  Cages:     %d total\n", stats.GetTotalCages())
		fmt.Printf("  Findings:  %d validated, %d rejected, %d candidate\n",
			stats.GetFindingsValidated(), stats.GetFindingsRejected(), stats.GetFindingsCandidate())
		if stats.GetTokensConsumed() > 0 {
			fmt.Printf("  Budget:    %dk tokens\n", stats.GetTokensConsumed()/1000)
		}
	}

	fmt.Println()
	switch status {
	case pb.AssessmentStatus_ASSESSMENT_STATUS_PENDING_REVIEW:
		fmt.Printf("Run: agentcage interventions\n")
	default:
		fmt.Printf("Run: agentcage report --assessment %s\n", assessmentID)
	}
}

func printDetachMessage(assessmentID string) {
	fmt.Printf("\nDetached. Assessment continues on the server.\n")
	fmt.Printf("Reconnect: agentcage assessments --id %s --follow\n", assessmentID)
}

func emitTimestamp(s *followState, format string, args ...any) {
	ts := time.Now().Format("15:04:05")
	msg := fmt.Sprintf(format, args...)
	if s.jsonMode {
		fmt.Printf("{\"ts\":%q,\"msg\":%q}\n", ts, msg)
	} else {
		fmt.Printf("[%s] %s\n", ts, msg)
	}
}

func friendlyStatus(s string) string {
	s = strings.TrimPrefix(s, "ASSESSMENT_STATUS_")
	return strings.ToLower(strings.ReplaceAll(s, "_", " "))
}

func friendlyCageType(s string) string {
	s = strings.TrimPrefix(s, "CAGE_TYPE_")
	return strings.ToLower(s)
}

func friendlySeverity(s string) string {
	s = strings.TrimPrefix(s, "FINDING_SEVERITY_")
	return strings.ToUpper(s)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
