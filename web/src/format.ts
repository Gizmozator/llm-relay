// Display formatting helpers. Money is integer micro-units; display = micro / 1e6.

export function fmtInt(n: number): string {
  return n.toLocaleString('en-US');
}

/**
 * "New" input tokens: the full-price input, i.e. total prompt minus the
 * cached and cache-write buckets. In + cached + cache-write = total input,
 * so the columns are disjoint and additive. Clamped at 0 for rows where an
 * upstream reported inconsistent counts.
 */
export function inNewTokens(t: {
  prompt_tokens: number;
  cached_tokens: number;
  cache_write_tokens: number;
}): number {
  return Math.max(0, t.prompt_tokens - t.cached_tokens - t.cache_write_tokens);
}

/** Money from micro-units: 2 decimals at >=1, 4 below. */
export function fmtMoney(micro: number, currency: string): string {
  const v = micro / 1e6;
  const digits = Math.abs(v) >= 1 ? 2 : 4;
  const s = v.toLocaleString('en-US', {
    minimumFractionDigits: digits,
    maximumFractionDigits: digits,
  });
  return currency ? `${s} ${currency}` : s;
}

/** Micro-units per 1M tokens → plain number string for price inputs. */
export function microToPerM(micro: number): string {
  return String(micro / 1e6);
}

export function fmtDateTime(iso: string | null | undefined): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return iso;
  return `${d.toLocaleDateString('en-GB')} ${d.toLocaleTimeString('en-GB', { hour12: false })}`;
}

/** Compact elapsed time from seconds: "45s", "1m 23s", "2h 05m", "3d 4h". */
export function fmtDurationSec(s: number): string {
  s = Math.max(0, Math.floor(s));
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m ${String(s % 60).padStart(2, '0')}s`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ${String(Math.floor((s % 3600) / 60)).padStart(2, '0')}m`;
  return `${Math.floor(s / 86400)}d ${Math.floor((s % 86400) / 3600)}h`;
}

/** Coarse "time ago" from an ISO timestamp: "just now", "45s ago", "3 min ago", "2 h ago", "5 d ago". */
export function fmtAgo(iso: string | null | undefined): string {
  if (!iso) return '—';
  const t = new Date(iso).getTime();
  if (isNaN(t)) return iso;
  const s = Math.max(0, Math.floor((Date.now() - t) / 1000));
  if (s < 10) return 'just now';
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`;
  return `${Math.floor(s / 86400)} d ago`;
}

/** Local YYYY-MM-DD for date inputs. */
export function isoDate(d: Date): string {
  const y = d.getFullYear();
  const m = String(d.getMonth() + 1).padStart(2, '0');
  const day = String(d.getDate()).padStart(2, '0');
  return `${y}-${m}-${day}`;
}

export function daysAgoISO(n: number): string {
  const d = new Date();
  d.setDate(d.getDate() - n);
  return isoDate(d);
}

export async function copyToClipboard(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // Fallback for non-secure contexts.
    try {
      const ta = document.createElement('textarea');
      ta.value = text;
      ta.style.position = 'fixed';
      ta.style.opacity = '0';
      document.body.appendChild(ta);
      ta.select();
      const ok = document.execCommand('copy');
      document.body.removeChild(ta);
      return ok;
    } catch {
      return false;
    }
  }
}
