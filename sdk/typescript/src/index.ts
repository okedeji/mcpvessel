// Client — everything the CLI does, as typed async methods.
export { AgentCage, type AgentCageConfig, type ApiKeyAuth } from './client';
export type { VaultConfig, RunConfig, RunEvent, ApiKeyInfo, PackOptions, PackResult } from './client';
export { generateJoinScript, type JoinOptions } from './client/join';

// Agent SDK — for TypeScript agents running inside cages.
export { AgentSDK, type AgentConfig, newFindingId, fetch, type FetchOptions, readCageEnv, type CageEnv } from './agent';
// Note: CageType is re-exported below from types/enums (the orchestrator-side enum).
// The agent-side CageType from readCageEnv is a string-literal union and is
// referenced via the CageEnv shape.

// Judge: building blocks for writing a payload-safety classifier
// service. Types describe the wire format; validators parse and check
// incoming requests / outgoing responses. The customer brings their
// own HTTP framework, auth, observability, and LLM logic.
export { validatePayloads, validateResults } from './judge';
export type { JudgePayload, JudgeResult } from './judge';

// Provisioner server — HTTP framework for bare-metal host management.
export { createProvisionerServer, type ProvisionerHandler, type ProvisionerServerConfig } from './provisioner';

// Vault client — direct Vault HTTP API access for secret management.
export { VaultClient } from './client/vault';

// Access client — API key management via Vault.
export { AccessClient } from './client/access';

// Shared types.
export * from './types/enums';
export type { AssessmentInfo, AssessmentConfig, Report, CreateAssessmentRequest, ListAssessmentsRequest, Workflow, PlanProposal, PlanProposalAction } from './types/assessment';
export type { Finding, Evidence, ValidationProof, ListFindingsRequest } from './types/findings';
export { FindingKind } from './types/findings';
export type { Intervention, ListInterventionsRequest, ResolveCageRequest, ResolveReviewRequest } from './types/intervention';
export type { FleetStatus, HostInfo, Capacity, DrainHostRequest } from './types/fleet';
export type { CageInfo, CageLogs } from './types/cage';
export type { AuditEntry, AuditDigest, ChainStatus, VerifyResult } from './types/audit';
export type { AgentFinding, Directive, DirectiveInstruction, HoldRequest, HoldResponse } from './types/agent';
export type { ProvisionResult, StatusResult } from './types/provisioner';
