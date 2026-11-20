// Typed fetch wrapper for the llm-relay dashboard API.
// Contract: docs/api-contract.md — errors are {"error":{"message":"..."}}.

export type Role = 'admin' | 'user';

export interface User {
  id: number;
  username: string;
  display_name?: string;
  role: Role;
  disabled?: boolean;
  created_at?: string;
}

export interface Session {
  token: string;
  user: User;
}

export interface ApiKey {
  id: number;
  name: string;
  prefix: string;
  created_at: string;
  last_used_at: string | null;
  revoked_at: string | null;
}

export interface CreatedKey {
  id: number;
  name: string;
  key: string;
  prefix: string;
}

export interface UsageTotals {
  requests: number;
  prompt_tokens: number;
  completion_tokens: number;
  cached_tokens: number;
  cache_write_tokens: number;
  cost_micro: number;
}

export interface UsageGroup extends UsageTotals {
  key: string;
  label: string;
}

export interface UsageSummary {
  currency: string;
  totals: UsageTotals;
  groups: UsageGroup[];
}

export type GroupBy = 'day' | 'user' | 'key' | 'model' | 'ip' | 'endpoint' | 'session';

export interface UsageRow {
  id: number;
  ts: string;
  username?: string;
  key_name: string;
  client_ip: string;
  session_id: string | null;
  endpoint: string;
  model_requested: string;
  prompt_tokens: number;
  completion_tokens: number;
  cached_tokens: number;
  cache_write_tokens: number;
  cost_micro: number;
  streamed: boolean;
  usage_estimated: boolean;
  status_code: number;
  latency_ms: number;
  error: string | null;
}

export interface UsageDetail {
  total: number;
  rows: UsageRow[];
}

export interface AdminUser {
  id: number;
  username: string;
  display_name?: string;
  role: Role;
  disabled: boolean;
  created_at: string;
  /** Admin-only: never exposed on /api/me. */
  price_multiplier: number;
}

export interface Backend {
  id: number;
  name: string;
  base_url: string;
  has_key: boolean;
  enabled: boolean;
  last_probe_at: string | null;
  last_probe_error: string | null;
  server_version: string | null;
  created_at: string;
  model_count: number;
}

export interface ProbeModel {
  id: string;
  in_catalog: boolean;
  model_id: number | null;
}

export interface ProbeResult {
  ok: boolean;
  server_version: string;
  error: string;
  /** Null when the probe failed or the upstream list was empty (nil slice → null). */
  models: ProbeModel[] | null;
}

export interface CatalogModel {
  id: number;
  name: string;
  backend_id: number;
  backend_name: string;
  upstream_model: string;
  enabled: boolean;
  price_in: number;
  price_out: number;
  price_cache_read: number;
  price_cache_write: number;
  context_length: number;
  concurrency_limit: number;
  bench_tok_per_s: number;
  bench_at: string;
}

/** Result of POST /api/admin/models/{id}/benchmark. */
export interface BenchmarkResult {
  bench_tok_per_s: number;
  completion_tokens: number;
  seconds: number;
  bench_at: string;
}

/** Model as exposed to all users on GET /api/models (no internal wiring). */
export interface PublicModel {
  name: string;
  price_in: number;
  price_out: number;
  price_cache_read: number;
  price_cache_write: number;
  context_length: number;
  concurrency_limit: number;
  bench_tok_per_s: number;
  bench_at: string;
}

export interface Settings {
  currency: string;
}

export interface Info {
  relay_url: string;
  dashboard_url: string;
  currency: string;
}

export class ApiError extends Error {
  status: number;
  constructor(message: string, status: number) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

export function errMsg(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

// ---------- session storage ----------

const TOKEN_KEY = 'llmrel…oken';
const USER_KEY = 'llmrel…user';

export function getToken(): string | null {
  return localStorage.getItem(TOKEN_KEY);
}

export function loadStoredUser(): User | null {
  const raw = localStorage.getItem(USER_KEY);
  if (!raw) return null;
  try {
    return JSON.parse(raw) as User;
  } catch {
    return null;
  }
}

export function saveSession(s: Session): void {
  localStorage.setItem(TOKEN_KEY, s.token);
  localStorage.setItem(USER_KEY, JSON.stringify(s.user));
}

export function clearSession(): void {
  localStorage.removeItem(TOKEN_KEY);
  localStorage.removeItem(USER_KEY);
}

// Called after a 401 clears storage; the auth provider overrides it.
let onUnauthorized: () => void = () => {
  window.location.replace('/login');
};

export function setUnauthorizedHandler(fn: () => void): void {
  onUnauthorized = fn;
}

// ---------- fetch core ----------

type Query = Record<string, string | number | undefined | null>;

interface RequestOptions {
  body?: unknown;
  query?: Query;
  /**
   * For endpoints where 401 is a normal business error (e.g. wrong old
   * password on change_password): do not clear the session on 401, let the
   * error surface to the caller instead.
   */
  noSessionClear?: boolean;
}

async function request<T>(method: string, path: string, opts: RequestOptions = {}): Promise<T> {
  let url = path;
  if (opts.query) {
    const qs = new URLSearchParams();
    for (const [k, v] of Object.entries(opts.query)) {
      if (v !== undefined && v !== null && v !== '') qs.set(k, String(v));
    }
    const s = qs.toString();
    if (s) url += '?' + s;
  }

  const headers: Record<string, string> = {};
  const token = getToken();
  if (token) headers['Authorization'] = 'Bearer ' + token;
  if (opts.body !== undefined) headers['Content-Type'] = 'application/json';

  let res: Response;
  try {
    res = await fetch(url, {
      method,
      headers,
      body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined,
    });
  } catch {
    throw new ApiError('Network error — the server is not reachable.', 0);
  }

  if (res.status === 401 && !path.endsWith('/auth/login') && !opts.noSessionClear) {
    clearSession();
    onUnauthorized();
    throw new ApiError('Session expired — please log in again.', 401);
  }

  if (res.status === 204) return undefined as T;

  let data: unknown = null;
  try {
    data = await res.json();
  } catch {
    // body-less or non-JSON response
  }

  if (!res.ok) {
    const errObj = data as { error?: { message?: string } } | null;
    const msg = errObj?.error?.message ?? `Request failed (HTTP ${res.status}).`;
    throw new ApiError(msg, res.status);
  }

  return data as T;
}

// ---------- endpoints ----------

export type UsageQuery = {
  from: string;
  to: string;
  user_id?: number;
  key_id?: number;
  model?: string;
};

export const api = {
  // auth
  login: (username: string, password: string) =>
    request<Session>('POST', '/api/auth/login', { body: { username, password } }),
  me: () => request<User>('GET', '/api/me'),
  patchMe: (display_name: string) => request<User>('PATCH', '/api/me', { body: { display_name } }),
  changePassword: (old_password: string, new_password: string) =>
    request<void>('POST', '/api/auth/change_password', {
      body: { old_password, new_password },
      // A 401 here means the old password was wrong, not an expired session.
      noSessionClear: true,
    }),
  info: () => request<Info>('GET', '/api/info'),

  // api keys (own)
  keys: () => request<{ keys: ApiKey[] }>('GET', '/api/keys'),
  createKey: (name: string) => request<CreatedKey>('POST', '/api/keys', { body: { name } }),
  deleteKey: (id: number) => request<void>('DELETE', `/api/keys/${id}`),

  // public model catalog (any user)
  getModels: () => request<{ models: PublicModel[] }>('GET', '/api/models'),

  // usage
  usageSummary: (admin: boolean, q: UsageQuery & { group_by: GroupBy }) =>
    request<UsageSummary>('GET', admin ? '/api/admin/usage/summary' : '/api/usage/summary', {
      query: q,
    }),
  usageDetail: (admin: boolean, q: UsageQuery & { page: number; page_size: number }) =>
    request<UsageDetail>('GET', admin ? '/api/admin/usage/detail' : '/api/usage/detail', {
      query: q,
    }),

  // admin — users
  users: () => request<{ users: AdminUser[] }>('GET', '/api/admin/users'),
  createUser: (username: string, password: string, role: Role, display_name?: string) =>
    request<AdminUser>('POST', '/api/admin/users', {
      body: display_name ? { username, display_name, password, role } : { username, password, role },
    }),
  patchUser: (
    id: number,
    patch: {
      password?: string;
      disabled?: boolean;
      role?: Role;
      display_name?: string;
      price_multiplier?: number;
    },
  ) => request<AdminUser>('PATCH', `/api/admin/users/${id}`, { body: patch }),

  // admin — backends
  backends: () => request<{ backends: Backend[] }>('GET', '/api/admin/backends'),
  createBackend: (name: string, base_url: string, api_key?: string) =>
    request<Backend>('POST', '/api/admin/backends', {
      body: api_key ? { name, base_url, api_key } : { name, base_url },
    }),
  patchBackend: (
    id: number,
    patch: { name?: string; base_url?: string; api_key?: string; enabled?: boolean },
  ) => request<Backend>('PATCH', `/api/admin/backends/${id}`, { body: patch }),
  deleteBackend: (id: number) => request<void>('DELETE', `/api/admin/backends/${id}`),
  probeBackend: (id: number) => request<ProbeResult>('POST', `/api/admin/backends/${id}/probe`),
  addBackendModels: (id: number, model_ids: string[]) =>
    request<{
      created: { id: number; name: string; upstream_model: string }[];
      skipped: string[];
    }>('POST', `/api/admin/backends/${id}/models`, { body: { model_ids } }),

  // admin — models
  models: () => request<{ models: CatalogModel[] }>('GET', '/api/admin/models'),
  patchModel: (
    id: number,
    patch: Partial<{
      name: string;
      enabled: boolean;
      price_in: number;
      price_out: number;
      price_cache_read: number;
      price_cache_write: number;
      context_length: number;
      concurrency_limit: number;
      bench_tok_per_s: number;
    }>,
  ) => request<CatalogModel>('PATCH', `/api/admin/models/${id}`, { body: patch }),
  deleteModel: (id: number) => request<void>('DELETE', `/api/admin/models/${id}`),
  /** Runs a timed generation upstream; can take minutes (409 if one is running). */
  benchmarkModel: (id: number) =>
    request<BenchmarkResult>('POST', `/api/admin/models/${id}/benchmark`),

  // admin — settings
  settings: () => request<Settings>('GET', '/api/admin/settings'),
  putSettings: (currency: string) =>
    request<Settings>('PUT', '/api/admin/settings', { body: { currency } }),
};
