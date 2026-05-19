import { AssessmentStatus } from './enums';

export interface TargetScope {
  host: string;
  ports?: string[];
  paths?: string[];
  skipPaths?: string[];
}

export interface Guidance {
  attackSurface?: {
    endpoints?: string[];
    apiSpecs?: string[];
    limitToListed?: boolean;
  };
  strategy?: {
    context?: string;
    knownWeaknesses?: string[];
  };
}

export interface NotificationConfig {
  webhook?: string;
  onFinding?: boolean;
  onComplete?: boolean;
}

export interface Workflow {
  /**
   * Pause the assessment after discovery on a plan_approval intervention.
   * Default true. Set false (or pass --auto-approve-plan) for autonomous
   * runs where exploitation should follow discovery without a human gate.
   */
  requirePlanApproval?: boolean;
  /**
   * Inject an X-Agentcage-Pentest header on every outbound request to the
   * target, identifying the traffic as authorized pentest activity.
   * Default true. Set false (or pass --no-pentest-header) for
   * adversarial-simulation engagements.
   */
  identifyInRequests?: boolean;
}

export interface PlanProposalAction {
  type: string;
  scope: TargetScope;
  vulnClass: string;
  objective: string;
  priority?: number;
}

export interface PlanProposal {
  goal: string;
  summary: string;
  actions: PlanProposalAction[];
  estimatedCages: number;
  estimatedTokens: number;
  notes?: string;
}

export interface CageTypeConfig {
  vcpus?: number;
  memoryMb?: number;
  maxBatchSize?: number;
  maxDuration?: string;
}

export interface AssessmentConfig {
  name?: string;
  customerId: string;
  scope: TargetScope;
  totalTokenBudget?: number;
  maxDuration?: string;
  maxTotalCages?: number;
  maxIterations?: number;
  cageTypes?: Record<string, CageTypeConfig>;
  guidance?: Guidance;
  workflow?: Workflow;
  notifications?: NotificationConfig;
  tags?: Record<string, string>;
}

export interface AssessmentStats {
  totalCages: number;
  activeCages: number;
  findingsCandidate: number;
  findingsValidated: number;
  findingsRejected: number;
  tokensConsumed: number;
}

export interface AssessmentInfo {
  assessmentId: string;
  status: AssessmentStatus;
  customerId: string;
  config?: AssessmentConfig;
  stats?: AssessmentStats;
  createdAt?: Date;
}

export interface CreateAssessmentRequest {
  config: AssessmentConfig;
  bundleRef: string;
}

export interface ListAssessmentsRequest {
  statusFilter?: AssessmentStatus;
  limit?: number;
  pageToken?: string;
}

export interface Report {
  assessmentId: string;
  executiveSummary: string;
  methodology: string;
  findings: ReportFinding[];
  remediationRoadmap: string;
  generatedAt: Date;
}

export interface ReportFinding {
  id: string;
  title: string;
  severity: string;
  vulnClass: string;
  endpoint: string;
  cwe?: string;
  cvssScore?: number;
  remediation?: string;
  validationProof?: string;
}
