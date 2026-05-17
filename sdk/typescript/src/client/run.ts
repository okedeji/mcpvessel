import { AssessmentService } from './assessment';
import type { AssessmentInfo, AssessmentConfig, AssessmentStats } from '../types/assessment';
import { AssessmentStatus } from '../types/enums';

/** Mirrors the CLI flags for `agentcage run`. */
export interface RunConfig {
  // Required
  agent: string;                   // --agent (path to .cage bundle or bundleRef)
  target: string;                  // --target (one host per assessment)
  customerId: string;              // --customer-id

  // Target scoping
  ports?: string[];                // --port (repeatable)
  paths?: string[];                // --path (repeatable)
  skipPaths?: string[];            // --skip-path (repeatable)

  // Budget & limits
  tokenBudget?: number;            // --token-budget
  maxDuration?: string;            // --max-duration
  maxTotalCages?: number;          // --max-total-cages
  maxIterations?: number;          // --max-iterations

  // Guidance
  context?: string;                // --context
  focus?: string[];                // --focus (repeatable)
  deprioritize?: string[];         // --deprioritize (repeatable)
  endpoints?: string[];            // --endpoint (repeatable)
  apiSpecs?: string[];             // --api-spec (repeatable)
  knownWeaknesses?: string[];      // --known-weakness (repeatable)
  requirePoc?: boolean;            // --require-poc
  headlessXss?: boolean;           // --headless-xss

  // Notifications
  notify?: string;                 // --notify (webhook URL)
  notifyOnFinding?: boolean;       // --notify-on-finding
  notifyOnComplete?: boolean;      // --notify-on-complete

  // Output
  follow?: boolean;                // --follow
  name?: string;                   // --name
  tags?: Record<string, string>;   // --tag (repeatable)

  // SDK-only
  pollIntervalMs?: number;         // poll interval for follow mode
}

export interface RunEvent {
  type: 'status' | 'stats' | 'complete' | 'error';
  assessmentId: string;
  status?: AssessmentStatus;
  stats?: AssessmentStats;
  error?: string;
}

const TERMINAL_STATUSES = new Set([
  AssessmentStatus.Approved,
  AssessmentStatus.Rejected,
  AssessmentStatus.Failed,
  AssessmentStatus.PendingReview,
  AssessmentStatus.Unspecified,
]);

/** Convert CLI-style RunConfig to the proto AssessmentConfig. */
function buildAssessmentConfig(rc: RunConfig): AssessmentConfig {
  return {
    name: rc.name,
    customerId: rc.customerId,
    scope: {
      hosts: [rc.target],
      ports: rc.ports,
      paths: rc.paths,
      skipPaths: rc.skipPaths,
    },
    totalTokenBudget: rc.tokenBudget,
    maxDuration: rc.maxDuration,
    maxTotalCages: rc.maxTotalCages,
    maxIterations: rc.maxIterations,
    guidance: {
      attackSurface: (rc.endpoints?.length || rc.apiSpecs?.length) ? {
        endpoints: rc.endpoints,
        apiSpecs: rc.apiSpecs,
      } : undefined,
      priorities: (rc.focus?.length || rc.deprioritize?.length) ? {
        vulnClasses: rc.focus,
        skipPaths: rc.deprioritize,
      } : undefined,
      strategy: (rc.context || rc.knownWeaknesses?.length) ? {
        context: rc.context,
        knownWeaknesses: rc.knownWeaknesses,
      } : undefined,
      validation: (rc.requirePoc || rc.headlessXss) ? {
        requirePoc: rc.requirePoc,
        headlessBrowserXss: rc.headlessXss,
      } : undefined,
    },
    notifications: (rc.notify || rc.notifyOnFinding || rc.notifyOnComplete) ? {
      webhook: rc.notify,
      onFinding: rc.notifyOnFinding,
      onComplete: rc.notifyOnComplete,
    } : undefined,
    tags: rc.tags,
  };
}

export async function run(
  service: AssessmentService,
  rc: RunConfig,
): Promise<AssessmentInfo> {
  const config = buildAssessmentConfig(rc);

  const info = await service.create({
    config,
    bundleRef: rc.agent,
  });

  if (!rc.follow) {
    return info;
  }

  return followAssessment(service, info.assessmentId, rc.pollIntervalMs ?? 3000);
}

export async function* follow(
  service: AssessmentService,
  assessmentId: string,
  pollIntervalMs = 3000,
): AsyncGenerator<RunEvent> {
  let lastStatus = '';
  let lastValidated = -1;
  let lastCandidate = -1;

  while (true) {
    const info = await service.get(assessmentId);
    const status = info.status;

    if (status !== lastStatus) {
      lastStatus = status;
      yield { type: 'status', assessmentId, status: status as AssessmentStatus };
    }

    if (info.stats) {
      const v = info.stats.findingsValidated;
      const c = info.stats.findingsCandidate;
      if (v !== lastValidated || c !== lastCandidate) {
        lastValidated = v;
        lastCandidate = c;
        yield { type: 'stats', assessmentId, stats: info.stats };
      }
    }

    if (TERMINAL_STATUSES.has(status as AssessmentStatus)) {
      yield { type: 'complete', assessmentId, status: status as AssessmentStatus };
      return;
    }

    await sleep(pollIntervalMs);
  }
}

async function followAssessment(
  service: AssessmentService,
  assessmentId: string,
  pollIntervalMs: number,
): Promise<AssessmentInfo> {
  while (true) {
    const info = await service.get(assessmentId);
    if (TERMINAL_STATUSES.has(info.status as AssessmentStatus)) {
      return info;
    }
    await sleep(pollIntervalMs);
  }
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
