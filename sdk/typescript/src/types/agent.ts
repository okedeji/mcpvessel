import { FindingStatus, Severity, DirectiveType } from './enums';
import { FindingKind } from './findings';

export interface AgentFinding {
  id: string;
  kind: FindingKind;
  status?: FindingStatus;
  severity: Severity;
  title: string;
  description?: string;
  // Required when kind=vulnerability, must be empty when kind=discovery.
  vulnClass?: string;
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
  // Required for kind=vulnerability. The validation cage replays
  // reproductionSteps to confirm the finding independently. Optional
  // (typically omitted) for kind=discovery.
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
