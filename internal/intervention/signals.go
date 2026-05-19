package intervention

const (
	SignalIntervention = "intervention"
	SignalReportReview = "report_review"
	SignalPlanApproval = "plan_approval"
)

// InterventionSignal is the Temporal signal payload sent to a cage workflow
// when an operator resolves a tripwire or payload review intervention.
type InterventionSignal struct {
	Action      Action            `json:"action"`
	Rationale   string            `json:"rationale"`
	Adjustments map[string]string `json:"adjustments,omitempty"`
}

// ReportReviewSignal is the Temporal signal payload sent to an assessment
// workflow when an operator resolves a report review.
type ReportReviewSignal struct {
	Decision    ReviewDecision      `json:"decision"`
	Rationale   string              `json:"rationale"`
	Adjustments []FindingAdjustment `json:"adjustments,omitempty"`
}

// PlanApprovalSignal is the Temporal signal payload sent to an
// assessment workflow when an operator resolves a plan-approval
// intervention. Feedback carries operator revisions on the "modify"
// decision; ignored for approve/reject/timeout.
type PlanApprovalSignal struct {
	Decision  PlanDecision `json:"decision"`
	Rationale string       `json:"rationale,omitempty"`
	Feedback  string       `json:"feedback,omitempty"`
}
