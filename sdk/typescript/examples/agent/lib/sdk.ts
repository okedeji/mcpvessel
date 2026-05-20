// AgentSDK singleton and terminate signal.
//
// Exposes `isTerminated()` as a getter rather than a `let` export so
// consumers don't have to reason about live-binding semantics across
// the CommonJS compile step.

import { AgentSDK, DirectiveInstruction } from '@agentcage/sdk';

export const agent = new AgentSDK();

let terminated = false;
export const isTerminated = (): boolean => terminated;

agent.watchDirectives((directive: DirectiveInstruction) => {
  if (directive.type === 'terminate') {
    terminated = true;
    agent.close();
    process.exit(0);
  }
});
