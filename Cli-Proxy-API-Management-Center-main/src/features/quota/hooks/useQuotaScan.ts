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
import { QUOTA_ADAPTERS, getQuotaMap, getQuotaSetter } from '../providers';
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
  /**
   * 真正拿到结果的凭证数（success 或 error，而不是 idle）。
   *
   * 存在的理由：「0 个失败」和「根本没取到数」必须长得不一样。`loadQuota`
   * 在自身忙时会直接返回 —— 那一批既没发请求也没写 store，但 await 正常
   * 返回，循环会把它记成已完成。没有这个计数，一次实际什么都没做的巡检会
   * 报出「全部正常」，比报错更危险。
   */
  resolved: number;
}

const emptyProgress: QuotaScanProgress = {
  total: 0,
  completed: 0,
  failed: 0,
  retrying: false,
};

/**
 * 直读 store 的额度读取器 —— 不订阅、不缓存、每次调用取当时的 state。
 *
 * 这个函数的存在本身就是为了修一个 bug：巡检原先接收 QuotaPage 传进来的
 * `quotaFor`，那是 `useCallback` 按渲染快照构造的。巡检要跑几分钟，期间
 * store 被持续写入、组件反复重渲染，但那个正在跑的 async 函数始终握着
 * **点击那一刻**的引用 —— 于是取数正确、卡片正确、筛选计数正确，唯独判定
 * 读的是点击前的空状态，永远报 0 个失败。
 *
 * 所以这里不接受任何外部 reader。reader 由 store 现取，陈旧引用在结构上
 * 无法存在，而不是靠调用方「记得传对的东西」。
 */
export const readLiveQuota = (
  entry: QuotaFileEntry
): { status?: string; error?: string } | undefined =>
  getQuotaMap(QUOTA_ADAPTERS[entry.type])[entry.file.name] as
    | { status?: string; error?: string }
    | undefined;

export function useQuotaScan(loadQuota: (targets: QuotaFileEntry[]) => Promise<void>) {
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
        return { failures: [], scanned: 0, cancelled: false, resolved: 0 };
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
          return { failures: [], scanned: 0, cancelled: true, resolved: 0 };
        }

        // ---- 重试 ----
        // Failures are read back out of the store rather than tracked live: the
        // loader owns what counts as a failure, and asking it again is how this
        // stays consistent with what the cards show. The read goes through
        // readLiveQuota so it sees every batch that has landed since.
        const failedNames = new Set<string>();
        for (const entry of entries) {
          if (readLiveQuota(entry)?.status === 'error') failedNames.add(entry.file.name);
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
            if (readLiveQuota(entry)?.status === 'error') stillFailing.add(entry.file.name);
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
            return readLiveQuota(entry)?.error ?? t('common.unknown_error');
          }
          return t('common.unknown_error');
        };
        const fileFor = (name: string): AuthFileItem | undefined =>
          entries.find((entry) => entry.file.name === name)?.file;

        // How many actually produced an answer. Anything still `idle` never got
        // fetched — most likely a batch the loader declined because it was busy.
        const resolved = entries.filter((entry) => {
          const status = readLiveQuota(entry)?.status;
          return status !== undefined && status !== 'idle' && status !== 'loading';
        }).length;

        return {
          failures: buildScanFailures(names, messageFor, fileFor),
          scanned: entries.length,
          cancelled: false,
          resolved,
        };
      } finally {
        runningRef.current = false;
        setRunning(false);
      }
    },
    [loadQuota]
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
