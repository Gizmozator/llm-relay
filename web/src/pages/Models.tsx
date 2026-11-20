import { api } from '../api';
import { fmtDateTime, fmtInt, fmtMoney } from '../format';
import { useLoad } from '../useLoad';
import { Banner, Empty, Loading } from '../components/ui';

const PRICE_FIELDS = [
  { key: 'price_in', label: 'Input' },
  { key: 'price_out', label: 'Output' },
  { key: 'price_cache_read', label: 'Cache read' },
  { key: 'price_cache_write', label: 'Cache write' },
] as const;

export default function ModelsPage() {
  const models = useLoad(() => api.getModels().then((r) => r.models), []);
  const info = useLoad(() => api.info(), []);
  const currency = info.data?.currency ?? '';

  return (
    <div className="page">
      <div className="page-head">
        <h2>Models</h2>
      </div>

      <section className="panel">
        {models.loading && !models.data ? (
          <Loading />
        ) : models.error ? (
          <Banner>{models.error}</Banner>
        ) : !models.data || models.data.length === 0 ? (
          <Empty label="No models available yet." />
        ) : (
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>Model</th>
                  {PRICE_FIELDS.map((p) => (
                    <th className="num" key={p.key}>
                      {p.label}
                      <div className="th-sub">per 1M tokens</div>
                    </th>
                  ))}
                  <th className="num">Context</th>
                  <th className="num">Speed</th>
                  <th className="num">Parallel</th>
                </tr>
              </thead>
              <tbody>
                {models.data.map((m) => (
                  <tr key={m.name}>
                    <td className="mono">{m.name}</td>
                    {PRICE_FIELDS.map((p) => (
                      <td className="num money" key={p.key}>
                        {fmtMoney(m[p.key], currency)}
                      </td>
                    ))}
                    <td className="num">{m.context_length ? `${fmtInt(m.context_length)} tokens` : '—'}</td>
                    <td className="num" title={m.bench_at ? fmtDateTime(m.bench_at) : undefined}>
                      {m.bench_tok_per_s ? `${m.bench_tok_per_s.toFixed(1)} tok/s` : '—'}
                    </td>
                    <td className="num">{m.concurrency_limit ? fmtInt(m.concurrency_limit) : '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </div>
  );
}
