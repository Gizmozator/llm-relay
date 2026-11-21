import { Fragment, useState } from 'react';
import { api, errMsg } from '../api';
import type { CatalogModel } from '../api';
import { fmtDateTime, microToPerM } from '../format';
import { useLoad } from '../useLoad';
import { useToast } from '../components/Toast';
import {
  Banner,
  ConfirmButton,
  EditableNumber,
  EditableText,
  Empty,
  Loading,
  Toggle,
} from '../components/ui';

type ModelPatch = {
  name?: string;
  enabled?: boolean;
  price_in?: number;
  price_out?: number;
  price_cache_read?: number;
  price_cache_write?: number;
  context_length?: number;
  concurrency_limit?: number;
  busy_threshold_pct?: number;
  bench_tok_per_s?: number;
};

const PRICE_FIELDS = [
  { key: 'price_in', label: 'Input' },
  { key: 'price_out', label: 'Output' },
  { key: 'price_cache_read', label: 'Cache read' },
  { key: 'price_cache_write', label: 'Cache write' },
] as const;

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

  const patch = async (id: number, p: ModelPatch) => {
    try {
      const updated = await api.patchModel(id, p);
      setRows((prev) => (prev ?? []).map((m) => (m.id === id ? updated : m)));
    } catch (e: unknown) {
      toast.push('error', errMsg(e));
      models.reload();
    }
  };

  // Timed generation upstream; the request can take minutes.
  const benchmark = async (m: CatalogModel) => {
    setBenching(m.id);
    try {
      const r = await api.benchmarkModel(m.id);
      toast.push('success', `Benchmark: ${r.bench_tok_per_s.toFixed(1)} tok/s`);
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
    } catch (e: unknown) {
      toast.push('error', errMsg(e));
    }
  };

  return (
    <div className="page">
      <div className="page-head">
        <h2>Model catalog</h2>
      </div>

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
                  <th>Enabled</th>
                  <th className="actions">Actions</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((m) => (
                  <Fragment key={m.id}>
                    <tr className="model-main">
                      <td>
                        <EditableText
                          value={m.name}
                          ariaLabel={`Public name of ${m.name}`}
                          onCommit={(v) => patch(m.id, { name: v })}
                        />
                      </td>
                      <td>{m.backend_name}</td>
                      <td className="mono">{m.upstream_model}</td>
                      <td>
                        <Toggle checked={m.enabled} onChange={(v) => patch(m.id, { enabled: v })} />
                      </td>
                      <td className="actions">
                        <button
                          className="btn small"
                          disabled={benching !== null}
                          onClick={() => benchmark(m)}
                        >
                          {benching === m.id ? 'Benching…' : 'Benchmark'}
                        </button>
                        <ConfirmButton label="Delete" onConfirm={() => remove(m)} />
                      </td>
                    </tr>
                    <tr className="sub-row">
                      <td colSpan={5}>
                        <div className="model-metrics">
                          {PRICE_FIELDS.map((p) => (
                            <span className="metric" key={p.key}>
                              <span className="metric-label">
                                {p.label} <span className="dim">{currency ? `${currency}/1M` : 'per 1M'}</span>
                              </span>
                              <EditableNumber
                                display={microToPerM(m[p.key])}
                                ariaLabel={`${p.label} price of ${m.name}`}
                                onCommit={(n) => patch(m.id, { [p.key]: Math.round(n * 1e6) } as ModelPatch)}
                              />
                            </span>
                          ))}
                          <span className="metric">
                            <span className="metric-label">Context</span>
                            <EditableNumber
                              display={String(m.context_length)}
                              ariaLabel={`Context length of ${m.name}`}
                              onCommit={(n) => patch(m.id, { context_length: Math.round(n) })}
                            />
                          </span>
                          <span className="metric" title="Parallel streams the engine serves at once (0 = unset)">
                            <span className="metric-label">Parallel</span>
                            <EditableNumber
                              display={String(m.concurrency_limit)}
                              ariaLabel={`Concurrency limit of ${m.name}`}
                              onCommit={(n) => patch(m.id, { concurrency_limit: Math.round(n) })}
                            />
                          </span>
                          <span
                            className="metric"
                            title={m.bench_at ? `Benchmarked ${fmtDateTime(m.bench_at)}` : 'Manual value'}
                          >
                            <span className="metric-label">Speed tok/s</span>
                            <EditableNumber
                              display={m.bench_tok_per_s ? m.bench_tok_per_s.toFixed(1) : '0'}
                              ariaLabel={`Speed (tok/s) of ${m.name}`}
                              onCommit={(n) => patch(m.id, { bench_tok_per_s: n })}
                            />
                          </span>
                          <span
                            className="metric"
                            title="Mean busy % at which agents see “busy” (0 = default 80)"
                          >
                            <span className="metric-label">Busy ≥ %</span>
                            <EditableNumber
                              display={String(m.busy_threshold_pct)}
                              ariaLabel={`Busy threshold (percent) of ${m.name}`}
                              onCommit={(n) => patch(m.id, { busy_threshold_pct: Math.round(n) })}
                            />
                          </span>
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
    </div>
  );
}
