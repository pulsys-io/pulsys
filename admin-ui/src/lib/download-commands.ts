import type { ModelGroup } from '@/lib/api';

export function modelRepoId(group: ModelGroup): string {
  return `${group.org}/${group.name}`;
}

export function modelRevision(group: ModelGroup): string {
  const revs = group.revisions ?? [];
  if (revs.includes('main')) return 'main';
  return revs[0] ?? 'main';
}

export function modelLocalDir(group: ModelGroup): string {
  return group.name || 'model';
}

/** HF cache ingress port when it is not co-located on the console origin. */
export function proxyPortForConsole(consolePort: string): string {
  const override = process.env.NEXT_PUBLIC_PROXY_PORT?.trim();
  if (override) return override;
  if (consolePort === '3000') return '8082';
  if (consolePort === '3001') return '8080';
  return '8080';
}

/** Proxy ingress URL derived from the console page URL (same host, HF port). */
export function proxyEndpointFromWindow(loc: Pick<Location, 'protocol' | 'hostname' | 'port'>): string {
  const configured = process.env.NEXT_PUBLIC_PROXY_BASE_URL?.replace(/\/$/, '');
  if (configured) return configured;
  const port = proxyPortForConsole(loc.port);
  return `${loc.protocol}//${loc.hostname}:${port}`;
}

export function buildDownloadCommands(group: ModelGroup, endpoint: string): string {
  const repo = modelRepoId(group);
  const revision = modelRevision(group);
  const localDir = modelLocalDir(group);
  return [
    `export HF_ENDPOINT="${endpoint}"`,
    'export HF_TOKEN="YOUR_PULSYS_TOKEN"',
    `hf download "${repo}" --revision "${revision}" --local-dir "./${localDir}"`,
  ].join('\n');
}
