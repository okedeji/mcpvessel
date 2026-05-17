import { AssessmentStatus } from './enums';

export interface TargetScope {
  hosts: string[];
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
  priorities?: {
    vulnClasses?: string[];
    skipPaths?: string[];
  };
  strategy?: {
    context?: string;
    knownWeaknesses?: string[];
  };
  validation?: {
    requirePoc?: boolean;
    headlessBrowserXss?: boolean;
  };
}

export interface NotificationConfig {
  webhook?: string;
  onFinding?: boolean;
  onComplete?: boolean;
}

export interface CageTypeConfig {
  vcpus?: number;
  memoryMb?: number;
  maxBatchSize?: number;
  maxDuration?: string;
}

export interface PayloadPattern {
  pattern: string;
  reason: string;
}

export interface PayloadConfig {
  extraBlock?: PayloadPattern[];
  extraFlag?: PayloadPattern[];
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
  payload?: PayloadConfig;
  guidance?: Guidance;
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
