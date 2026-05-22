import { FindingStatus, Severity } from './enums';

export enum FindingKind {
  Vulnerability = 'vulnerability',
  Discovery = 'discovery',
  ValidationProof = 'validation_proof',
}

export interface Evidence {
  request?: Buffer;
  response?: Buffer;
  screenshot?: Buffer;
  poc?: string;
  metadata?: Record<string, string>;
}

export interface ValidationProof {
  reproductionSteps: string;
  confirmed: boolean;
  deterministic: boolean;
  validatorCageId: string;
  evidence?: string;
}

export interface Finding {
  id: string;
  assessmentId: string;
  cageId: string;
  kind: FindingKind;
  status: FindingStatus;
  severity: Severity;
  title: string;
  description?: string;
  vulnClass?: string;
  endpoint: string;
  evidence?: Evidence;
  /**
   * True when the finding has a screenshot. Surfaced separately from
   * `evidence.screenshot` because list responses strip evidence bytes
   * to keep wire size bounded; the full screenshot is only returned by
   * a single-finding fetch.
   */
  hasScreenshot?: boolean;
  parentFindingId?: string;
  chainDepth?: number;
  cwe?: string;
  cvssScore?: number;
  remediation?: string;
  validationProof?: ValidationProof;
  createdAt?: Date;
  updatedAt?: Date;
  validatedAt?: Date;
}

export interface ListFindingsRequest {
  assessmentId: string;
  statusFilter?: FindingStatus;
  severityFilter?: Severity;
  limit?: number;
}

export interface DeleteByAssessmentResponse {
  deleted: number;
}
