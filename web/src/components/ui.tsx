import { useEffect, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { copyToClipboard } from '../format';

/** Inline error/info banner. */
export function Banner({ kind = 'error', children }: { kind?: 'error' | 'info'; children: ReactNode }) {
  return <div className={`banner banner-${kind}`}>{children}</div>;
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
}: {
  title: string;
  onClose: () => void;
  children: ReactNode;
  wide?: boolean;
}) {
  return (
    <div className="modal-overlay" onClick={onClose}>
      <div
        className={`modal${wide ? ' modal-wide' : ''}`}
        onClick={(e) => e.stopPropagation()}
        role="dialog"
        aria-modal="true"
      >
        <div className="modal-head">
          <h3>{title}</h3>
          <button className="icon-btn" onClick={onClose} aria-label="Close">
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
}: {
  checked: boolean;
  onChange: (v: boolean) => void;
  disabled?: boolean;
}) {
  return (
    <label className="switch">
      <input
        type="checkbox"
        checked={checked}
        disabled={disabled}
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
}: {
  onConfirm: () => void;
  label: string;
  className?: string;
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
      onClick={() => {
        if (armed) {
          setArmed(false);
          onConfirm();
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

/**
 * Parse free-typed numeric input: strips spaces and thousands separators,
 * then validates the whole string (no trailing junk). Returns null when the
 * input is not a finite number. With decimalComma, a comma is read as the
 * decimal separator ("2,5" → 2.5) — right for fractional fields like the
 * price multiplier; integer fields keep comma = thousands separator.
 */
export function parseNumInput(raw: string, decimalComma = false): number | null {
  let s = raw.replace(/[\s\u00a0]/g, '');
  if (decimalComma) s = s.replace(',', '.');
  s = s.replace(/,/g, '');
  if (!s) return null;
  const n = Number(s);
  return Number.isFinite(n) ? n : null;
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
  onCommit: (v: string) => void;
  mono?: boolean;
  allowEmpty?: boolean;
  placeholder?: string;
  ariaLabel?: string;
}) {
  const [v, setV] = useState(value);
  const focused = useRef(false);
  // Adopt external updates, but never clobber a value mid-edit in this input.
  useEffect(() => {
    if (!focused.current) setV(value);
  }, [value]);

  const commit = () => {
    const next = v.trim();
    if (next ? next !== value : allowEmpty && value) onCommit(next);
    else setV(value);
  };

  return (
    <input
      className={`input input-inline${mono ? ' mono' : ''}`}
      value={v}
      placeholder={placeholder}
      aria-label={ariaLabel}
      onFocus={() => {
        focused.current = true;
      }}
      onChange={(e) => setV(e.target.value)}
      onBlur={() => {
        focused.current = false;
        commit();
      }}
      onKeyDown={(e) => {
        if (e.key === 'Enter') (e.target as HTMLInputElement).blur();
        if (e.key === 'Escape') setV(value);
      }}
    />
  );
}

/** Inline-editable number; commits on blur or Enter, reverts on invalid. */
export function EditableNumber({
  display,
  onCommit,
  ariaLabel,
}: {
  display: string;
  onCommit: (n: number) => void;
  ariaLabel?: string;
}) {
  const [v, setV] = useState(display);
  const focused = useRef(false);
  // Adopt external updates, but never clobber a value mid-edit in this input.
  useEffect(() => {
    if (!focused.current) setV(display);
  }, [display]);

  const commit = () => {
    const n = parseNumInput(v);
    if (n === null || n < 0) {
      setV(display);
      return;
    }
    if (n !== Number(display)) onCommit(n);
    else setV(display);
  };

  return (
    <input
      className="input input-inline input-num"
      value={v}
      aria-label={ariaLabel}
      inputMode="decimal"
      onFocus={() => {
        focused.current = true;
      }}
      onChange={(e) => setV(e.target.value)}
      onBlur={() => {
        focused.current = false;
        commit();
      }}
      onKeyDown={(e) => {
        if (e.key === 'Enter') (e.target as HTMLInputElement).blur();
        if (e.key === 'Escape') setV(display);
      }}
    />
  );
}
