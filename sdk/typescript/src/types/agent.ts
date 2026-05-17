import { FindingStatus, Severity, DirectiveType } from './enums';

export interface AgentFinding {
  id: string;
  status?: FindingStatus;
  severity: Severity;
  title: string;
  description?: string;
  vulnClass: string;
  endpoint: string;
  evidence?: {
    request?: Buffer;
    response?: Buffer;
    screenshot?: Buffer;
    poc?: string;
    metadata?: Record<string, string>;
  };
  parentFindingId?: string;
  chainDepth?: number;
  cwe?: string;
  cvssScore?: number;
  remediation?: string;
  // Required for vulnerability findings (severity != info). The validator
  // cage replays reproductionSteps to confirm the finding independently.
  // Optional for surface/info findings discovered during mapping.
  validationProof?: {
    reproductionSteps: string;
    confirmed: boolean;
    deterministic: boolean;
    validatorCageId: string;
    evidence?: string;
  };
}

export interface DirectiveInstruction {
  type: DirectiveType;
  message?: string;
  holdId?: string;
  allowed?: boolean;
  reason?: string;
}

export interface Directive {
  sequence: number;
  instructions: DirectiveInstruction[];
}

export interface HoldRequest {
  holdId: string;
  message: string;
  context?: Record<string, unknown>;
}

export interface HoldResponse {
  holdId: string;
  allowed: boolean;
  message?: string;
}
