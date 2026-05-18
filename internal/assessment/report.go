package assessment

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/okedeji/agentcage/internal/findings"
	"github.com/okedeji/agentcage/internal/gateway"
)

type Report struct {
	AssessmentID       string          `json:"assessment_id"`
	CustomerID         string          `json:"customer_id"`
	GeneratedAt        time.Time       `json:"generated_at"`
	Status             string          `json:"status"`
	ExecutiveSummary   string          `json:"executive_summary,omitempty"`
	Methodology        string          `json:"methodology,omitempty"`
	Summary            ReportSummary   `json:"summary"`
	Findings           []ReportFinding `json:"findings"`
	RemediationRoadmap string          `json:"remediation_roadmap,omitempty"`
	AuditDigestID      string          `json:"audit_digest_id,omitempty"`
}

type ReportSummary struct {
	TotalFindings int `json:"total_findings"`
	Critical      int `json:"critical"`
	High          int `json:"high"`
	Medium        int `json:"medium"`
	Low           int `json:"low"`
	Info          int `json:"info"`
}

type ReportFinding struct {
	ID              string  `json:"id"`
	Title           string  `json:"title"`
	Status          string  `json:"status"`
	Severity        string  `json:"severity"`
	VulnClass       string  `json:"vuln_class"`
	CWE             string  `json:"cwe,omitempty"`
	CVSSScore       float64 `json:"cvss_score,omitempty"`
	Endpoint        string  `json:"endpoint"`
	Description     string  `json:"description"`
	Evidence        string  `json:"evidence,omitempty"`
	Remediation     string  `json:"remediation,omitempty"`
	ValidationProof string  `json:"validation_proof,omitempty"`
}

// GenerateReport accepts every finding the assessment produced — both
// validated vulnerabilities and unvalidated candidates from surface
// discovery. The report shows them all so a discovery-only run still
// gives the operator visibility into what was learned.
func GenerateReport(ctx context.Context, assessmentID, customerID string, allFindings []findings.Finding, target string, llm *gateway.Client) (*Report, error) {
	if assessmentID == "" {
		return nil, fmt.Errorf("generating report: assessment ID is required")
	}
	if customerID == "" {
		return nil, fmt.Errorf("generating report: customer ID is required")
	}

	summary := ReportSummary{TotalFindings: len(allFindings)}
	reportFindings := make([]ReportFinding, 0, len(allFindings))

	for _, f := range allFindings {
		switch f.Severity {
		case findings.SeverityCritical:
			summary.Critical++
		case findings.SeverityHigh:
			summary.High++
		case findings.SeverityMedium:
			summary.Medium++
		case findings.SeverityLow:
			summary.Low++
		case findings.SeverityInfo:
			summary.Info++
		}

		var evidence string
		if meta := f.Evidence.Metadata; meta != nil {
			if v, ok := meta["summary"]; ok {
				evidence = v
			}
		}

		var proofSummary string
		if f.ValidationProof != nil && f.ValidationProof.Confirmed {
			proofSummary = fmt.Sprintf("Confirmed by cage %s", f.ValidationProof.ValidatorCageID)
			if f.ValidationProof.ReproductionSteps != "" {
				proofSummary += ": " + f.ValidationProof.ReproductionSteps
			}
		}

		reportFindings = append(reportFindings, ReportFinding{
			ID:              f.ID,
			Title:           f.Title,
			Status:          f.Status.String(),
			Severity:        f.Severity.String(),
			VulnClass:       f.VulnClass,
			CWE:             f.CWE,
			CVSSScore:       f.CVSSScore,
			Endpoint:        f.Endpoint,
			Description:     f.Description,
			Evidence:        evidence,
			Remediation:     f.Remediation,
			ValidationProof: proofSummary,
		})
	}

	report := &Report{
		AssessmentID: assessmentID,
		CustomerID:   customerID,
		GeneratedAt:  time.Now(),
		Status:       StatusPendingReview.String(),
		Summary:      summary,
		Findings:     reportFindings,
		Methodology:  fmt.Sprintf("Automated penetration test against %s using autonomous LLM-driven exploit agents in sandboxed Firecracker microVMs.", target),
	}

	if llm != nil {
		report.ExecutiveSummary = generateExecutiveSummary(ctx, llm, assessmentID, summary, target)
		report.RemediationRoadmap = generateRemediationRoadmap(ctx, llm, assessmentID, reportFindings)
	}

	return report, nil
}

const execSummaryPrompt = `You are writing the executive summary for a penetration test report. The audience is a CISO who needs to understand the risk in 2-3 paragraphs.

Given the findings summary, write a concise executive summary covering:
- What was tested
- The overall risk posture
- The most critical issues found
- What to prioritize fixing first

Respond with plain text only, no JSON, no headers.`

func generateExecutiveSummary(ctx context.Context, llm *gateway.Client, assessmentID string, summary ReportSummary, target string) string {
	input := fmt.Sprintf("Target: %s\nFindings: %d total (%d critical, %d high, %d medium, %d low, %d info)",
		target, summary.TotalFindings, summary.Critical, summary.High, summary.Medium, summary.Low, summary.Info)

	resp, err := llm.ChatCompletion(ctx, "report", assessmentID, 0, gateway.LLMRequest{
		Messages: []gateway.LLMMessage{
			{Role: "system", Content: execSummaryPrompt},
			{Role: "user", Content: input},
		},
	})
	if err != nil || len(resp.Choices) == 0 {
		return ""
	}
	return resp.Choices[0].Message.Content
}

const roadmapPrompt = `You are writing a remediation roadmap for a penetration test report. Given a list of findings with their severity and remediation steps, produce a prioritized action plan.

Group by priority (immediate, short-term, long-term). Be concise and actionable.

Respond with plain text only, no JSON.`

func generateRemediationRoadmap(ctx context.Context, llm *gateway.Client, assessmentID string, reportFindings []ReportFinding) string {
	findingsJSON, err := json.Marshal(reportFindings)
	if err != nil {
		return ""
	}

	resp, err := llm.ChatCompletion(ctx, "report", assessmentID, 0, gateway.LLMRequest{
		Messages: []gateway.LLMMessage{
			{Role: "system", Content: roadmapPrompt},
			{Role: "user", Content: string(findingsJSON)},
		},
	})
	if err != nil || len(resp.Choices) == 0 {
		return ""
	}
	return resp.Choices[0].Message.Content
}

func FormatJSON(report *Report) ([]byte, error) {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("formatting report as JSON: %w", err)
	}
	return data, nil
}
