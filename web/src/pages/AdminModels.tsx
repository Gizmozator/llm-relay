import { useState } from 'react';
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
                  {PRICE_FIELDS.map((p) => (
                    <th className="num" key={p.key}>
                      {p.label}
                      <div className="th-sub">{currency ? `${currency}/1M` : 'per 1M'}</div>
                    </th>
                  ))}
                  <th className="num">
                    Context length
                    <div className="th-sub">tokens</div>
                  </th>
                  <th className="num">
                    Concurrency
                    <div className="th-sub">parallel</div>
                  </th>
                  <th className="num">
                    Speed
                    <div className="th-sub">tok/s</div>
                  </th>
                  <th></th>
                </tr>
              </thead>
              <tbody>
                {rows.map((m) => (
                  <tr key={m.id}>
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
                    {PRICE_FIELDS.map((p) => (
                      <td className="num" key={p.key}>
                        <EditableNumber
                          display={microToPerM(m[p.key])}
                          ariaLabel={`${p.label} price of ${m.name}`}
                          onCommit={(n) => patch(m.id, { [p.key]: Math.round(n * 1e6) } as ModelPatch)}
                        />
                      </td>
                    ))}
                    <td className="num">
                      <EditableNumber
                        display={String(m.context_length)}
                        ariaLabel={`Context length of ${m.name}`}
                        onCommit={(n) => patch(m.id, { context_length: Math.round(n) })}
                      />
                    </td>
                    <td className="num">
                      <EditableNumber
                        display={String(m.concurrency_limit)}
                        ariaLabel={`Concurrency limit of ${m.name}`}
                        onCommit={(n) => patch(m.id, { concurrency_limit: Math.round(n) })}
                      />
                    </td>
                    <td
                      className="num"
                      title={m.bench_at ? `Benchmarked ${fmtDateTime(m.bench_at)}` : 'Manual value'}
                    >
                      <EditableNumber
                        display={m.bench_tok_per_s ? m.bench_tok_per_s.toFixed(1) : '0'}
                        ariaLabel={`Speed (tok/s) of ${m.name}`}
                        onCommit={(n) => patch(m.id, { bench_tok_per_s: n })}
                      />
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
                ))}
              </tbody>
            </table>
          </div>
        </section>
      )}
    </div>
  );
}
