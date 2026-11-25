import { useEffect, useRef, useState } from 'react';
import type { KeyboardEvent, ReactNode } from 'react';
import { copyToClipboard } from '../format';
import { useToast } from './Toast'; // one-way: Toast.tsx imports only React, no cycle

/** Inline error/info banner. */
export function Banner({ kind = 'error', children }: { kind?: 'error' | 'info'; children: ReactNode }) {
  return <div className={`banner banner-${kind}`}>{children}</div>;
}

/** Stat card for the dashboard/usage card rows (shared: the two pages
 *  duplicated this markup and the copies had drifted apart). */
export function Card({
  label,
  value,
  accent,
  title,
}: {
  label: string;
  value: ReactNode;
  accent?: boolean;
  title?: string;
}) {
  return (
    <div className={`card${accent ? ' card-accent' : ''}`} title={title}>
      <div className="card-label">{label}</div>
      <div className="card-value">{value}</div>
    </div>
  );
}

export function Loading({ label = 'Loading…' }: { label?: string }) {
  return (
    <div className="loading">
      <span className="spinner" /> {label}
    </div>
  );
}

export function Empty({ label = 'Nothing here yet.' }: { label?: string }) {
  return <div className="empty">{label}</div>;
}

export function Modal({
  title,
  onClose,
  children,
  wide,
  dismissable = true,
}: {
  title: string;
  onClose: () => void;
  children: ReactNode;
  wide?: boolean;
  /** Set false while a commit is in flight: overlay click and the header ×
   *  stop closing the dialog so the user cannot walk away from a request
   *  that will still land. */
  dismissable?: boolean;
}) {
  return (
    <div className="modal-overlay" onClick={dismissable ? onClose : undefined}>
      <div
        className={`modal${wide ? ' modal-wide' : ''}`}
        onClick={(e) => e.stopPropagation()}
        role="dialog"
        aria-modal="true"
      >
        <div className="modal-head">
          <h3>{title}</h3>
          <button className="icon-btn" onClick={onClose} disabled={!dismissable} aria-label="Close">
            ×
          </button>
        </div>
        <div className="modal-body">{children}</div>
      </div>
    </div>
  );
}

export function Toggle({
  checked,
  onChange,
  disabled,
  ariaLabel,
}: {
  checked: boolean;
  onChange: (v: boolean) => void;
  disabled?: boolean;
  /** Names the input for assistive tech: a bare switch has no text of its
   *  own, and a title on a wrapping span does not reach the checkbox. */
  ariaLabel?: string;
}) {
  return (
    <label className="switch">
      <input
        type="checkbox"
        checked={checked}
        disabled={disabled}
        aria-label={ariaLabel}
        onChange={(e) => onChange(e.target.checked)}
      />
      <span />
    </label>
  );
}

/** Two-step confirm button: first click arms it, second confirms. */
export function ConfirmButton({
  onConfirm,
  label,
  className = 'btn small danger',
  disabled = false,
}: {
  onConfirm: () => void | Promise<void>;
  label: string;
  className?: string;
  /** Blocks both the arm and the confirm (e.g. the action is in flight). */
  disabled?: boolean;
}) {
  const [armed, setArmed] = useState(false);

  useEffect(() => {
    if (!armed) return;
    const t = window.setTimeout(() => setArmed(false), 3000);
    return () => window.clearTimeout(t);
  }, [armed]);

  return (
    <button
      className={className}
      disabled={disabled}
      onClick={() => {
        if (armed) {
          setArmed(false);
          // Callers pass async handlers; a rejected promise would be an
          // unhandled rejection with the failure invisible. Surface it.
          try {
            void Promise.resolve(onConfirm()).catch((e: unknown) => {
              console.error('confirm action failed:', e);
            });
          } catch (e: unknown) {
            console.error('confirm action failed:', e);
          }
        } else {
          setArmed(true);
        }
      }}
    >
      {armed ? 'Confirm?' : label}
    </button>
  );
}

/** Read-only monospace value with a copy button; muted hint when unset. */
export function CopyField({
  label,
  value,
  hint,
}: {
  label?: string;
  value: string;
  hint: string;
}) {
  const [copied, setCopied] = useState(false);

  const copy = async () => {
    const ok = await copyToClipboard(value);
    setCopied(ok);
    if (ok) window.setTimeout(() => setCopied(false), 2000);
  };

  return (
    <div className="field">
      {label && <span className="field-label">{label}</span>}
      {value ? (
        <div className="copy-row">
          <input
            className="input mono"
            readOnly
            value={value}
            onFocus={(e) => e.target.select()}
            aria-label={label ?? 'Copyable value'}
          />
          <button className="btn" onClick={copy}>
            {copied ? 'Copied ✓' : 'Copy'}
          </button>
        </div>
      ) : (
        <span className="muted-hint">{hint}</span>
      )}
    </div>
  );
}

/**
 * Monospace snippet with a copy button; optional caption (a file name, a
 * command label). Shared by the agent guide and the connect-your-server
 * guide so the copy affordance stays identical across pages.
 */
export function CodeBlock({ text, title }: { text: string; title?: string }) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    const ok = await copyToClipboard(text);
    setCopied(ok);
    if (ok) window.setTimeout(() => setCopied(false), 2000);
  };
  return (
    <>
      {title && <div className="code-block-title mono">{title}</div>}
      <div className="code-block">
        <pre className="mono">{text}</pre>
        <button className="btn small" onClick={copy}>
          {copied ? 'Copied ✓' : 'Copy'}
        </button>
      </div>
    </>
  );
}

/** Labeled form field. */
export function Field({
  label,
  children,
}: {
  label: string;
  children: ReactNode;
}) {
  return (
    <label className="field">
      <span className="field-label">{label}</span>
      {children}
    </label>
  );
}

/** Stats-window choices for the federation pages: rolling day windows
 *  (the API accepts 1..365) plus UTC calendar months — "this month" and
 *  "last month" are the fairness-review presets. */
export const STATS_DAY_CHOICES = [7, 30, 90, 365];

export type StatsWindow =
  | { kind: 'days'; days: number }
  | { kind: 'month'; ym: string }; // "YYYY-MM" (UTC calendar month)

/** The query pair the stats endpoints take (days XOR month). */
export function windowToQuery(w: StatsWindow): { days?: number; month?: string } {
  return w.kind === 'days' ? { days: w.days } : { month: w.ym };
}

/** UTC "YYYY-MM" offset months from now (negative goes back). */
function monthOffset(utcNow: Date, offset: number): string {
  const d = new Date(Date.UTC(utcNow.getUTCFullYear(), utcNow.getUTCMonth() + offset, 1));
  return `${d.getUTCFullYear()}-${String(d.getUTCMonth() + 1).padStart(2, '0')}`;
}

/** Human label for the window, used in the panel headings. */
export function windowLabel(w: StatsWindow): string {
  if (w.kind === 'days') return `last ${w.days} days`;
  const [y, m] = w.ym.split('-').map(Number);
  return new Date(Date.UTC(y, m - 1, 1)).toLocaleDateString('en-US', {
    month: 'long',
    year: 'numeric',
    timeZone: 'UTC',
  });
}

/** "Stats window" select (rolling days + calendar months), shared by the
 *  federation pages. */
export function StatsWindowSelect({
  value,
  onChange,
}: {
  value: StatsWindow;
  onChange: (w: StatsWindow) => void;
}) {
  const now = new Date();
  const thisMonth = monthOffset(now, 0);
  const lastMonth = monthOffset(now, -1);
  const enc = (w: StatsWindow) => (w.kind === 'days' ? `d${w.days}` : `m:${w.ym}`);
  const dec = (s: string): StatsWindow =>
    s.startsWith('m:') ? { kind: 'month', ym: s.slice(2) } : { kind: 'days', days: Number(s.slice(1)) };
  return (
    <Field label="Stats window">
      <select className="input" value={enc(value)} onChange={(e) => onChange(dec(e.target.value))}>
        {STATS_DAY_CHOICES.map((d) => (
          <option key={d} value={`d${d}`}>
            last {d} days
          </option>
        ))}
        <option value={`m:${thisMonth}`}>this month ({windowLabel({ kind: 'month', ym: thisMonth })})</option>
        <option value={`m:${lastMonth}`}>last month ({windowLabel({ kind: 'month', ym: lastMonth })})</option>
      </select>
    </Field>
  );
}

/**
 * Parse free-typed numeric input with ONE strict rule shared by every
 * numeric editor: ^\d+([.,]\d*)?$ | ^[.,]\d+$ — digits with at most ONE
 * decimal separator, dot OR comma (comma serves EU keyboards), including
 * the shorthands ".5" (→ 0.5) and "5." (→ 5); at least one digit required.
 * Anything else is invalid: no sign, no exponent, no hex, no thousands
 * separators. A dot is always the decimal separator; "1,000" is REJECTED
 * as ambiguous (comma + exactly three digits could be a thousands
 * separator) instead of silently becoming 1000 or 1 — type 1000 for one
 * thousand. Returns null when the input does not match.
 */
export function parseNumInput(raw: string): number | null {
  const s = raw.trim();
  if (!/^(\d+([.,]\d*)?|[.,]\d+)$/.test(s)) return null;
  if (/^\d+,\d{3}$/.test(s)) return null; // "1,000": ambiguous with a thousands separator
  const n = Number(s.replace(',', '.'));
  // The regex admits arbitrarily long digit runs; a pasted 400-digit
  // number would otherwise commit as Infinity.
  return Number.isFinite(n) ? n : null;
}

/**
 * Shared machinery for the inline editors (EditableText, EditableNumber):
 * draft state adopted from the stored value, a typed-since-adopt flag, the
 * commit guard implementing the Promise<boolean> contract, and a spreadable
 * inputProps(commitDraft) bundle so the focus/blur/keydown wiring lives once.
 * adopt() snaps the draft to the stored value but stays inert while a commit
 * is in flight (the draft then holds the value being sent); discard() is the
 * revert-like variant that always fires (failed commit, rejected or abandoned
 * input).
 * A commit fails — and the draft snaps back — when onCommit resolves
 * false, rejects, or throws synchronously (a sync throw must not escape the
 * blur handler with the draft showing a value the server never accepted). The
 * revert reads the LATEST stored value through a ref, so an external refresh
 * landing mid-flight wins over the commit-time snapshot; and only the newest
 * commit may revert, so a slow request #1 resolving false after request #2
 * succeeded cannot snap the draft back past the newer edit. The guard stays
 * silent toward the user: CALLERS own error surfacing (every current caller
 * toasts in its own catch); the console.error is a last-resort trace for
 * callers forwarding a raw API promise.
 */
function useEditable(stored: string, label: string) {
  const [draft, setDraft] = useState(stored);
  const focused = useRef(false);
  const storedRef = useRef(stored);
  const commitSeq = useRef(0);
  // "The user actually typed since the last adopt/commit." A focus-blur with
  // no keystrokes must never PATCH: the draft is then a stale echo of what-
  // ever was on screen when focus landed, and an external refresh (delib-
  // erately skipped while focused) may have moved the server past it.
  const dirty = useRef(false);
  // Commits whose server round-trip has not settled yet. While one is in
  // flight the draft already holds what the server is getting — a plain
  // adopt() (e.g. the !dirty guard firing on a re-focus+blur mid-flight)
  // would snap the cell back to the pre-commit stored value and flicker
  // until the reload lands. The revert-on-failure path is NOT an adopt:
  // it must fire even here (the server refused, the draft is wrong).
  const inflight = useRef(0);
  // Adopt external updates, but never clobber a value mid-edit in this input.
  // The revert-target ref rides the same effect: assigning it during render
  // is a side effect a discarded concurrent render could leave stale.
  useEffect(() => {
    storedRef.current = stored;
    if (!focused.current) {
      setDraft(stored);
      dirty.current = false;
    }
  }, [stored]);

  // Discard the draft: snap to the stored value and clear the typed flag.
  // Revert-like, so it fires even while a commit is in flight — the draft
  // is wrong or unwanted there (server refused it, invalid input, an
  // abandoned edit), not a stale echo of an accepted value.
  const discard = () => {
    dirty.current = false;
    setDraft(storedRef.current);
  };
  // User-facing adopt (Escape, unchanged short-circuits): inert while a
  // commit is in flight — the draft holds the value being sent; snapping
  // to the stale stored value would flicker.
  const adopt = () => {
    if (inflight.current > 0) return;
    discard();
  };
  // User keystrokes: mark the draft as actually edited.
  const typeDraft = (v: string) => {
    dirty.current = true;
    setDraft(v);
  };

  const commit = <T,>(next: T, onCommit: (v: T) => void | Promise<boolean>) => {
    if (!dirty.current) {
      // Nothing was typed since the draft last matched the server: adopt the
      // (possibly refreshed) stored value instead of PATCHing a stale echo.
      adopt();
      return;
    }
    dirty.current = false;
    const seq = ++commitSeq.current;
    // Revert is NOT adopt(): the server refused this value, so the draft
    // must snap back even if another commit is still in flight.
    const revert = () => {
      if (seq === commitSeq.current && !focused.current) discard();
    };
    try {
      const r = onCommit(next);
      if (r instanceof Promise) {
        inflight.current++;
        // A rejected promise (a caller forwarding a raw API promise) is a
        // failed commit just like resolving false — snap back either way,
        // and never leave an unhandled rejection.
        void r.then(
          (ok) => {
            inflight.current--;
            if (ok === false) revert();
          },
          (e) => {
            inflight.current--;
            revert();
            console.error(`${label} commit failed`, e);
          },
        );
      }
    } catch (e) {
      revert();
      console.error(`${label} commit failed`, e);
    }
  };
  // Spreadable focus/blur/keydown wiring shared by every inline editor:
  // the call site keeps its own attrs (className, value, onChange, size,
  // inputMode…), only the commit plumbing lives here.
  const inputProps = (commitDraft: () => void) => ({
    onFocus: () => {
      focused.current = true;
    },
    onBlur: () => {
      focused.current = false;
      commitDraft();
    },
    onKeyDown: (e: KeyboardEvent<HTMLInputElement>) => {
      // During IME composition (CJK candidate selection) Enter accepts the
      // candidate and Escape cancels it — both belong to the input method,
      // not the cell; hijacking them would commit a half-composed value.
      if (e.nativeEvent.isComposing) return;
      if (e.key === 'Enter') {
        // "Accept this cell", not "submit the form": inside a modal <form>
        // the default Enter would implicitly submit (save + close) while the
        // user was still working through the row grid.
        e.preventDefault();
        (e.target as HTMLInputElement).blur();
      }
      if (e.key === 'Escape') adopt();
    },
  });

  return { draft, typeDraft, commit, adopt, discard, inputProps };
}

/** Inline-editable single-line text; commits on blur or Enter. */
export function EditableText({
  value,
  onCommit,
  mono,
  allowEmpty,
  placeholder,
  ariaLabel,
}: {
  value: string;
  /** Return Promise<boolean> to get revert-on-rejection: resolving false,
   *  rejecting or throwing synchronously snaps the input back to the stored
   *  value (a 409 "name taken" leaves the prop unchanged, so the
   *  adopt-external-updates effect alone would never fire). The component
   *  stays silent on failure — the CALLER owns user-visible error surfacing
   *  (toast in the catch); the hook's console.error only traces raw-promise
   *  callers. */
  onCommit: (v: string) => void | Promise<boolean>;
  mono?: boolean;
  allowEmpty?: boolean;
  placeholder?: string;
  ariaLabel?: string;
}) {
  const { draft, typeDraft, commit, adopt, inputProps } = useEditable(value, 'EditableText');

  const commitDraft = () => {
    const next = draft.trim();
    if (next ? next !== value : allowEmpty && value) commit(next, onCommit);
    else adopt();
  };

  return (
    <input
      className={`input input-inline${mono ? ' mono' : ''}`}
      value={draft}
      // Intrinsic width follows the value (tables size columns to max-content,
      // where a bare input would fall back to its ~20-char default and clip
      // long names). Grows live while editing.
      size={Math.max(draft.length, 10)}
      placeholder={placeholder}
      aria-label={ariaLabel}
      {...inputProps(commitDraft)}
      onChange={(e) => typeDraft(e.target.value)}
    />
  );
}

/**
 * Inline-editable number; commits on blur or Enter, reverts on invalid.
 * Unlike its ui.tsx siblings this one is NOT context-free: it surfaces
 * invalid/bound violations through useToast, so it must render under
 * ToastProvider (every call site does — App wraps the whole tree).
 */
export function EditableNumber({
  display,
  onCommit,
  ariaLabel,
  max,
}: {
  display: string;
  /** Same revert-on-rejection contract as EditableText: return
   *  Promise<boolean>; resolving false (or rejecting/throwing) snaps the
   *  input back to the stored display value. Failures stay silent here —
   *  the caller owns the error toast. */
  onCommit: (n: number) => void | Promise<boolean>;
  ariaLabel?: string;
  /** Largest accepted value, inclusive (default: no upper bound). Lower
   *  bounds are server-side rules (the unsigned parser already rejects
   *  anything below zero); an inclusive client min could not express the
   *  one strict bound that exists (price_multiplier > 0) anyway. */
  max?: number;
}) {
  const toast = useToast();
  const { draft, typeDraft, commit, adopt, discard, inputProps } = useEditable(display, 'EditableNumber');

  const commitDraft = () => {
    const raw = draft.trim();
    // Unchanged FIRST: a focus-blur no-op must never validate or PATCH.
    // Trimmed-string equality catches stored values the strict parser
    // cannot read back (legacy forms like "1e3"); the numeric compare
    // after it still counts "2,5" over "2.5" as unchanged.
    if (raw === display.trim()) {
      adopt();
      return;
    }
    const n = parseNumInput(raw);
    if (n !== null && n === parseNumInput(display)) {
      adopt();
      return;
    }
    // The paths below DISCARD the draft (abandoned or rejected input), so
    // they use discard(), not adopt(): a rejected draft must vanish even
    // while an unrelated commit is still in flight, or the invalid value
    // lingers on screen until the next reload.
    if (raw === '') {
      // Select-all + delete: abandoning the edit, not invalid typing —
      // revert quietly, the old silent snap-back.
      discard();
      return;
    }
    if (n === null) {
      // Invalid input is visible, not a silent snap-back: the user must
      // learn WHY the draft was rejected.
      discard();
      toast.push('error', 'Not a number — digits with one . or , separator (no thousands separators)');
      return;
    }
    if (max !== undefined && n > max) {
      discard();
      toast.push('error', `Value must be ≤ ${max}`);
      return;
    }
    commit(n, onCommit);
  };

  return (
    <input
      className="input input-inline input-num"
      value={draft}
      aria-label={ariaLabel}
      inputMode="decimal"
      {...inputProps(commitDraft)}
      onChange={(e) => typeDraft(e.target.value)}
    />
  );
}
