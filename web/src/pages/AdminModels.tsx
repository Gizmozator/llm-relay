import { Fragment, useEffect, useMemo, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { api, errMsg, FED_VISIBILITIES, isSyntheticBackendUrl, VISIBILITIES } from '../api';
import type { CatalogModel, FedVisibility, GroupPatch, ModelGroup, ModelPatch, Visibility } from '../api';
import { fmtDateTime, microToPerM } from '../format';
import { useLoad } from '../useLoad';
import { useToast } from '../components/Toast';
import {
  Banner,
  ConfirmButton,
  EditableNumber,
  EditableText,
  Empty,
  Field,
  Loading,
  Modal,
  parseNumInput,
  Toggle,
} from '../components/ui';

const PRICE_FIELDS = [
  { key: 'price_in', label: 'Input' },
  { key: 'price_out', label: 'Output' },
  { key: 'price_cache_read', label: 'Cache read' },
  { key: 'price_cache_write', label: 'Cache write' },
] as const;

// Display labels for the Share dropdown. The stored/wire value stays 'off'
// (the store CHECK-constrains fed_visibility to off|all|allowlist), but the
// UI shows 'local' so the option reads as a sharing scope (local / allowlist
// / all) instead of an on/off toggle. Display only — the <option value> and
// every PATCH payload still carry the raw enum.
const FED_VISIBILITY_LABELS: Record<FedVisibility, string> = {
  off: 'local',
  all: 'all',
  allowlist: 'allowlist',
};

export default function AdminModelsPage() {
  const toast = useToast();
  const models = useLoad(() => api.models().then((r) => r.models), []);
  const settings = useLoad(() => api.settings(), []);
  const currency = settings.data?.currency ?? '';
  // Local copy of the rows: a field commit merges the PATCH response here
  // instead of a full reload, which would clobber mid-edit values in the
  // other inputs of the table. Re-synced whenever a fresh list arrives.
  const [rows, setRows] = useState<CatalogModel[] | null>(null);
  const [rowsBase, setRowsBase] = useState<CatalogModel[] | null>(null);
  if (models.data !== rowsBase) {
    setRowsBase(models.data);
    setRows(models.data);
  }
  // Id of the model whose benchmark is in flight; the server serializes
  // benchmarks, so while one runs the others would only get a 409.
  const [benching, setBenching] = useState<number | null>(null);
  // Model whose price calculator dialog is open.
  const [calcFor, setCalcFor] = useState<CatalogModel | null>(null);
  // Model groups: the list drives the groups section AND the per-row group
  // badge (a model belongs to at most one group — the store rejects a
  // second membership with 409).
  const groups = useLoad(() => api.groups().then((r) => r.groups), []);
  // Synthetic (federation-materialized) backends: base_url peer://… or
  // hub://…. Their model rows mirror another instance's catalog, so every
  // field is read-only here; the kill switches are the peer/backend disable
  // in the Servers section.
  const backends = useLoad(() => api.backends().then((r) => r.backends), []);
  const fedRole = useLoad(() => api.fedRole(), []);
  const peers = useLoad(() => api.adminPeers().then((r) => r.peers), []);
  const syntheticBackends = useMemo(
    () => (backends.data ?? []).filter((b) => isSyntheticBackendUrl(b.base_url)),
    [backends.data],
  );
  const syntheticBackendIds = useMemo(
    () => new Set(syntheticBackends.map((b) => b.id)),
    [syntheticBackends],
  );
  const syntheticBackendNames = useMemo(
    () => new Set(syntheticBackends.map((b) => b.name)),
    [syntheticBackends],
  );
  // FAIL CLOSED: until the backend list is in (or once it has failed), every
  // row is treated as synthetic-locked — rendering editable controls on a
  // possibly-materialized row is worse than locking a local one for a beat.
  const isSynthetic = (m: CatalogModel) => backends.data === null || syntheticBackendIds.has(m.backend_id);
  // On a spoke an allowlist shares nothing (a spoke has no peer table of its
  // own — the catalog rule), so the option needs an honest tooltip there.
  const fedShareTitle =
    fedRole.data?.role === 'spoke'
      ? 'Federation sharing: local = not shared with the federation, all = shared with the hub, allowlist = effectively local on a spoke (only “all” is pushed)'
      : 'Federation sharing: local = not shared with the federation, all = every peer, allowlist = the selected peers only';
  const [showGroupAdd, setShowGroupAdd] = useState(false);
  // Id of the group whose member editor is open; the group itself is looked
  // up from the fresh list so a reload never edits a stale copy.
  const [editGroupId, setEditGroupId] = useState<number | null>(null);
  const groupOf = useMemo(() => {
    const map = new Map<number, string>();
    for (const g of groups.data ?? []) {
      for (const mem of g.members) map.set(mem.model_id, g.name);
    }
    return map;
  }, [groups.data]);
  const editGroup =
    editGroupId === null
      ? null
      : (groups.data ?? []).find((g) => g.id === editGroupId) ?? null;
  // Models claimed by another group: one group per model, so the member
  // picker must not offer these.
  const takenModelIds = useMemo(() => {
    const set = new Set<number>();
    for (const g of groups.data ?? []) {
      if (g.id === editGroupId) continue;
      for (const mem of g.members) set.add(mem.model_id);
    }
    return set;
  }, [groups.data, editGroupId]);

  const patch = async (id: number, p: ModelPatch): Promise<boolean> => {
    try {
      const updated = await api.patchModel(id, p);
      setRows((prev) => (prev ?? []).map((m) => (m.id === id ? updated : m)));
      // A rename or an enabled flip must also refresh the groups payload:
      // the group chips and the member editor render model names and
      // model_enabled state from its member rows.
      if (p.name !== undefined || p.enabled !== undefined) groups.reload();
      return true;
    } catch (e: unknown) {
      toast.push('error', errMsg(e));
      models.reload();
      return false;
    }
  };

  // Timed generation + prefill upstream; the request can take minutes.
  const benchmark = async (m: CatalogModel) => {
    setBenching(m.id);
    try {
      const r = await api.benchmarkModel(m.id);
      if (r.bench_pp_tok_per_s) {
        toast.push(
          'success',
          `Benchmark: TG ${r.bench_tok_per_s.toFixed(1)} · PP ${r.bench_pp_tok_per_s.toFixed(0)} tok/s`,
        );
      } else {
        toast.push(
          'error',
          `Benchmark: TG ${r.bench_tok_per_s.toFixed(1)} tok/s (PP failed: ${r.pp_error ?? 'unknown'})`,
        );
      }
      models.reload();
    } catch (e: unknown) {
      toast.push('error', errMsg(e));
    } finally {
      setBenching(null);
    }
  };

  const remove = async (m: CatalogModel) => {
    try {
      await api.deleteModel(m.id);
      toast.push('success', `Deleted “${m.name}”.`);
      models.reload();
      // Deleting a member row silently drops its group membership
      // server-side: the badges and the member chips must follow.
      groups.reload();
    } catch (e: unknown) {
      toast.push('error', errMsg(e));
    }
  };

  // Group membership flips member visibility (join → internal, leave →
  // public), so membership changes reload the model table too; name/enable
  // edits touch only the group row.
  const patchGroup = async (g: ModelGroup, p: GroupPatch): Promise<boolean> => {
    try {
      await api.patchGroup(g.id, p);
      // Flat message choice (no nested ternary): the toast must be honest
      // about whichever part of the GroupPatch was actually sent.
      let msg: string;
      if (p.name !== undefined) msg = `Group renamed to “${p.name}”.`;
      else if (p.members !== undefined) msg = `Group “${g.name}” members updated.`;
      else msg = `Group “${g.name}” ${p.enabled ? 'enabled' : 'disabled'}.`;
      toast.push('success', msg);
      return true;
    } catch (e: unknown) {
      // e.g. enabling an empty group → 409; the reload reverts the toggle.
      toast.push('error', errMsg(e));
      return false;
    } finally {
      groups.reload();
    }
  };

  const removeGroup = async (g: ModelGroup) => {
    try {
      await api.deleteGroup(g.id);
      toast.push('success', `Deleted group “${g.name}”.`);
    } catch (e: unknown) {
      // The group survives a failed delete: reloading would clobber mid-edit
      // table values for nothing, and closing the editor would lie about it.
      toast.push('error', errMsg(e));
      return;
    }
    // Deleting a group releases its members (visibility flips back), so the
    // model table follows as well.
    groups.reload();
    models.reload();
    // If the members editor was open on this group, close it: editGroup
    // would resolve to null after the reload and the modal would silently
    // vanish instead.
    if (editGroupId === g.id) setEditGroupId(null);
  };

  return (
    <div className="page">
      <div className="page-head">
        <h2>Model catalog</h2>
      </div>

      {/* Auxiliary loads fail visibly, and each degrades honestly: no
          backend list → every row stays synthetic-locked; no role → Share
          controls hidden; no peers → the allowlist picker shows the error
          instead of an empty list. */}
      {backends.error && (
        <Banner>Backend list unavailable — model rows stay locked (federation state unknown): {backends.error}</Banner>
      )}
      {fedRole.error && (
        <Banner>Federation role unavailable — Share controls hidden: {fedRole.error}</Banner>
      )}
      {peers.error && (
        <Banner>Peer list unavailable — allowlist editing disabled: {peers.error}</Banner>
      )}

      {models.loading && !models.data ? (
        <Loading />
      ) : models.error ? (
        <Banner>{models.error}</Banner>
      ) : !rows || rows.length === 0 ? (
        <section className="panel">
          <Empty label="No models in the catalog. Add a backend and sync its models." />
        </section>
      ) : (
        <section className="panel">
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>Public name</th>
                  <th>Backend</th>
                  <th>Upstream id</th>
                  <th title="public: listed on /v1/models; internal: hidden but still requestable (group members are internal)">
                    Visibility
                  </th>
                  <th>Enabled</th>
                  <th className="num" title="Parallel streams the engine serves at once (0 = unset)">
                    Parallel
                  </th>
                  <th className="num" title="Mean busy % at which agents see “busy” (0 = default 80; over 100 = several parallel streams)">
                    Busy ≥ %
                  </th>
                  <th className="actions">Actions</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((m) => (
                  <Fragment key={m.id}>
                    <tr className="model-main">
                      <td>
                        {isSynthetic(m) ? (
                          <span className="mono">{m.name}</span>
                        ) : (
                          <EditableText
                            value={m.name}
                            ariaLabel={`Public name of ${m.name}`}
                            onCommit={(v) => patch(m.id, { name: v })}
                          />
                        )}
                        {groupOf.has(m.id) && (
                          <span className="tag" title={`Member of group “${groupOf.get(m.id)}”`}>
                            group:{groupOf.get(m.id)}
                          </span>
                        )}
                        {isSynthetic(m) && (
                          <span
                            className="badge fed"
                            title="Materialized from a federation peer — this row mirrors the peer’s catalog"
                          >
                            federation
                          </span>
                        )}
                      </td>
                      <td>{m.backend_name}</td>
                      <td className="mono">{m.upstream_model}</td>
                      <td>
                        {isSynthetic(m) ? (
                          <span title="Set by the peer’s catalog">{m.visibility}</span>
                        ) : (
                          <select
                            className="input input-inline"
                            value={m.visibility}
                            aria-label={`Visibility of ${m.name}`}
                            title={
                              groupOf.has(m.id)
                                ? 'Group member: re-exposing it is allowed, but any later membership edit of the group forces members back to internal — a manual “public” here is transient'
                                : undefined
                            }
                            onChange={(e) => patch(m.id, { visibility: e.target.value as Visibility })}
                          >
                            {VISIBILITIES.map((v) => (
                              <option key={v} value={v}>
                                {v}
                              </option>
                            ))}
                          </select>
                        )}
                      </td>
                      <td>
                        {isSynthetic(m) ? (
                          <span title="Managed by the peer — to stop serving this model, disable the peer in the Servers section">
                            {m.enabled ? 'on' : 'off'}
                          </span>
                        ) : (
                          <Toggle checked={m.enabled} onChange={(v) => patch(m.id, { enabled: v })} />
                        )}
                      </td>
                      <td className="num">
                        {isSynthetic(m) ? (
                          String(m.concurrency_limit)
                        ) : (
                          <EditableNumber
                            display={String(m.concurrency_limit)}
                            ariaLabel={`Concurrency limit of ${m.name}`}
                            onCommit={(n) => patch(m.id, { concurrency_limit: Math.round(n) })}
                          />
                        )}
                      </td>
                      <td className="num">
                        {isSynthetic(m) ? (
                          String(m.busy_threshold_pct)
                        ) : (
                          <EditableNumber
                            display={String(m.busy_threshold_pct)}
                            ariaLabel={`Busy threshold (percent) of ${m.name}`}
                            max={10000}
                            onCommit={(n) => patch(m.id, { busy_threshold_pct: Math.round(n) })}
                          />
                        )}
                      </td>
                      <td className="actions">
                        {!isSynthetic(m) && (
                          <>
                            <button className="btn small" onClick={() => setCalcFor(m)}>
                              Calc prices
                            </button>
                            <button
                              className="btn small"
                              disabled={benching !== null}
                              onClick={() => benchmark(m)}
                            >
                              {benching === m.id ? 'Benching…' : 'Benchmark'}
                            </button>
                            <ConfirmButton label="Delete" onConfirm={() => remove(m)} />
                          </>
                        )}
                      </td>
                    </tr>
                    <tr className="sub-row">
                      <td colSpan={8}>
                        <div className="model-metrics">
                          {PRICE_FIELDS.map((p) => (
                            <span className="metric" key={p.key}>
                              <span className="metric-label">
                                {p.label} <span className="dim">{currency ? `${currency}/1M` : 'per 1M'}</span>
                              </span>
                              {isSynthetic(m) ? (
                                <span className="mono">{microToPerM(m[p.key])}</span>
                              ) : (
                                <EditableNumber
                                  display={microToPerM(m[p.key])}
                                  ariaLabel={`${p.label} price of ${m.name}`}
                                  onCommit={(n) => patch(m.id, { [p.key]: Math.round(n * 1e6) } as ModelPatch)}
                                />
                              )}
                            </span>
                          ))}
                          <span className="metric">
                            <span className="metric-label">Context</span>
                            {isSynthetic(m) ? (
                              <span className="mono">{m.context_length}</span>
                            ) : (
                              <EditableNumber
                                display={String(m.context_length)}
                                ariaLabel={`Context length of ${m.name}`}
                                onCommit={(n) => patch(m.id, { context_length: Math.round(n) })}
                              />
                            )}
                          </span>
                          <span
                            className="metric"
                            title={m.bench_at ? `Benchmarked ${fmtDateTime(m.bench_at)}` : 'Manual value'}
                          >
                            <span className="metric-label">TG tok/s</span>
                            {isSynthetic(m) ? (
                              <span className="mono">{m.bench_tok_per_s ? m.bench_tok_per_s.toFixed(1) : '0'}</span>
                            ) : (
                              <EditableNumber
                                display={m.bench_tok_per_s ? m.bench_tok_per_s.toFixed(1) : '0'}
                                ariaLabel={`Generation speed (tok/s) of ${m.name}`}
                                onCommit={(n) => patch(m.id, { bench_tok_per_s: n })}
                              />
                            )}
                          </span>
                          <span
                            className="metric"
                            title={m.bench_at ? `Benchmarked ${fmtDateTime(m.bench_at)}` : 'Manual value'}
                          >
                            <span className="metric-label">PP tok/s</span>
                            {isSynthetic(m) ? (
                              <span className="mono">{m.bench_pp_tok_per_s ? m.bench_pp_tok_per_s.toFixed(0) : '0'}</span>
                            ) : (
                              <EditableNumber
                                display={m.bench_pp_tok_per_s ? m.bench_pp_tok_per_s.toFixed(0) : '0'}
                                ariaLabel={`Prompt-processing speed (tok/s) of ${m.name}`}
                                onCommit={(n) => patch(m.id, { bench_pp_tok_per_s: n })}
                              />
                            )}
                          </span>
                          {/* Share controls: hidden on synthetic rows (the
                              server ignores fed_visibility there — the
                              materialization scheme decides sharing) and
                              while the role is unknown (degrade quietly). */}
                          {fedRole.data && !isSynthetic(m) && (
                            <span className="metric" title={fedShareTitle}>
                              <span className="metric-label">Share</span>
                              <select
                                className="input input-inline"
                                value={m.fed_visibility}
                                aria-label={`Federation sharing of ${m.name}`}
                                onChange={(e) => patch(m.id, { fed_visibility: e.target.value as FedVisibility })}
                              >
                                {FED_VISIBILITIES.map((v) => (
                                  <option key={v} value={v}>
                                    {FED_VISIBILITY_LABELS[v]}
                                  </option>
                                ))}
                              </select>
                            </span>
                          )}
                          {!isSynthetic(m) && m.fed_visibility === 'allowlist' && (
                            <span className="metric" title="Ctrl/⌘-click to select the peers this model is shared with">
                              <span className="metric-label">Peers</span>
                              {peers.error ? (
                                <span className="tag err" title={peers.error}>peers unavailable</span>
                              ) : (peers.data ?? []).length === 0 ? (
                                <span className="dim">no peers enrolled</span>
                              ) : (
                                <select
                                  className="input fed-peer-select"
                                  multiple
                                  size={3}
                                  aria-label={`Peer allowlist of ${m.name}`}
                                  value={m.fed_peer_ids.map(String)}
                                  onChange={(e) => {
                                    const ids = Array.from(e.target.selectedOptions).map((o) => Number(o.value));
                                    void patch(m.id, { fed_visibility: 'allowlist', fed_peer_ids: ids });
                                  }}
                                >
                                  {(peers.data ?? []).map((p) => (
                                    <option key={p.id} value={String(p.id)}>
                                      {p.name} ({p.owner_name})
                                    </option>
                                  ))}
                                </select>
                              )}
                            </span>
                          )}
                        </div>
                      </td>
                    </tr>
                  </Fragment>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      )}

      <section className="panel">
        <div className="panel-head">
          <h3>Model groups</h3>
          <button className="btn small primary" onClick={() => setShowGroupAdd(true)}>
            + New group
          </button>
        </div>
        {groups.loading && !groups.data ? (
          <Loading />
        ) : groups.data && groups.data.length > 0 ? (
          <>
            {/* A background refetch failure (groups.reload also fires from
                unrelated paths: model rename/enable patches, group delete)
                must not wipe a good table — the error rides as a banner
                above the rows; only the no-data case replaces the panel. */}
            {groups.error && <Banner>{groups.error}</Banner>}
            <div className="table-scroll">
              <table>
                <thead>
                  <tr>
                    <th>Group</th>
                    <th>Enabled</th>
                    <th>Members</th>
                    <th className="actions">Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {groups.data.map((g) => (
                    <tr key={g.id}>
                      <td>
                        <EditableText
                          value={g.name}
                          mono
                          ariaLabel={`Name of group ${g.name}`}
                          onCommit={(v) => patchGroup(g, { name: v })}
                        />
                      </td>
                      <td>
                        <span title="Disabled groups are not advertised on /v1/models; enabling needs ≥1 member">
                          <Toggle
                            checked={g.enabled}
                            ariaLabel={`Enable group ${g.name}`}
                            // A memberless group cannot be enabled (the store 409s) —
                            // spare the round-trip. Disabling is always allowed.
                            disabled={!g.enabled && g.members.length === 0}
                            onChange={(v) => patchGroup(g, { enabled: v })}
                          />
                        </span>
                      </td>
                      <td>
                        {g.members.length === 0 ? (
                          <span className="dim">no members</span>
                        ) : (
                          g.members.map((mem) => {
                            const tags = memberHealthTags(mem);
                            const loc = syntheticBackendNames.has(mem.backend_name)
                              ? `peer: ${mem.backend_name}`
                              : `@ ${mem.backend_name}`;
                            return (
                              <span
                                className="tag"
                                key={mem.model_id}
                                title={`${mem.model_name} ${loc} · weight ${fmtWeight(mem.weight)} · position ${mem.position}`}
                              >
                                {mem.model_name} {loc} · {fmtWeight(mem.weight)}
                                {tags && ` (${tags})`}
                              </span>
                            );
                          })
                        )}
                      </td>
                      <td className="actions">
                        <button className="btn small" onClick={() => setEditGroupId(g.id)}>
                          Members
                        </button>
                        <ConfirmButton label="Delete" onConfirm={() => removeGroup(g)} />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </>
        ) : groups.error ? (
          <Banner>{groups.error}</Banner>
        ) : (
          <Empty label="No model groups. A group pools several model rows under one public name; sessions spread across members and stay pinned to one." />
        )}
      </section>

      {showGroupAdd && (
        <GroupCreateModal
          onClose={() => setShowGroupAdd(false)}
          onCreated={(name) => {
            toast.push('success', `Group “${name}” created — add members first, then flip its Enabled toggle.`);
            setShowGroupAdd(false);
            groups.reload();
          }}
        />
      )}

      {editGroup && (
        <GroupMembersModal
          group={editGroup}
          models={rows ?? []}
          taken={takenModelIds}
          synthetic={syntheticBackendNames}
          onClose={() => setEditGroupId(null)}
          onSaved={() => {
            toast.push('success', `Members updated for “${editGroup.name}”.`);
            setEditGroupId(null);
            // Membership flips member visibility, so both tables follow.
            groups.reload();
            models.reload();
          }}
        />
      )}

      {calcFor && (
        <PriceCalcModal
          model={calcFor}
          currency={currency}
          onClose={() => setCalcFor(null)}
          onApply={async (prices) => {
            if (await patch(calcFor.id, prices)) {
              toast.push('success', `Prices updated for “${calcFor.name}”.`);
              setCalcFor(null);
            }
          }}
        />
      )}
    </div>
  );
}

// ---------- price calculator ----------
//
// Cost-recovery pricing from electricity: energy per token is W / tok/s
// joules, 1 kWh is 3.6 MJ, so the cost of 1M tokens in micro-currency is
// elec × W × 1e6 / (3.6 × tok/s). Output tokens are priced from the
// generation pair (power, tok/s); input and cache-write from the prompt-
// processing pair (writing cache is prefilling new content); cache read is
// 0 — cached tokens skip prefill. Values round up to whole cents per 1M.

type PriceSet = {
  price_in: number;
  price_out: number;
  price_cache_read: number;
  price_cache_write: number;
};

interface CalcInputs {
  tgW: string;
  tgTps: string;
  ppW: string;
  ppTps: string;
  elec: string;
}

const CALC_LS_KEY = 'llmrel…calc';
const CALC_DEFAULTS: CalcInputs = { tgW: '', tgTps: '', ppW: '', ppTps: '', elec: '' };
const PRICE_KEYS = ['price_in', 'price_out', 'price_cache_read', 'price_cache_write'] as const;

function loadCalcInputs(): CalcInputs {
  try {
    return { ...CALC_DEFAULTS, ...(JSON.parse(localStorage.getItem(CALC_LS_KEY) ?? '{}') as Partial<CalcInputs>) };
  } catch {
    return CALC_DEFAULTS;
  }
}

const posNum = (s: string): number | null => {
  const v = parseFloat(s);
  return Number.isFinite(v) && v > 0 ? v : null;
};

// Round a micro-unit price up to whole cents per 1M (1 cent = 10 000 µ).
const ceilCent = (micro: number) => Math.ceil(micro / 10_000) * 10_000;
const microPerM = (w: number, tps: number, elec: number) => (elec * w * 1e6) / (3.6 * tps);

function PriceCalcModal({
  model,
  currency,
  onClose,
  onApply,
}: {
  model: CatalogModel;
  currency: string;
  onClose: () => void;
  onApply: (p: PriceSet) => Promise<void>;
}) {
  const stored = useMemo(loadCalcInputs, []);
  const [inp, setInp] = useState<CalcInputs>({
    ...stored,
    tgTps: model.bench_tok_per_s ? String(model.bench_tok_per_s) : stored.tgTps,
    ppTps: model.bench_pp_tok_per_s ? String(model.bench_pp_tok_per_s) : stored.ppTps,
  });
  const set = (k: keyof CalcInputs) => (e: { target: { value: string } }) =>
    setInp((prev) => ({ ...prev, [k]: e.target.value }));

  // Derived prices in micro/1M; null until every input is a positive number.
  const calc = useMemo(() => {
    const tgW = posNum(inp.tgW);
    const tgTps = posNum(inp.tgTps);
    const ppW = posNum(inp.ppW);
    const ppTps = posNum(inp.ppTps);
    const elec = posNum(inp.elec);
    if (!tgW || !tgTps || !ppW || !ppTps || !elec) return null;
    const pp = ceilCent(microPerM(ppW, ppTps, elec));
    return {
      price_in: pp,
      price_out: ceilCent(microPerM(tgW, tgTps, elec)),
      price_cache_read: 0,
      price_cache_write: pp,
    };
  }, [inp]);

  // Editable preview in currency/1M: recomputed whenever the derived prices
  // change, manual edits stick until the next calculator-input change.
  const [prices, setPrices] = useState<Record<(typeof PRICE_KEYS)[number], string>>({
    price_in: '',
    price_out: '',
    price_cache_read: '',
    price_cache_write: '',
  });
  useEffect(() => {
    if (!calc) return;
    setPrices({
      price_in: String(calc.price_in / 1e6),
      price_out: String(calc.price_out / 1e6),
      price_cache_read: '0',
      price_cache_write: String(calc.price_cache_write / 1e6),
    });
  }, [calc]);

  const parsed = PRICE_KEYS.map((k) => parseFloat(prices[k]));
  const pricesValid = parsed.every((v) => Number.isFinite(v) && v >= 0);
  const [busy, setBusy] = useState(false);
  const toast = useToast();

  const apply = async () => {
    setBusy(true);
    try {
      try {
        localStorage.setItem(CALC_LS_KEY, JSON.stringify(inp));
      } catch (e: unknown) {
        // Quota/private-mode storage failure: worth a note, but the price
        // commit below must still run.
        toast.push('error', `Could not remember calculator inputs: ${errMsg(e)}`);
      }
      const [price_in, price_out, price_cache_read, price_cache_write] = parsed.map((v) =>
        Math.round(v * 1e6),
      );
      await onApply({ price_in, price_out, price_cache_read, price_cache_write });
    } catch (e: unknown) {
      // setItem or onApply rejecting must not vanish as an unhandled
      // rejection behind `void apply()` — the operator needs to see it.
      toast.push('error', errMsg(e));
    } finally {
      setBusy(false);
    }
  };

  const cur = currency || 'EUR';
  const numProps = { className: 'input', type: 'number' as const, min: 0 };

  return (
    <Modal title={`Price calculator — ${model.name}`} onClose={onClose}>
      <form
        className="form-stack"
        onSubmit={(e) => {
          e.preventDefault();
          if (calc && pricesValid && !busy) void apply();
        }}
      >
        <Field label="Generation power (W)">
          <input {...numProps} step="1" value={inp.tgW} onChange={set('tgW')} autoFocus />
        </Field>
        <Field label="Generation speed (tok/s)">
          <input {...numProps} step="0.1" value={inp.tgTps} onChange={set('tgTps')} />
        </Field>
        <Field label="Prompt-processing power (W)">
          <input {...numProps} step="1" value={inp.ppW} onChange={set('ppW')} />
        </Field>
        <Field label="Prompt-processing speed (tok/s)">
          <input {...numProps} step="1" value={inp.ppTps} onChange={set('ppTps')} />
        </Field>
        <Field label={`Electricity price (${cur}/kWh)`}>
          <input {...numProps} step="0.001" value={inp.elec} onChange={set('elec')} />
        </Field>
        <p className="dim">
          Cost per 1M tokens = W × {cur}/kWh ÷ (3.6 × tok/s), rounded up to 1 cent. Cache read
          stays 0: cached tokens skip prefill. You can adjust the values below before applying.
        </p>
        <Field label={`Input price (${cur}/1M)`}>
          <input
            {...numProps}
            step="0.000001"
            disabled={!calc}
            value={prices.price_in}
            onChange={(e) => setPrices({ ...prices, price_in: e.target.value })}
          />
        </Field>
        <Field label={`Output price (${cur}/1M)`}>
          <input
            {...numProps}
            step="0.000001"
            disabled={!calc}
            value={prices.price_out}
            onChange={(e) => setPrices({ ...prices, price_out: e.target.value })}
          />
        </Field>
        <Field label={`Cache read price (${cur}/1M)`}>
          <input
            {...numProps}
            step="0.000001"
            disabled={!calc}
            value={prices.price_cache_read}
            onChange={(e) => setPrices({ ...prices, price_cache_read: e.target.value })}
          />
        </Field>
        <Field label={`Cache write price (${cur}/1M)`}>
          <input
            {...numProps}
            step="0.000001"
            disabled={!calc}
            value={prices.price_cache_write}
            onChange={(e) => setPrices({ ...prices, price_cache_write: e.target.value })}
          />
        </Field>
        <div className="modal-actions">
          <button type="button" className="btn" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="btn primary" disabled={!calc || !pricesValid || busy}>
            Apply prices
          </button>
        </div>
      </form>
    </Modal>
  );
}

// ---------- model groups ----------

// Weight display: seeded weights are floats (bench tok/s), so trim to two
// decimals instead of printing 42.50000000000001-style noise.
const fmtWeight = (w: number) => String(Math.round(w * 100) / 100);

// One health wording for every member listing (groups-table chips and the
// member editor); "" when the member is fully servable.
const memberHealthTags = (m: {
  model_enabled: boolean;
  backend_enabled: boolean;
  backend_draining: boolean;
}) =>
  [
    !m.model_enabled && 'model off',
    !m.backend_enabled && 'backend off',
    m.backend_draining && 'draining',
  ]
    .filter(Boolean)
    .join(', ');

// Seed the member-editor draft with the RAW stored weight, not the
// display-rounded fmtWeight: the PATCH is a full replace, so seeding a
// rounded value would silently rewrite a bench-seeded weight (e.g.
// 137.4567 → 137.46) on every unrelated edit. String() is the shortest
// round-tripping form, so an untouched field re-sends the stored value
// verbatim — except in exponent notation (below 1e-6 or ≥ 1e21), which
// the strict parser rejects, so those fall back to fixed notation.
// A weight ≤0 should never be stored (the API seeds it from the
// benchmark at write time); the blank branch is defensive for hand-written
// rows — it drafts such a member as "auto" instead of a literal 0 the
// validator would reject.
const weightSeed = (w: number) =>
  /[eE]/.test(String(w))
    ? w.toLocaleString('en-US', { useGrouping: false, maximumFractionDigits: 20 })
    : String(w);

const seedMembers = (g: ModelGroup) =>
  g.members.map((m) => ({ model_id: m.model_id, weight: m.weight > 0 ? weightSeed(m.weight) : '' }));

function GroupCreateModal({
  onClose,
  onCreated,
}: {
  onClose: () => void;
  onCreated: (name: string) => void;
}) {
  const [name, setName] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (busy) return;
    setBusy(true);
    setError(null);
    try {
      const g = await api.createGroup(name.trim());
      onCreated(g.name);
    } catch (err: unknown) {
      setError(errMsg(err));
      setBusy(false);
    }
  };

  return (
    <Modal title="New model group" onClose={onClose} dismissable={!busy}>
      <form
        className="form-stack"
        onSubmit={(e) => {
          void submit(e);
        }}
      >
        {error && <Banner>{error}</Banner>}
        <Field label="Public name">
          <input
            className="input mono"
            value={name}
            placeholder="qwen3-coder"
            onChange={(e) => setName(e.target.value)}
            autoFocus
          />
        </Field>
        <p className="dim">
          The group owns this public name: it must not collide with a public model or another
          group. New groups start disabled: add members first, then flip the Enabled toggle
          in the groups table — a group without members cannot be enabled.
        </p>
        <div className="modal-actions">
          <button type="button" className="btn" onClick={onClose} disabled={busy}>
            Cancel
          </button>
          <button type="submit" className="btn primary" disabled={busy || !name.trim()}>
            {busy ? 'Creating…' : 'Create group'}
          </button>
        </div>
      </form>
    </Modal>
  );
}

/**
 * Member editor. The group PATCH replaces the whole member list, so the
 * modal edits a local copy and commits once: row order is the member
 * position. Weight contract: blank = the server seeds from the model's
 * bench_tok_per_s; anything else must be a positive number — the input is
 * free text (inputMode decimal) so save()'s validator owns every rule
 * (a type=number field would silently sanitize garbage to blank).
 * Joining flips a model to internal, leaving flips it
 * back (store rule) — members stay directly requestable either way.
 */
function GroupMembersModal({
  group,
  models,
  taken,
  synthetic,
  onClose,
  onSaved,
}: {
  group: ModelGroup;
  models: CatalogModel[];
  taken: Set<number>;
  /** Backend names that belong to federation (synthetic) backends: the
   *  member location reads “peer: <name>” instead of “@ <name>”. */
  synthetic: Set<string>;
  onClose: () => void;
  onSaved: () => void;
}) {
  // Lazy initializer: seedMembers builds the whole draft array; evaluating
  // it on every render (plain argument) allocates work React discards
  // after the first mount.
  const [members, setMembers] = useState<{ model_id: number; weight: string }[]>(
    () => seedMembers(group),
  );
  // Re-seed rules (the PATCH is a full replace, so a draft seeded from a
  // stale copy could commit members that no longer exist):
  //  - a DIFFERENT group id always reseeds (the editor moved to another
  //    group);
  //  - the SAME group arriving as a fresh object from a late-landing
  //    groups.reload() (one fired just BEFORE the modal opened) reseeds
  //    only while the draft is UNTOUCHED. Keyed on the group id plus a
  //    touched flag, not object identity: identity alone would discard an
  //    in-progress member/weight draft on any reload of the same group.
  const touched = useRef(false);
  const seededId = useRef(group.id);
  useEffect(() => {
    if (group.id !== seededId.current) {
      seededId.current = group.id;
      touched.current = false;
    }
    if (!touched.current) setMembers(seedMembers(group));
  }, [group]);
  // Every draft mutation goes through here so the re-seed above knows the
  // operator is mid-edit.
  const mutate = (next: { model_id: number; weight: string }[]) => {
    touched.current = true;
    setMembers(next);
  };
  const [addId, setAddId] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // Two-step arm for the destructive save (see save()): clearing every
  // member commits server-side either way (the store only rejects an empty
  // list when the payload itself flips enabled=true) — an enabled group
  // would serve nothing, a disabled one would publish every departing
  // member — so the UI makes the operator confirm it explicitly.
  const [armedEmpty, setArmedEmpty] = useState(false);

  // Member location: federation members are named by their peer, local
  // ones by their backend.
  const loc = (backendName: string) =>
    synthetic.has(backendName) ? `peer: ${backendName}` : `@ ${backendName}`;
  const label = (id: number) => {
    const mem = group.members.find((m) => m.model_id === id);
    if (mem) return `${mem.model_name} ${loc(mem.backend_name)}`;
    const m = models.find((x) => x.id === id);
    return m ? `${m.name} ${loc(m.backend_name)}` : `model ${id}`;
  };
  // One group per model (store enforces it with 409), so models claimed by
  // another group and models already listed here are not offered.
  const candidates = models.filter(
    (m) => !taken.has(m.id) && !members.some((x) => x.model_id === m.id),
  );

  const add = () => {
    if (!addId) return;
    mutate([...members, { model_id: Number(addId), weight: '' }]);
    setAddId('');
  };

  // Reorder within the draft: the whole row (weight draft included) swaps,
  // so moving never loses an edited weight. Index = the position sent.
  const move = (from: number, to: number) => {
    const next = [...members];
    [next[from], next[to]] = [next[to], next[from]];
    mutate(next);
  };

  // Health hints with the shared wording (memberHealthTags), from the
  // member payload (the model list carries no backend state, so newly
  // added candidates can only show the model-off hint below). Returns
  // the finished tag element (or null) so the wording is evaluated once.
  const healthHint = (id: number) => {
    const mem = group.members.find((m) => m.model_id === id);
    if (!mem) return null;
    const hints = memberHealthTags(mem);
    return hints ? <span className="tag warn">{`(${hints})`}</span> : null;
  };

  // Any draft change disarms the empty-save confirmation (change-based,
  // unlike ConfirmButton's time-based auto-disarm — a timer could disarm
  // while the operator reads the warning).
  useEffect(() => setArmedEmpty(false), [members]);

  // Clearing the member list is the destructive edit the backend does not
  // reject (it only refuses an empty list when the payload flips
  // enabled=true), so it needs an explicit second click either way: an
  // ENABLED group would serve nothing, a disabled one would publish every
  // departing member to the public catalog. Only arming when members are
  // actually leaving: a freshly created (already empty) group saving empty
  // is a no-op, not a destruction.
  const emptyDanger = members.length === 0 && group.members.length > 0;

  const save = async () => {
    // Weight validation: blank means "seed from the benchmark" (field is
    // omitted); anything else must be a positive number, parsed with the
    // SAME strict rule as every numeric editor (no hex/exponent/thousands
    // surprises — Number() would read "0x10" as 16). A typed negative or
    // unparseable value must not silently collapse into the seed.
    for (const m of members) {
      const w = m.weight.trim();
      if (w === '') continue;
      const n = parseNumInput(w);
      if (n === null || n <= 0) {
        setError(
          `Weight for ${label(m.model_id)} must be a positive number (digits, one . or , ` +
            `separator) — leave it blank to seed from the model's benchmark speed.`,
        );
        return;
      }
    }
    if (emptyDanger && !armedEmpty) {
      setArmedEmpty(true);
      return;
    }
    setBusy(true);
    setError(null);
    try {
      await api.patchGroup(group.id, {
        members: members.map((m, i) => ({
          model_id: m.model_id,
          // Parsed with the strict rule (a comma decimal would be NaN via
          // bare Number()); validation above guarantees non-null.
          ...(m.weight.trim() === '' ? {} : { weight: parseNumInput(m.weight.trim()) as number }),
          position: i,
        })),
      });
      onSaved();
    } catch (err: unknown) {
      setError(errMsg(err));
      setBusy(false);
      // A failed destructive save must not leave the confirmation armed:
      // the next click would then commit without the second click.
      setArmedEmpty(false);
    }
  };

  // Flat label computation (no nested ternary): the destructive-action
  // affordance must be scannable.
  let submitLabel = 'Save members';
  if (busy) {
    submitLabel = 'Saving…';
  } else if (emptyDanger && armedEmpty) {
    submitLabel = 'Confirm: remove all members';
  }

  return (
    <Modal title={`Members — ${group.name}`} onClose={onClose} wide dismissable={!busy}>
      <form
        className="form-stack"
        onSubmit={(e) => {
          e.preventDefault();
          if (!busy) void save();
        }}
      >
        {error && <Banner>{error}</Banner>}
        <fieldset disabled={busy} aria-label="Member editor">
          {members.length === 0 ? (
            <Empty label="No members yet — add at least one model row below." />
          ) : (
            <div className="table-scroll">
              <table>
                <thead>
                  <tr>
                    <th>Model</th>
                    <th className="num" title="Relative share of new sessions; leave blank to seed from the model's benchmark speed (a positive number is required otherwise)">
                      Weight
                    </th>
                    <th title="Member order (position): ties in the session-to-member choice break by it">
                      Order
                    </th>
                    <th></th>
                  </tr>
                </thead>
                <tbody>
                  {members.map((mem, i) => (
                    <tr key={mem.model_id}>
                      <td className="mono">
                        {label(mem.model_id)}
                        {healthHint(mem.model_id)}
                      </td>
                      <td className="num">
                        <input
                          className="input input-num"
                          type="text"
                          inputMode="decimal"
                          placeholder="auto"
                          title="Blank = seed from benchmark speed; otherwise a positive number — digits with one . or , as decimal separator (no thousands separators)"
                          value={mem.weight}
                          aria-label={`Weight of ${label(mem.model_id)}`}
                          onChange={(e) =>
                            mutate(members.map((x, j) => (j === i ? { ...x, weight: e.target.value } : x)))
                          }
                        />
                      </td>
                      <td>
                        <button
                          type="button"
                          className="btn small"
                          disabled={i === 0}
                          aria-label={`Move ${label(mem.model_id)} up`}
                          onClick={() => move(i, i - 1)}
                        >
                          ↑
                        </button>
                        <button
                          type="button"
                          className="btn small"
                          disabled={i === members.length - 1}
                          aria-label={`Move ${label(mem.model_id)} down`}
                          onClick={() => move(i, i + 1)}
                        >
                          ↓
                        </button>
                      </td>
                      <td>
                        <button
                          type="button"
                          className="btn small"
                          onClick={() => mutate(members.filter((_, j) => j !== i))}
                        >
                          Remove
                        </button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          {/* Not a Field: a <label> may associate with only one control, and
              this row holds a select AND a button. Same classes, div wrapper. */}
          <div className="field">
            <span className="field-label">Add model</span>
            <div className="copy-row">
              <select className="input" value={addId} onChange={(e) => setAddId(e.target.value)} aria-label="Model to add">
                <option value="">— choose a model —</option>
                {candidates.map((m) => (
                  <option key={m.id} value={m.id}>
                    {m.name} {loc(m.backend_name)}
                    {!m.enabled && ' (model off)'}
                  </option>
                ))}
              </select>
              <button type="button" className="btn" disabled={!addId} onClick={add}>
                Add
              </button>
            </div>
          </div>
        </fieldset>
        <p className="dim">
          Adding a model hides it from the public catalog (it becomes internal); removing it
          makes it public again. Members stay directly requestable either way. Row order is
          the member position — use ↑/↓ to move a member.
        </p>
        {!group.enabled && (
          <p className="dim">This group is disabled — enable it from the groups table after saving.</p>
        )}
        {emptyDanger && armedEmpty && (
          <Banner kind="info">
            {group.enabled
              ? `This removes every member from the ENABLED group “${group.name}”. All of them are published to the public catalog (visibility flips to public) and the group stops pooling: the name keeps resolving to a former member only if one carries it (lowest id, no failover) — otherwise it resolves to nothing until a member is added.`
              : `This removes every member of “${group.name}”. All of them will be published to the public catalog (their visibility flips back to public).`}{' '}
            Click the button again to confirm.
          </Banner>
        )}
        <div className="modal-actions">
          <button type="button" className="btn" onClick={onClose} disabled={busy}>
            Cancel
          </button>
          <button
            type="submit"
            className={emptyDanger && armedEmpty ? 'btn danger' : 'btn primary'}
            disabled={busy}
          >
            {submitLabel}
          </button>
        </div>
      </form>
    </Modal>
  );
}
