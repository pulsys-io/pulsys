import { csrfHeaders, isMutatingMethod } from './csrf';

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
    this.name = 'ApiError';
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const method = (init?.method ?? 'GET').toUpperCase();
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    ...(init?.headers as Record<string, string> | undefined),
  };
  if (isMutatingMethod(method)) {
    Object.assign(headers, csrfHeaders());
  }
  const res = await fetch(path, {
    ...init,
    credentials: 'include',
    headers,
  });
  if (!res.ok) {
    let msg = res.statusText;
    try {
      const j = (await res.json()) as { error?: string };
      if (j.error) msg = j.error;
    } catch {
      /* ignore */
    }
    throw new ApiError(msg, res.status);
  }
  if (res.status === 204) return undefined as T;
  return res.json() as Promise<T>;
}

export type Tenant = {
  id: string;
  name: string;
  display_name: string;
  created_at: string;
};

export type User = {
  id: string;
  email: string;
  display_name: string;
  role: string;
  is_active: boolean;
  created_at: string;
};

export type Token = {
  id: string;
  name: string;
  prefix: string;
  scopes: string[];
  last_used_at?: string;
  expires_at?: string;
  created_at: string;
  revoked_at?: string;
};

export type TokenCreateResult = Token & { secret: string };

export type AuditEntry = {
  id: string;
  actor_type: string;
  actor_id?: string;
  action: string;
  resource?: string;
  outcome: string;
  metadata: Record<string, unknown>;
  occurred_at: string;
};

export type CachedModel = {
  path: string;
  upstream_host: string;
  status_code: number;
  total_bytes?: number;
};

export type ModelGroup = {
  org: string;
  name: string;
  upstream_host: string;
  revisions?: string[] | null;
  file_count: number;
  total_bytes: number;
  files?: CachedModel[] | null;
};

export type GroupedListing = {
  items?: ModelGroup[] | null;
  grand_total_bytes: number;
};

export type CacheStats = {
  used_bytes: number;
  quota_bytes: number;
  free_disk_bytes: number;
  entry_count: number;
};

export type ImportJobProgress = {
  phase?: string;
  files_total?: number;
  files_done?: number;
  bytes_total?: number;
  bytes_done?: number;
  current_file?: string;
  current_file_bytes_total?: number;
  current_file_bytes_done?: number;
  download_bps?: number;
  message?: string;
  updated_at?: string;
};

export type ImportJobPayload = {
  repo_id?: string;
  revision?: string;
  repo_type?: string;
};

export type ImportJob = {
  id: string;
  type: string;
  status: 'queued' | 'running' | 'retrying' | 'succeeded' | 'failed' | 'canceled';
  payload?: ImportJobPayload | null;
  progress?: ImportJobProgress | null;
  error?: string;
  error_hint?: string;
  error_detail?: string;
  // ISO timestamp set when an operator has called Cancel but the row
  // has not yet moved to a terminal state. Drives the "Cancelling..."
  // badge and the Force-remove escape hatch in the imports UI.
  cancel_requested_at?: string;
  attempt: number;
  lease_owner?: string;
  lease_until?: string;
  started_at?: string;
  completed_at?: string;
  created_at: string;
  updated_at: string;
};

export const api = {
  tenant: () => request<Tenant>('/admin/api/v1/tenant'),
  users: (limit = 100) =>
    request<{ items: User[] }>(`/admin/api/v1/users?limit=${limit}`),
  tokens: (limit = 100) =>
    request<{ items: Token[] }>(`/admin/api/v1/tokens?limit=${limit}`),
  createToken: (body: { name: string; scopes?: string[]; expires_in_seconds?: number }) =>
    request<TokenCreateResult>('/admin/api/v1/tokens', {
      method: 'POST',
      body: JSON.stringify(body),
    }),
  revokeToken: (id: string) =>
    request<void>(`/admin/api/v1/tokens/${id}`, { method: 'DELETE' }),
  audit: (limit = 50) =>
    request<{ items: AuditEntry[] }>(`/admin/api/v1/audit?limit=${limit}`),
  imports: (limit = 20) =>
    request<{ items?: ImportJob[] | null }>(`/admin/api/v1/imports?limit=${limit}`),
  importDetail: (id: string) => request<ImportJob>(`/admin/api/v1/imports/${id}`),
  createImport: (body: { repo_id: string; revision?: string }) =>
    request<ImportJob>('/admin/api/v1/imports', {
      method: 'POST',
      body: JSON.stringify(body),
    }),
  cancelImport: (id: string) =>
    request<void>(`/admin/api/v1/imports/${encodeURIComponent(id)}/cancel`, { method: 'POST' }),
  // forceCancelImport bypasses River's "no delete running" safety and
  // flips the row directly to canceled. Use only when regular Cancel
  // doesn't take effect (orphaned row, dead worker process). The UI
  // surfaces it behind an explicit "Force remove" affordance.
  forceCancelImport: (id: string) =>
    request<void>(`/admin/api/v1/imports/${encodeURIComponent(id)}/force-cancel`, {
      method: 'POST',
    }),
  deleteImport: (id: string) =>
    request<void>(`/admin/api/v1/imports/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  models: (limit = 200) =>
    request<{ items: CachedModel[] }>(`/admin/api/v1/models?limit=${limit}`),
  modelsGrouped: (limit = 500, includeFiles = true) =>
    request<GroupedListing>(
      `/admin/api/v1/models/grouped?limit=${limit}&include_files=${includeFiles}`,
    ),
  cacheStats: () => request<CacheStats>('/admin/api/v1/cache/stats'),
  purgeCacheModel: (org: string, name: string) =>
    request<{ purged: number; trimmed: number; bytes_freed: number }>(
      `/admin/api/v1/models/cache`,
      {
        method: 'DELETE',
        body: JSON.stringify({ org, name }),
      },
    ),
};
