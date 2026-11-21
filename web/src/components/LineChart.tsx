import { useEffect, useRef } from 'react';
import Chart from 'chart.js/auto';

interface LineChartProps {
  labels: string[];
  values: number[];
  label: string;
  /** Formats a value for the tooltip and y-axis ticks. */
  format: (v: number) => string;
  /**
   * Multi-series mode: when set, renders one line per series (palette
   * colors) instead of the single `values` dataset.
   */
  series?: { label: string; values: number[] }[];
  /** Show the dataset legend (use with multi-series charts). */
  legend?: boolean;
  /** Fixed y-axis maximum (e.g. concurrency_limit × 100 for utilization). */
  yMax?: number;
}

const PALETTE = [
  '#38bdf8', '#818cf8', '#34d399', '#fbbf24',
  '#fb7185', '#a78bfa', '#22d3ee', '#f472b6',
];

/** Thin wrapper around Chart.js (no React binding): one or more line datasets. */
export default function LineChart({ labels, values, label, format, series, legend, yMax }: LineChartProps) {
  const multi = Boolean(series && series.length > 0);
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  const chartRef = useRef<Chart | null>(null);
  const formatRef = useRef(format);
  formatRef.current = format;

  useEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas) return;
    chartRef.current = new Chart(canvas, {
      type: 'line',
      data: {
        labels: [],
        datasets: multi
          ? (series ?? []).map((s, i) => ({
              label: s.label,
              data: [] as number[],
              borderColor: PALETTE[i % PALETTE.length],
              backgroundColor: 'transparent',
              fill: false,
              tension: 0.25,
              pointRadius: 0,
              pointHoverRadius: 3,
              borderWidth: 2,
            }))
          : [
              {
                label,
                data: [],
                borderColor: '#38bdf8',
                backgroundColor: 'rgba(56, 189, 248, 0.12)',
                fill: true,
                tension: 0.25,
                pointRadius: 2,
                pointHoverRadius: 4,
                borderWidth: 2,
              },
            ],
      },
      options: {
        responsive: true,
        maintainAspectRatio: false,
        interaction: { mode: 'index', intersect: false },
        plugins: {
          legend: { display: Boolean(legend), labels: { color: '#94a3b8' } },
          tooltip: {
            callbacks: {
              // dataset.label is kept current by the update effect, so the
              // tooltip follows metric switches without recreating the chart.
              label: (item) =>
                ` ${item.dataset.label}: ${formatRef.current(Number((item.parsed as { y: number }).y))}`,
            },
          },
        },
        scales: {
          x: {
            ticks: { color: '#94a3b8', maxRotation: 0, autoSkip: true, maxTicksLimit: 15 },
            grid: { color: 'rgba(148, 163, 184, 0.12)' },
          },
          y: {
            beginAtZero: true,
            ticks: {
              color: '#94a3b8',
              callback: (v) => formatRef.current(Number(v)),
            },
            grid: { color: 'rgba(148, 163, 184, 0.12)' },
          },
        },
      },
    });
    return () => {
      chartRef.current?.destroy();
      chartRef.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [multi, legend]);

  useEffect(() => {
    const chart = chartRef.current;
    if (!chart) return;
    chart.data.labels = labels;
    if (multi) {
      // Rebuild datasets when the model set changes; update data in place
      // when it matches (keeps Chart.js diffing cheap).
      const src = series ?? [];
      if (chart.data.datasets.length !== src.length) {
        chart.data.datasets = src.map((s, i) => ({
          label: s.label,
          data: s.values,
          borderColor: PALETTE[i % PALETTE.length],
          backgroundColor: 'transparent',
          fill: false,
          tension: 0.25,
          pointRadius: 0,
          pointHoverRadius: 3,
          borderWidth: 2,
        }));
      } else {
        src.forEach((s, i) => {
          chart.data.datasets[i].label = s.label;
          chart.data.datasets[i].data = s.values;
        });
      }
    } else {
      chart.data.datasets[0].label = label;
      chart.data.datasets[0].data = values;
    }
    // Fixed axis ceiling (utilization: concurrency_limit × 100 %);
    // undefined restores Chart.js auto-scaling.
    if (chart.options.scales?.y) chart.options.scales.y.max = yMax;
    chart.update('none');
  }, [labels, values, label, multi, series, yMax]);

  return (
    <div className="chart-wrap">
      <canvas ref={canvasRef} />
    </div>
  );
}
