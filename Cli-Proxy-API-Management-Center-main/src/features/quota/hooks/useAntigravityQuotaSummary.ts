/**
 * Antigravity Gemini/Claude schedulable-quota summary.
 *
 * Local-state-derived (no live upstream calls), so it's cheap to poll on a
 * timer in addition to the manual refresh button — mirrors the cache/poll
 * shape of useProviderRecentRequests.
 */

import { useCallback, useEffect, useMemo, useState } from 'react';
import { useInterval } from '@/hooks/useInterval';
import { antigravityQuotaSummaryApi, type AntigravityQuotaSummaryResponse } from '@/services/api';
import { useAuthStore } from '@/stores';

const ANTIGRAVITY_QUOTA_SUMMARY_POLL_MS = 60_000;
const ANTIGRAVITY_QUOTA_SUMMARY_STALE_TIME_MS = 60_000;

export type UseAntigravityQuotaSummaryOptions = {
  enabled?: boolean;
};

type AntigravityQuotaSummaryCache = {
  cachedSummary: AntigravityQuotaSummaryResponse | null;
  cachedAt: number;
  inFlightRequest: Promise<AntigravityQuotaSummaryResponse> | null;
};

const createCache = (): AntigravityQuotaSummaryCache => ({
  cachedSummary: null,
  cachedAt: 0,
  inFlightRequest: null,
});

const createCacheController = () => {
  let currentApiBase = '';
  let currentManagementKey = '';
  let currentCache = createCache();

  return {
    forScope(apiBase: string, managementKey: string): AntigravityQuotaSummaryCache {
      if (apiBase !== currentApiBase || managementKey !== currentManagementKey) {
        currentApiBase = apiBase;
        currentManagementKey = managementKey;
        currentCache = createCache();
      }
      return currentCache;
    },
  };
};

const cacheController = createCacheController();

const fetchAntigravityQuotaSummary = async (
  cache: AntigravityQuotaSummaryCache
): Promise<AntigravityQuotaSummaryResponse> => {
  if (!cache.inFlightRequest) {
    const request = antigravityQuotaSummaryApi
      .get()
      .then((payload) => {
        cache.cachedSummary = payload;
        cache.cachedAt = Date.now();
        return payload;
      })
      .finally(() => {
        if (cache.inFlightRequest === request) {
          cache.inFlightRequest = null;
        }
      });
    cache.inFlightRequest = request;
  }

  return cache.inFlightRequest;
};

export function useAntigravityQuotaSummary(options: UseAntigravityQuotaSummaryOptions = {}) {
  const enabled = options.enabled ?? true;
  const apiBase = useAuthStore((state) => state.apiBase);
  const managementKey = useAuthStore((state) => state.managementKey);
  const cache = useMemo(
    () => cacheController.forScope(apiBase, managementKey),
    [apiBase, managementKey]
  );

  const [summaryState, setSummaryState] = useState(() => ({ cache, value: cache.cachedSummary }));
  const [loadingState, setLoadingState] = useState(() => ({ cache, value: false }));
  const [error, setError] = useState<string | null>(null);

  const setSummaryForCurrentScope = useCallback(
    (value: AntigravityQuotaSummaryResponse | null) => setSummaryState({ cache, value }),
    [cache]
  );

  const setLoadingForCurrentScope = useCallback(
    (value: boolean) => setLoadingState({ cache, value }),
    [cache]
  );

  const loadSummary = useCallback(
    async (loadOptions: { force?: boolean } = {}) => {
      if (!enabled) {
        return cache.cachedSummary;
      }

      const hasFreshCache =
        cache.cachedAt > 0 && Date.now() - cache.cachedAt < ANTIGRAVITY_QUOTA_SUMMARY_STALE_TIME_MS;

      if (!loadOptions.force && hasFreshCache) {
        setSummaryForCurrentScope(cache.cachedSummary);
        return cache.cachedSummary;
      }

      setLoadingForCurrentScope(true);
      try {
        const next = await fetchAntigravityQuotaSummary(cache);
        setError(null);
        setSummaryForCurrentScope(next);
        return next;
      } catch (err: unknown) {
        setError(err instanceof Error ? err.message : String(err));
        if (cache.cachedAt > 0) {
          setSummaryForCurrentScope(cache.cachedSummary);
        }
        return cache.cachedSummary;
      } finally {
        setLoadingForCurrentScope(false);
      }
    },
    [cache, enabled, setLoadingForCurrentScope, setSummaryForCurrentScope]
  );

  const refresh = useCallback(async () => loadSummary({ force: true }), [loadSummary]);

  useEffect(() => {
    if (!enabled) {
      setSummaryForCurrentScope(null);
      return;
    }
    void loadSummary().catch(() => {});
  }, [enabled, loadSummary, setSummaryForCurrentScope]);

  useInterval(
    () => {
      void refresh().catch(() => {});
    },
    enabled ? ANTIGRAVITY_QUOTA_SUMMARY_POLL_MS : null
  );

  const summary = summaryState.cache === cache ? summaryState.value : cache.cachedSummary;
  const isLoading = loadingState.cache === cache ? loadingState.value : cache.inFlightRequest !== null;

  return {
    summary: enabled ? summary : null,
    isLoading: enabled ? isLoading : false,
    error,
    refresh,
  };
}
