export enum AssessmentStatus {
  Unspecified = 'ASSESSMENT_STATUS_UNSPECIFIED',
  Discovery = 'ASSESSMENT_STATUS_DISCOVERY',
  AwaitingPlanApproval = 'ASSESSMENT_STATUS_AWAITING_PLAN_APPROVAL',
  Exploitation = 'ASSESSMENT_STATUS_EXPLOITATION',
  Validation = 'ASSESSMENT_STATUS_VALIDATION',
  PendingReview = 'ASSESSMENT_STATUS_PENDING_REVIEW',
  Approved = 'ASSESSMENT_STATUS_APPROVED',
  Rejected = 'ASSESSMENT_STATUS_REJECTED',
  Failed = 'ASSESSMENT_STATUS_FAILED',
  Unreviewed = 'ASSESSMENT_STATUS_UNREVIEWED',
  PlanUnapproved = 'ASSESSMENT_STATUS_PLAN_UNAPPROVED',
}

export enum FindingStatus {
  Candidate = 'candidate',
  Validated = 'validated',
  Rejected = 'rejected',
}

export enum Severity {
  Info = 'info',
  Low = 'low',
  Medium = 'medium',
  High = 'high',
  Critical = 'critical',
}

export enum InterventionType {
  TripwireEscalation = 'tripwire_escalation',
  PayloadReview = 'payload_review',
  ReportReview = 'report_review',
  PolicyViolation = 'policy_violation',
  AgentHold = 'agent_hold',
  PlanApproval = 'plan_approval',
}

export enum PlanDecision {
  Approve = 'approve',
  Reject = 'reject',
  Modify = 'modify',
}

export enum InterventionStatus {
  Pending = 'pending',
  Resolved = 'resolved',
  TimedOut = 'timed_out',
}

export enum InterventionAction {
  Resume = 'resume',
  Kill = 'kill',
  Allow = 'allow',
  Block = 'block',
}

export enum ReviewDecision {
  Approve = 'approve',
  Reject = 'reject',
  Retest = 'retest',
}

export enum HostPool {
  Provisioning = 'provisioning',
  Warm = 'warm',
  Active = 'active',
  Draining = 'draining',
}

export enum CageType {
  Discovery = 'discovery',
  Validation = 'validation',
  Exploitation = 'exploitation',
}

export enum DirectiveType {
  Continue = 'continue',
  Redirect = 'redirect',
  Terminate = 'terminate',
  HoldResult = 'hold_result',
}
