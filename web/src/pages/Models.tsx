import { api, errMsg } from '../api';
import type { CatalogModel } from '../api';
import { microToPerM } from '../format';
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
};

const PRICE_FIELDS = [
  { key: 'price_in', label: 'Input' },
  { key: 'price_out', label: 'Output' },
  { key: 'price_cache_read', label: 'Cache read' },
  { key: 'price_cache_write', label: 'Cache write' },
] as const;

export default function ModelsPage() {
  const toast = useToast();
  const models = useLoad(() => api.models().then((r) => r.models), []);
  const settings = useLoad(() => api.settings(), []);
  const currency = settings.data?.currency ?? '';

  const patch = async (id: number, p: ModelPatch) => {
    try {
      await api.patchModel(id, p);
      models.reload();
    } catch (e: unknown) {
      toast.push('error', errMsg(e));
      models.reload();
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
      ) : !models.data || models.data.length === 0 ? (
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
                  <th></th>
                </tr>
              </thead>
              <tbody>
                {models.data.map((m) => (
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
                    <td className="actions">
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
