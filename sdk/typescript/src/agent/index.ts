import { FindingsWriter } from './findings-writer';
import { DirectiveWatcher, DirectiveCallback } from './directive-watcher';
import { requestHold } from './hold-client';
import type { AgentFinding, HoldRequest, HoldResponse } from '../types/agent';

export interface AgentConfig {
  socketDir?: string;
}

export class AgentSDK {
  private findingsWriter: FindingsWriter;
  private directiveWatcher: DirectiveWatcher;
  private socketDir: string;

  constructor(config: AgentConfig = {}) {
    this.socketDir = config.socketDir ?? '/var/run/agentcage';
    this.findingsWriter = new FindingsWriter(`${this.socketDir}/findings.sock`);
    this.directiveWatcher = new DirectiveWatcher(`${this.socketDir}/directives.json`);
  }

  async submitFinding(finding: AgentFinding): Promise<void> {
    return this.findingsWriter.submit(finding);
  }

  watchDirectives(callback: DirectiveCallback, pollIntervalMs?: number): void {
    this.directiveWatcher.watch(callback, pollIntervalMs);
  }

  async requestHold(req: HoldRequest): Promise<HoldResponse> {
    return requestHold(req, `${this.socketDir}/hold.sock`);
  }

  close(): void {
    this.findingsWriter.close();
    this.directiveWatcher.close();
  }
}

export type { AgentFinding, HoldRequest, HoldResponse, DirectiveInstruction, Directive } from '../types/agent';
export type { DirectiveCallback } from './directive-watcher';
export { newFindingId } from './ids';
export { fetch, type FetchOptions } from './fetch';
export { readCageEnv, type CageEnv, type CageType } from './env';
