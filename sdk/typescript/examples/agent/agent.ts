/**
 * Starter agent for agentcage.
 *
 * One image, three cage types. Dispatches on AGENTCAGE_CAGE_TYPE:
 *
 *   discovery     → cages/discovery.ts      map attack surface
 *   exploitation  → cages/exploitation/    probe one vuln class
 *   validation    → cages/validation.ts    placeholder
 *
 * Shared platform helpers live in lib/ (env, sdk, auth, http, llm).
 * Each cage type imports only what it needs. To add a new vuln class
 * to the exploitation cage, drop one file in cages/exploitation/ and
 * register it in that directory's index.ts.
 */

import { env } from './lib/env';
import { auth } from './lib/auth';
import { agent } from './lib/sdk';
import { runDiscovery } from './cages/discovery';
import { runExploitation } from './cages/exploitation';
import { runValidation } from './cages/validation';

console.log(`Agent starting. cage_type=${env.cageType} target=${env.target}`);
if (env.vulnClass) {
  console.log(`vuln_class=${env.vulnClass} paths=${env.scopePaths.join(',') || '(none)'}`);
}
console.log(auth ? `auth: ${auth.type} ${auth.name}` : 'auth: none (unauthenticated probes)');

async function main(): Promise<void> {
  switch (env.cageType) {
    case 'discovery':
      await runDiscovery();
      break;
    case 'exploitation':
      await runExploitation();
      break;
    case 'validation':
      await runValidation();
      break;
  }
  agent.close();
}

main().catch((err) => {
  console.error('Agent error:', err);
  agent.close();
  process.exit(1);
});
