import { useCallback, useEffect, useState } from 'react';
import { errMsg } from './api';

export interface LoadState<T> {
  data: T | null;
  loading: boolean;
  error: string | null;
  reload: () => void;
}

/**
 * Runs `loader` on mount and whenever `deps` change; exposes data/loading/error
 * and a manual `reload`.
 */
export function useLoad<T>(loader: () => Promise<T>, deps: unknown[]): LoadState<T> {
  const [data, setData] = useState<T | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [nonce, setNonce] = useState(0);

  const reload = useCallback(() => setNonce((n) => n + 1), []);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);
    // A synchronous throw from the loader (bad accessor, coding bug)
    // must surface as the load error, not an unhandled rejection.
    let pending: Promise<T>;
    try {
      pending = loader();
    } catch (e: unknown) {
      setError(errMsg(e));
      setLoading(false);
      return;
    }
    pending
      .then((d) => {
        if (!cancelled) {
          setData(d);
          setLoading(false);
        }
      })
      .catch((e: unknown) => {
        if (!cancelled) {
          setError(errMsg(e));
          setLoading(false);
        }
      });
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, nonce]);

  return { data, loading, error, reload };
}
