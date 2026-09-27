/**
 * 一键巡检的执行器：分批 + 重试 + 进度。
 *
 * Drives the existing per-page batch loader rather than fetching quota itself, so
 * a swept credential lands in the same store the cards read from and the page
 * updates as the sweep goes — no second code path that could disagree with the
 * one a manual refresh uses.
 */

import { useCallback, useRef, useState } from 'react';
import type { TFunction } from 'i18next';
import type { AuthFileItem } from '@/types';
import type { QuotaFileEntry } from '../logic';
import { QUOTA_ADAPTERS, getQuotaSetter } from '../providers';
import {
  buildScanFailures,
  chunkEntries,
  DEFAULT_QUOTA_SCAN_TUNING,
  sleep,
  type QuotaScanFailure,
  type QuotaScanTuning,
} from '../scanLogic';

export interface QuotaScanProgress {
  total: number;
  completed: number;
  failed: number;
  /** 重试阶段时为 true，便于界面区分「在扫」和「在补」。 */
  retrying: boolean;
}

export interface QuotaScanOutcome {
  failures: QuotaScanFailure[];
  scanned: number;
  cancelled: boolean;
}

const emptyProgress: QuotaScanProgress = {
  total: 0,
  completed: 0,
  failed: 0,
  retrying: false,
};

export function useQuotaScan(
  loadQuota: (targets: QuotaFileEntry[]) => Promise<void>,
  quotaFor: (entry: QuotaFileEntry) => { status?: string } | undefined
) {
  const [running, setRunning] = useState(false);
  const [progress, setProgress] = useState<QuotaScanProgress>(emptyProgress);
  const cancelRef = useRef(false);
  // Guards against a second press starting a concurrent sweep; the loader has its
  // own dedupe, but two sweeps would still interleave their progress reporting.
  const runningRef = useRef(false);

  const cancel = useCallback(() => {
    cancelRef.current = true;
  }, []);

  const run = useCallback(
    async (
      entries: QuotaFileEntry[],
      t: TFunction,
      tuning: QuotaScanTuning = DEFAULT_QUOTA_SCAN_TUNING
    ): Promise<QuotaScanOutcome> => {
      if (runningRef.current || entries.length === 0) {
        return { failures: [], scanned: 0, cancelled: false };
      }
      runningRef.current = true;
      cancelRef.current = false;
      setRunning(true);
      setProgress({ ...emptyProgress, total: entries.length });

      try {
        const batches = chunkEntries(entries, tuning.batchSize);

        // ---- 主扫 ----
        for (const batch of batches) {
          if (cancelRef.current) break;
          await loadQuota(batch);
          setProgress((prev) => ({
            ...prev,
            completed: prev.completed + batch.length,
          }));
          if (!cancelRef.current) await sleep(tuning.batchIntervalMs);
        }

        if (cancelRef.current) {
          return { failures: [], scanned: 0, cancelled: true };
        }

        // ---- 重试 ----
        // Read failures back out of the store rather than tracking them live: the
        // loader owns what counts as a failure, and asking it again is how this
        // stays consistent with what the cards show.
        const failedNames = new Set<string>();
        for (const entry of entries) {
          if (quotaFor(entry)?.status === 'error') failedNames.add(entry.file.name);
        }

        setProgress((prev) => ({
          ...prev,
          failed: failedNames.size,
          retrying: failedNames.size > 0,
        }));

        // Retried one at a time, on its own cadence. Retrying inside the batch
        // would replay the request while the other 19 are still hitting the
        // upstream, which is exactly the condition that produced the failure.
        for (let round = 0; round < tuning.maxRetries; round += 1) {
          if (cancelRef.current || failedNames.size === 0) break;
          await sleep(tuning.retryIntervalMs);

          const stillFailing = new Set<string>();
          for (const entry of entries) {
            if (!failedNames.has(entry.file.name)) continue;
            if (cancelRef.current) break;

            await loadQuota([entry]);
            if (quotaFor(entry)?.status === 'error') stillFailing.add(entry.file.name);
            await sleep(tuning.retryIntervalMs);
          }
          failedNames.clear();
          stillFailing.forEach((name) => failedNames.add(name));

          setProgress((prev) => ({ ...prev, failed: failedNames.size }));
        }

        setProgress((prev) => ({ ...prev, retrying: false }));

        const names = Array.from(failedNames);
        const messageFor = (name: string) => {
          for (const entry of entries) {
            if (entry.file.name !== name) continue;
            const state = quotaFor(entry) as { error?: string } | undefined;
            return state?.error ?? t('common.unknown_error');
          }
          return t('common.unknown_error');
        };
        const fileFor = (name: string): AuthFileItem | undefined =>
          entries.find((entry) => entry.file.name === name)?.file;

        return {
          failures: buildScanFailures(names, messageFor, fileFor),
          scanned: entries.length,
          cancelled: false,
        };
      } finally {
        runningRef.current = false;
        setRunning(false);
      }
    },
    [loadQuota, quotaFor]
  );

  return { running, progress, run, cancel };
}

/**
 * 停用后把缓存里的这些凭证抹掉。
 *
 * A disabled credential's quota is no longer a fact worth keeping on screen, and
 * the card would otherwise keep rendering a status the credential can no longer
 * have. Done per provider because the store is keyed by provider.
 */
export function pruneQuotaForDisabled(names: readonly string[]) {
  if (names.length === 0) return;
  const target = new Set(names);
  for (const adapter of Object.values(QUOTA_ADAPTERS)) {
    const setQuota = getQuotaSetter(adapter);
    setQuota((prev) => {
      const stale = Object.keys(prev).filter((name) => target.has(name));
      if (stale.length === 0) return prev;
      const next = { ...prev };
      stale.forEach((name) => delete next[name]);
      return next;
    });
  }
}
