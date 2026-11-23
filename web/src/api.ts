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
  /** Upstream id the chosen member served; for a group this tells the
   *  members apart when their upstream ids differ. */
  model_upstream?: string | null;
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
  draining: boolean;
  /** Live reachability from the shared health cache (re-probed ≤ once/min). */
  online: boolean;
  health_error?: string;
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

// The models.visibility column is CHECK-constrained to exactly these two
// values (migration 0010); the union is derived FROM the array so a future
// third value is a single-point change.
export const VISIBILITIES = ['public', 'internal'] as const;
export type Visibility = (typeof VISIBILITIES)[number];

/** Catalog row kind on GET /api/models: a plain model row or a model group.
 *  Derived from the array — same single-point convention as Visibility.
 *  (No runtime consumer: the const exists only to derive the union.) */
const CATALOG_KINDS = ['model', 'group'] as const;
export type CatalogKind = (typeof CATALOG_KINDS)[number];

/** Body of PATCH /api/admin/models/{id}: every field is optional (partial
 *  merge). Exported so callers and the api method share one shape. */
export type ModelPatch = Partial<{
  name: string;
  enabled: boolean;
  visibility: Visibility;
  price_in: number;
  price_out: number;
  price_cache_read: number;
  price_cache_write: number;
  context_length: number;
  concurrency_limit: number;
  busy_threshold_pct: number;
  bench_tok_per_s: number;
  bench_pp_tok_per_s: number;
  fed_visibility: FedVisibility;
  /** Replaces the allowlist wholesale; only accepted together with
   *  fed_visibility 'allowlist' (server rule). */
  fed_peer_ids: number[];
}>;

export interface CatalogModel {
  id: number;
  name: string;
  backend_id: number;
  backend_name: string;
  upstream_model: string;
  /** 'public' | 'internal' — internal members are requestable but hidden
   *  from /v1/models (group members are set internal by the store). */
  visibility: Visibility;
  enabled: boolean;
  price_in: number;
  price_out: number;
  price_cache_read: number;
  price_cache_write: number;
  context_length: number;
  concurrency_limit: number;
  busy_threshold_pct: number;
  bench_tok_per_s: number;
  bench_pp_tok_per_s: number;
  bench_at: string;
  /** Federation sharing switch ('off' | 'all' | 'allowlist'); fed_peer_ids
   *  is the allowlist side (peer ids, empty unless fed_visibility is
   *  'allowlist'). */
  fed_visibility: FedVisibility;
  fed_peer_ids: number[];
}

/** Result of POST /api/admin/models/{id}/benchmark. The prefill pass can
 * fail without voiding the decode result: then pp_* are absent and
 * pp_error says why (the stored PP value keeps its previous measurement). */
export interface BenchmarkResult {
  bench_tok_per_s: number;
  completion_tokens: number;
  seconds: number;
  bench_pp_tok_per_s?: number;
  prompt_tokens?: number;
  pp_seconds?: number;
  pp_error?: string;
  bench_at: string;
}

/** Model group as returned by the admin group endpoints: the row plus its
 *  members flattened with the display fields the editor needs. These types
 *  mirror the server response shape, not a render list — fields the UI
 *  never reads (member_count, upstream_model, visibility) stay so the
 *  decoder matches the wire. */
export interface GroupMember {
  model_id: number;
  weight: number;
  position: number;
  model_name: string;
  backend_name: string;
  upstream_model: string;
  visibility: Visibility;
  model_enabled: boolean;
  backend_enabled: boolean;
  backend_draining: boolean;
}

export interface ModelGroup {
  id: number;
  name: string;
  enabled: boolean;
  member_count: number;
  members: GroupMember[];
}

/** One member in a group PATCH: the full member list replaces the stored
 *  one. Omitted weight (or ≤0) seeds from the model's bench_tok_per_s —
 *  the UI leaves the field out for a blank input. */
export interface GroupMemberInput {
  model_id: number;
  weight?: number;
  position: number;
}

/** Body of PATCH /api/admin/groups/{id}: every field is optional (partial
 *  merge; `members` replaces the whole member list). Exported so callers
 *  and the api method share one shape (same convention as ModelPatch). */
export type GroupPatch = Partial<{
  name: string;
  enabled: boolean;
  members: GroupMemberInput[];
}>;

/** Model as exposed to all users on GET /api/models (no internal wiring). */
export interface PublicModel {
  /** Always set by the server; group rows carry no per-member fields (all zero). */
  kind: CatalogKind;
  name: string;
  price_in: number;
  price_out: number;
  price_cache_read: number;
  price_cache_write: number;
  context_length: number;
  concurrency_limit: number;
  bench_tok_per_s: number;
  bench_pp_tok_per_s: number;
  bench_at: string;
}

export interface Settings {
  currency: string;
}

// ---------- federation ----------

/** Instance role; mirrors store.FedRole*. */
export const FED_ROLES = ['standalone', 'hub', 'spoke'] as const;
export type FedRole = (typeof FED_ROLES)[number];

/** GET/PUT /api/admin/federation. hub_token is echoed back by the server
 *  (it is the hub's token the admin pasted, not a secret this server minted). */
export interface FedSettings {
  role: FedRole;
  hub_url: string;
  hub_token: string;
  /** Spoke role only: the live channel state from the dialer's point of
   *  view, with the hub's hello_ack identity (empty while disconnected). */
  spoke?: { connected: boolean; hub_name: string; hub_version: string };
}

/** One peer row on both list surfaces (self-service + admin). `online` is
 *  only ever true on the admin surface (the hub stamps it from its live
 *  registry); the self-service list leaves it false. */
export interface FedPeer {
  id: number;
  name: string;
  owner_user_id: number;
  owner_name: string;
  enabled: boolean;
  last_seen: string;
  version: string;
  created_at: string;
  /** Models this peer may see: every fed_visibility 'all' model + its
   *  allowlist rows (computed server-side at list time). */
  shared_models: number;
  online: boolean;
}

/** POST /api/peers — the raw fed- token appears exactly here (and in the
 *  rotate responses), same shown-once contract as API keys. */
export interface CreatedPeer {
  peer: FedPeer;
  token: string;
}

/** One served/consumed bucket; same six counters as UsageTotals. */
export interface FedCounts {
  requests: number;
  prompt_tokens: number;
  completion_tokens: number;
  cached_tokens: number;
  cache_write_tokens: number;
  cost_micro: number;
}

/** Hub ledger row per user (keyed by user_id: deleted users share the
 *  '(deleted user)' label but stay distinct entries). */
export interface FedUserTotals {
  user_id: number;
  user: string;
  served: FedCounts;
  consumed: FedCounts;
}

/** Hub ledger row per peer. served sums the rows naming it as host;
 *  consumed is OWNER-LEVEL (every consumed row of the peer's owner — an
 *  owner with several peers carries the same total under each), so it must
 *  never be summed across rows. */
export interface FedPeerTotals {
  peer_id: number;
  peer: string;
  served: FedCounts;
  consumed: FedCounts;
}

/** Spoke: what this box served to one hub-stamped identity. */
export interface FedSourceEntry {
  fed_user: string;
  counts: FedCounts;
}

/** Spoke: what one local user spent on federated models. */
export interface FedConsumedEntry {
  user_id: number;
  user: string;
  counts: FedCounts;
}

export type FedAdminStats =
  | { role: 'hub'; since: string; users: FedUserTotals[]; peers: FedPeerTotals[] }
  | { role: 'spoke'; since: string; served_by_source: FedSourceEntry[]; consumed: FedConsumedEntry[] }
  | { role: 'standalone' };

export type FedMyStats =
  | { role: 'hub'; since: string; totals: FedUserTotals[] }
  | { role: 'spoke'; since: string; consumed: FedConsumedEntry[] }
  | { role: 'standalone' };

/** Per-model federation sharing switch; mirrors the store CHECK: 'off'
 *  (default) | 'all' | 'allowlist' (fed_peer_ids names this instance's
 *  peers; on a spoke allowlist is effectively off — a spoke shares only
 *  'all', it has no peer table of its own). */
export const FED_VISIBILITIES = ['off', 'all', 'allowlist'] as const;
export type FedVisibility = (typeof FED_VISIBILITIES)[number];

/** Canonical synthetic-backend predicate: mirrors the store's SQL rule
 *  (`syntheticWhere` in internal/store/fedmaterialize.go) — the spoke's
 *  hub:// backends, or a peer://<id> backend on the hub. The SQL is the
 *  broader `LIKE 'peer://%'`; the digit-bound form here covers every row
 *  federation actually writes (`PeerBackendURL` is `peer://` + digits).
 *  Keep the two in lockstep. */
export function isSyntheticBackendUrl(baseUrl: string): boolean {
  return /^peer:\/\/\d+$/.test(baseUrl) || baseUrl === 'hub://federation' || baseUrl === 'hub://';
}

/** One minute of the live dashboard window (GET /api/live). */
export interface LiveBucket {
  minute: string;
  requests: number;
  prompt_tokens: number;
  completion_tokens: number;
  cached_tokens: number;
  cache_write_tokens: number;
}

export interface LiveModel {
  /** Physical model row id: group members share a name, so rows key on id.
   *  On the personal route a group is one pool row with a synthetic
   *  id = -groupId (never collides with a real row id). */
  id: number;
  name: string;
  /** Empty on synthetic pool rows (member wiring is admin-only). */
  backend: string;
  /** Engine-side model id — admin responses only, absent for users. */
  upstream?: string;
  online: boolean;
  draining: boolean;
  active: number;
  /** True on synthetic pool rows (explicit marker; do not re-derive it
   *  from the negative id or the empty backend). Mirrors the wire. */
  pool?: boolean;
  /** Per-member limit for admins; summed pool limit on pool rows
   *  (0 = unknown/auto-scale). Mirrors the wire — the dashboard axis
   *  scales off LiveUtil.concurrency_limit, not this field. */
  concurrency_limit: number;
}

export interface LiveUtil {
  model: string;
  /** Busy % of each minute (aligned with buckets); >100 = parallel streams. */
  values: number[];
  /** Catalog concurrency limit; 0 = unset (frontend auto-scales the axis). */
  concurrency_limit: number;
}

/** One proxied request in the admin connections panel: running now, or
 *  finished within the 60 s memory window (running=false). */
export interface LiveConnection {
  user: string;
  key: string;
  model: string;
  endpoint: string;
  session?: string;
  ip?: string;
  streaming: boolean;
  running: boolean;
  started_at: string;
  ended_at?: string;
  seconds: number;
}

/** One user's most recent completed request within 24 h (admin route only). */
export interface LiveLastUsed {
  user: string;
  last_used: string;
}

export interface LiveResponse {
  buckets: LiveBucket[];
  active: { model: string; count: number }[];
  models: LiveModel[];
  util: LiveUtil[];
  /** Admin route only: who is on which model right now. */
  connections?: LiveConnection[];
  /** Admin route only: per-user most recent usage within 24 h. */
  last_used?: LiveLastUsed[];
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

  // live dashboard
  live: (admin: boolean, window?: number) =>
    request<LiveResponse>('GET', admin ? '/api/admin/live' : '/api/live', {
      query: { window },
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
    patch: { name?: string; base_url?: string; api_key?: string; enabled?: boolean; draining?: boolean },
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
  patchModel: (id: number, patch: ModelPatch) =>
    request<CatalogModel>('PATCH', `/api/admin/models/${id}`, { body: patch }),
  deleteModel: (id: number) => request<void>('DELETE', `/api/admin/models/${id}`),
  /** Runs a timed generation upstream; can take minutes (409 if one is running). */
  benchmarkModel: (id: number) =>
    request<BenchmarkResult>('POST', `/api/admin/models/${id}/benchmark`),

  // admin — model groups
  groups: () => request<{ groups: ModelGroup[] }>('GET', '/api/admin/groups'),
  createGroup: (name: string) => request<ModelGroup>('POST', '/api/admin/groups', { body: { name } }),
  patchGroup: (id: number, patch: GroupPatch) =>
    request<ModelGroup>('PATCH', `/api/admin/groups/${id}`, { body: patch }),
  deleteGroup: (id: number) => request<void>('DELETE', `/api/admin/groups/${id}`),

  // admin — settings
  settings: () => request<Settings>('GET', '/api/admin/settings'),
  putSettings: (currency: string) =>
    request<Settings>('PUT', '/api/admin/settings', { body: { currency } }),

  // federation — role + stats (session auth, any user)
  fedRole: () => request<{ role: FedRole }>('GET', '/api/federation/role'),
  fedMyStats: (days: number) =>
    request<FedMyStats>('GET', '/api/federation/stats', { query: { days } }),

  // federation — own servers (the My Keys surface; token shown once)
  myPeers: () => request<{ peers: FedPeer[] }>('GET', '/api/peers'),
  createPeer: (name: string) => request<CreatedPeer>('POST', '/api/peers', { body: { name } }),
  rotateMyPeer: (id: number) => request<{ token: string }>('POST', `/api/peers/${id}/rotate`),
  deleteMyPeer: (id: number) => request<void>('DELETE', `/api/peers/${id}`),

  // federation — admin: settings pair, peer registry, ledger
  fedSettings: () => request<FedSettings>('GET', '/api/admin/federation'),
  putFedSettings: (role: FedRole, hub_url: string, hub_token: string) =>
    request<FedSettings>('PUT', '/api/admin/federation', { body: { role, hub_url, hub_token } }),
  adminPeers: () => request<{ peers: FedPeer[] }>('GET', '/api/admin/peers'),
  adminRotatePeer: (id: number) => request<{ token: string }>('POST', `/api/admin/peers/${id}/rotate`),
  adminPatchPeer: (id: number, enabled: boolean) =>
    request<FedPeer>('PATCH', `/api/admin/peers/${id}`, { body: { enabled } }),
  adminDeletePeer: (id: number) => request<void>('DELETE', `/api/admin/peers/${id}`),
  fedAdminStats: (days: number) =>
    request<FedAdminStats>('GET', '/api/admin/federation/stats', { query: { days } }),
};
