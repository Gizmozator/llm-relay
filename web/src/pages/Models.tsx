import { api } from '../api';
import { fmtMoney } from '../format';
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
