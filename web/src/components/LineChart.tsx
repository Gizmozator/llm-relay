import { useEffect, useRef } from 'react';
import Chart from 'chart.js/auto';

interface LineChartProps {
  labels: string[];
  values: number[];
  label: string;
  /** Formats a value for the tooltip and y-axis ticks. */
  format: (v: number) => string;
}

/** Thin wrapper around Chart.js (no React binding): one line chart instance. */
export default function LineChart({ labels, values, label, format }: LineChartProps) {
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
        datasets: [
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
          legend: { display: false },
          tooltip: {
            callbacks: {
              label: (item) =>
                ` ${label}: ${formatRef.current(Number((item.parsed as { y: number }).y))}`,
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
  }, []);

  useEffect(() => {
    const chart = chartRef.current;
    if (!chart) return;
    chart.data.labels = labels;
    chart.data.datasets[0].data = values;
    chart.update('none');
  }, [labels, values]);

  return (
    <div className="chart-wrap">
      <canvas ref={canvasRef} />
    </div>
  );
}
