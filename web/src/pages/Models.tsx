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
                  <th className="num" title="Token generation speed">
                    TG
                  </th>
                  <th className="num" title="Prompt-processing speed">
                    PP
                  </th>
                  <th className="num">Parallel</th>
                </tr>
              </thead>
              <tbody>
                {models.data.map((m) => {
                  // A group row pools several machines under one name; it
                  // carries no single price/speed of its own (billing and
                  // speed follow the member that serves the session), so
                  // its numeric cells stay empty instead of showing zeros.
                  const isGroup = m.kind === 'group';
                  // One place for the "group rows carry no numbers" rule:
                  // unset values and group rows both render an em dash, so
                  // a future column cannot forget the guard.
                  const num = (v: number, fmt: (n: number) => string) =>
                    !isGroup && v ? fmt(v) : '—';
                  return (
                    <tr key={m.name}>
                      <td className="mono">
                        {m.name}
                        {isGroup && (
                          <span
                            className="badge group"
                            title="Model group: one name backed by several machines; each conversation stays pinned to one of them"
                          >
                            group
                          </span>
                        )}
                      </td>
                      {PRICE_FIELDS.map((p) => (
                        <td className="num money" key={p.key}>
                          {/* 0 is a real price (free) — only group rows dash here. */}
                          {isGroup ? '—' : fmtMoney(m[p.key], currency)}
                        </td>
                      ))}
                      <td className="num">{num(m.context_length, (v) => `${fmtInt(v)} tokens`)}</td>
                      <td className="num" title={m.bench_at ? fmtDateTime(m.bench_at) : undefined}>
                        {num(m.bench_tok_per_s, (v) => `${v.toFixed(1)} tok/s`)}
                      </td>
                      <td className="num" title={m.bench_at ? fmtDateTime(m.bench_at) : undefined}>
                        {num(m.bench_pp_tok_per_s, (v) => `${v.toFixed(0)} tok/s`)}
                      </td>
                      <td className="num">{num(m.concurrency_limit, fmtInt)}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </div>
  );
}
