import { Fragment, useEffect, useMemo, useState } from 'react';
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
  Field,
  Loading,
  Modal,
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
  bench_pp_tok_per_s?: number;
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
  // Model whose price calculator dialog is open.
  const [calcFor, setCalcFor] = useState<CatalogModel | null>(null);

  const patch = async (id: number, p: ModelPatch): Promise<boolean> => {
    try {
      const updated = await api.patchModel(id, p);
      setRows((prev) => (prev ?? []).map((m) => (m.id === id ? updated : m)));
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
                      <td className="num">
                        <EditableNumber
                          display={String(m.concurrency_limit)}
                          ariaLabel={`Concurrency limit of ${m.name}`}
                          onCommit={(n) => patch(m.id, { concurrency_limit: Math.round(n) })}
                        />
                      </td>
                      <td className="num">
                        <EditableNumber
                          display={String(m.busy_threshold_pct)}
                          ariaLabel={`Busy threshold (percent) of ${m.name}`}
                          onCommit={(n) => patch(m.id, { busy_threshold_pct: Math.round(n) })}
                        />
                      </td>
                      <td className="actions">
                        <button
                          className="btn small"
                          onClick={() => setCalcFor(m)}
                        >
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
                      </td>
                    </tr>
                    <tr className="sub-row">
                      <td colSpan={7}>
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
                          <span
                            className="metric"
                            title={m.bench_at ? `Benchmarked ${fmtDateTime(m.bench_at)}` : 'Manual value'}
                          >
                            <span className="metric-label">TG tok/s</span>
                            <EditableNumber
                              display={m.bench_tok_per_s ? m.bench_tok_per_s.toFixed(1) : '0'}
                              ariaLabel={`Generation speed (tok/s) of ${m.name}`}
                              onCommit={(n) => patch(m.id, { bench_tok_per_s: n })}
                            />
                          </span>
                          <span
                            className="metric"
                            title={m.bench_at ? `Benchmarked ${fmtDateTime(m.bench_at)}` : 'Manual value'}
                          >
                            <span className="metric-label">PP tok/s</span>
                            <EditableNumber
                              display={m.bench_pp_tok_per_s ? m.bench_pp_tok_per_s.toFixed(0) : '0'}
                              ariaLabel={`Prompt-processing speed (tok/s) of ${m.name}`}
                              onCommit={(n) => patch(m.id, { bench_pp_tok_per_s: n })}
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

  const apply = async () => {
    setBusy(true);
    try {
      localStorage.setItem(CALC_LS_KEY, JSON.stringify(inp));
      const [price_in, price_out, price_cache_read, price_cache_write] = parsed.map((v) =>
        Math.round(v * 1e6),
      );
      await onApply({ price_in, price_out, price_cache_read, price_cache_write });
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
