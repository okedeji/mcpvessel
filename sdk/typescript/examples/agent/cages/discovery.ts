// Discovery cage: map the target's attack surface and file one
// discovery finding per interesting endpoint. No exploitation.
//
// Flow: fetch seed pages → ask LLM for paths → crawl → ask LLM to
// analyze the surface → submit findings.

import { FindingKind, Severity, newFindingId } from '@agentcage/sdk';
import { agent, isTerminated } from '../lib/sdk';
import { env } from '../lib/env';
import { fetchSafe, HttpResponse } from '../lib/http';
import { askLLM, extractJSON } from '../lib/llm';

interface SeedData {
  homepage: HttpResponse | null;
  robots: HttpResponse | null;
  sitemap: HttpResponse | null;
}

async function seedCrawl(t: string): Promise<SeedData> {
  console.log('Fetching seed pages (/, /robots.txt, /sitemap.xml)...');
  const [homepage, robots, sitemap] = await Promise.all([
    fetchSafe(`https://${t}/`),
    fetchSafe(`https://${t}/robots.txt`),
    fetchSafe(`https://${t}/sitemap.xml`),
  ]);
  return { homepage, robots, sitemap };
}

function extractLinks(html: string): string[] {
  const paths: string[] = [];
  const seen = new Set<string>();
  const pattern = /href=["']([^"']+)["']/g;
  let match;
  while ((match = pattern.exec(html)) !== null) {
    const href = match[1];
    if (href.startsWith('/') && !href.startsWith('//') && !seen.has(href)) {
      seen.add(href);
      paths.push(href);
    }
  }
  return paths.slice(0, 50);
}

async function planCrawl(t: string, seed: SeedData): Promise<string[]> {
  console.log('Asking LLM to plan crawl paths...');
  let seedSummary = '';
  if (seed.homepage) {
    seedSummary += `Homepage (HTTP ${seed.homepage.status}):\n${seed.homepage.body.slice(0, 2000)}\n\n`;
    seedSummary += `Response headers: ${JSON.stringify(seed.homepage.headers)}\n\n`;
  } else {
    seedSummary += 'Homepage: unreachable\n\n';
  }
  if (seed.robots && seed.robots.status === 200) {
    seedSummary += `robots.txt:\n${seed.robots.body.slice(0, 2000)}\n\n`;
  }
  if (seed.sitemap && seed.sitemap.status === 200) {
    seedSummary += `sitemap.xml:\n${seed.sitemap.body.slice(0, 2000)}\n\n`;
  }
  if (env.scopePaths.length > 0) {
    seedSummary += `Operator-supplied paths in scope (already queued for crawl):\n${env.scopePaths.join('\n')}\n\n`;
  }

  const response = await askLLM([
    {
      role: 'system',
      content: `You are a penetration testing assistant performing attack surface discovery.

Based on the target's homepage, robots.txt, sitemap, and any operator-supplied paths, determine what additional paths to crawl. Return a JSON array of paths to crawl (max 50): ["/api/v1", "/login", "/admin", ...]

Only include paths likely to exist on THIS target. Do not guess generic paths with no evidence. Skip paths already listed under operator-supplied paths (they're already queued).`,
    },
    { role: 'user', content: `Target: ${t}\nObjective: ${env.objective || 'Full security assessment'}\n\n${seedSummary}` },
  ]);

  let planned: string[] = [];
  try {
    const paths = extractJSON(response);
    if (Array.isArray(paths)) {
      planned = paths.filter((p: any) => typeof p === 'string' && p.startsWith('/')).slice(0, 50);
    }
  } catch {
    console.log('LLM response was not valid JSON, raw:', response.slice(0, 200));
  }

  if (planned.length === 0) {
    const links = extractLinks(seed.homepage?.body ?? '');
    if (links.length > 0) {
      console.log(`Extracted ${links.length} links from homepage HTML`);
      planned = links;
    } else {
      console.log('No paths from LLM or HTML, using minimal seed list');
      planned = ['/', '/api', '/login', '/admin', '/docs', '/graphql', '/health', '/search', '/sitemap.xml'];
    }
  }

  // Always crawl operator-supplied paths first, deduplicated against LLM plan.
  const seen = new Set(env.scopePaths);
  return [...env.scopePaths, ...planned.filter((p) => !seen.has(p))];
}

interface Endpoint {
  path: string;
  status: number;
  contentType: string;
  headers: Record<string, string>;
  snippet: string;
}

async function crawlPaths(t: string, paths: string[]): Promise<Endpoint[]> {
  const endpoints: Endpoint[] = [];
  for (const path of paths) {
    if (isTerminated()) break;
    const resp = await fetchSafe(`https://${t}${path}`);
    if (resp && resp.status < 500) {
      endpoints.push({
        path,
        status: resp.status,
        contentType: resp.headers['content-type'] ?? '',
        headers: resp.headers,
        snippet: resp.body.slice(0, 500),
      });
    }
  }
  return endpoints;
}

interface SurfaceEntry {
  endpoint: string;
  technologies: string[];
  vuln_classes: string[];
  priority: string;
  reason: string;
}

async function analyzeSurface(t: string, endpoints: Endpoint[]): Promise<SurfaceEntry[]> {
  console.log('Asking LLM to analyze attack surface...');
  const summary = endpoints
    .map((e) => `${e.path} [HTTP ${e.status}] ${e.contentType}\n${e.snippet.slice(0, 200)}`)
    .join('\n\n');
  const response = await askLLM([
    {
      role: 'system',
      content: `You are a penetration testing assistant. Analyze crawled endpoints and produce a prioritized attack surface map.

For each interesting endpoint, identify technologies, applicable vuln_classes, priority, and reason.
Return JSON array (max 20):
[{"endpoint": "/path", "technologies": ["express"], "vuln_classes": ["sqli", "xss"], "priority": "high", "reason": "why"}]`,
    },
    { role: 'user', content: `Target: ${t}\n\nCrawled endpoints:\n${summary}` },
  ]);

  try {
    const parsed = extractJSON(response);
    if (Array.isArray(parsed) && parsed.length > 0) return parsed;
  } catch {
    console.log('LLM analysis was not valid JSON, raw:', response.slice(0, 200));
  }

  console.log('Falling back to raw endpoints as surface map');
  return endpoints
    .filter((e) => e.status < 400)
    .map((e) => ({
      endpoint: e.path,
      technologies: [],
      vuln_classes: ['unknown'],
      priority: 'medium',
      reason: `HTTP ${e.status}, ${e.contentType}`,
    }));
}

async function submitSurface(t: string, surface: SurfaceEntry[]): Promise<void> {
  for (const entry of surface) {
    if (isTerminated()) break;
    await agent.submitFinding({
      id: newFindingId(),
      kind: FindingKind.Discovery,
      severity: Severity.Info,
      title: `Discovered: ${entry.endpoint}`,
      endpoint: `https://${t}${entry.endpoint}`,
      description: `${entry.reason}. Technologies: ${entry.technologies.join(', ') || 'unknown'}. Suggested tests: ${entry.vuln_classes.join(', ')}. Priority: ${entry.priority}.`,
      evidence: {
        metadata: {
          priority: entry.priority,
          vuln_classes: entry.vuln_classes.join(','),
          technologies: entry.technologies.join(','),
        },
      },
    });
    console.log(`  [${entry.priority}] ${entry.endpoint} → ${entry.vuln_classes.join(', ')}`);
  }
}

export async function runDiscovery(): Promise<void> {
  console.log(`\n── Discovering ${env.target} ──`);
  const seed = await seedCrawl(env.target);
  const paths = await planCrawl(env.target, seed);
  console.log(`LLM planned ${paths.length} paths to crawl`);
  const endpoints = await crawlPaths(env.target, paths);
  console.log(`${endpoints.length} live endpoints found`);
  if (endpoints.length === 0) {
    console.log('No live endpoints, target may be unreachable');
    return;
  }
  const surface = await analyzeSurface(env.target, endpoints);
  console.log(`${surface.length} interesting endpoints identified`);
  await submitSurface(env.target, surface);
  console.log('\nDiscovery complete.');
}
