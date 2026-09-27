/**
 * 用真实的 run() 验证失败判定 —— 不是模拟，是那个被点按钮时调用的函数。
 *
 * The bug this locks down was invisible to the pure-logic tests: `run` fetched
 * correctly and the store held the truth, but `run` read through a reader it had
 * closed over on the click, so it saw the store as it was *before* the sweep and
 * reported zero failures. A test of the classification helper cannot catch that,
 * because the helper was right — the input was stale.
 *
 * So this drives the hook itself. `renderToStaticMarkup` runs the component body
 * synchronously, which is enough to obtain a real `run` from `useCallback`; the
 * sweep is then invoked directly with a stand-in loader that writes failures into
 * the real store, exactly as the real loader does.
 */

import { beforeEach, describe, expect, test } from 'bun:test';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { useQuotaStore } from '@/stores/useQuotaStore';
import { useQuotaScan, type QuotaScanOutcome } from '@/features/quota/hooks/useQuotaScan';
import { DEFAULT_QUOTA_SCAN_TUNING } from '@/features/quota/scanLogic';
import type { QuotaFileEntry } from '@/features/quota/logic';
import type { AuthFileItem, AntigravityQuotaState } from '@/types';
import type { TFunction } from 'i18next';

const entry = (name: string): QuotaFileEntry =>
  ({ file: { name, provider: 'antigravity' } as AuthFileItem, type: 'antigravity' }) as QuotaFileEntry;

const errorState = (message: string): AntigravityQuotaState => ({
  status: 'error',
  groups: [],
  error: message,
});

const successState = (): AntigravityQuotaState => ({ status: 'success', groups: [] });

const t = ((key: string) => key) as unknown as TFunction;

/** 安静且快速的节奏，测试不该等真实间隔。 */
const FAST = { ...DEFAULT_QUOTA_SCAN_TUNING, batchSize: 2, batchIntervalMs: 0, retryIntervalMs: 0 };

/**
 * 拿到一个真实 hook 实例。
 *
 * renderToStaticMarkup 同步执行组件体，所以 useCallback 返回的就是那个真实
 * 的 run。SSR 下 setState 是空操作，而 run 的返回值不依赖它们。
 */
function captureRun(loadQuota: (targets: QuotaFileEntry[]) => Promise<void>) {
  let captured: ((entries: QuotaFileEntry[], t: TFunction, tuning?: typeof FAST) => Promise<QuotaScanOutcome>) | null = null;
  const Probe = () => {
    const { run } = useQuotaScan(loadQuota);
    captured = run as typeof captured;
    return null;
  };
  renderToStaticMarkup(createElement(Probe));
  if (!captured) throw new Error('failed to capture run');
  return captured;
}

beforeEach(() => {
  useQuotaStore.getState().clearQuotaCache();
});

describe('run() failure detection', () => {
  test('reports failures the loader wrote during the sweep', async () => {
    // 巡检开始前 store 是空的 —— 旧实现在这个状态下会全程读空快照并报 0。
    const entries = [entry('a.json'), entry('b.json'), entry('c.json')];

    const loadQuota = async (targets: QuotaFileEntry[]) => {
      // 模拟真实 loader：把这一批的结果写进 store，成功失败都有。
      const writes: Record<string, AntigravityQuotaState> = {};
      for (const target of targets) {
        writes[target.file.name] =
          target.file.name === 'a.json' ? errorState('auth token refresh failed') : successState();
      }
      useQuotaStore.getState().setAntigravityQuota((prev) => ({ ...prev, ...writes }));
    };

    const run = captureRun(loadQuota);
    const outcome = await run(entries, t, FAST);

    expect(outcome.failures.map((f) => f.name)).toEqual(['a.json']);
    expect(outcome.failures[0].message).toBe('auth token refresh failed');
    expect(outcome.scanned).toBe(3);
    expect(outcome.cancelled).toBe(false);
  });

  test('reports every failure, not just the first batch', async () => {
    // 跨批次的可见性：批次 1 的失败必须在批次 2 落地后仍然被统计到。
    const entries = [
      entry('a.json'),
      entry('b.json'),
      entry('c.json'),
      entry('d.json'),
    ];

    const loadQuota = async (targets: QuotaFileEntry[]) => {
      const writes: Record<string, AntigravityQuotaState> = {};
      for (const target of targets) writes[target.file.name] = errorState('boom');
      useQuotaStore.getState().setAntigravityQuota((prev) => ({ ...prev, ...writes }));
    };

    const run = captureRun(loadQuota);
    const outcome = await run(entries, t, FAST);
    expect(outcome.failures).toHaveLength(4);
  });

  test('a failure cleared by a retry drops out of the result', async () => {
    // 重试的意义就在这里：判定必须跟着 store 的重写变化，否则重试等于白做。
    const entries = [entry('flaky.json')];
    let attempt = 0;

    const loadQuota = async (targets: QuotaFileEntry[]) => {
      const writes: Record<string, AntigravityQuotaState> = {};
      for (const target of targets) {
        attempt += 1;
        // 第一次失败，之后成功 —— 模拟限流后的恢复。
        writes[target.file.name] = attempt === 1 ? errorState('rate limited') : successState();
      }
      useQuotaStore.getState().setAntigravityQuota((prev) => ({ ...prev, ...writes }));
    };

    const run = captureRun(loadQuota);
    const outcome = await run(entries, t, FAST);

    expect(outcome.failures).toEqual([]);
  });

  test('a failure that survives every retry stays in the result', async () => {
    const entries = [entry('dead.json')];

    const loadQuota = async (targets: QuotaFileEntry[]) => {
      const writes: Record<string, AntigravityQuotaState> = {};
      for (const target of targets) writes[target.file.name] = errorState('auth token refresh failed');
      useQuotaStore.getState().setAntigravityQuota((prev) => ({ ...prev, ...writes }));
    };

    const run = captureRun(loadQuota);
    const outcome = await run(entries, t, FAST);

    expect(outcome.failures.map((f) => f.name)).toEqual(['dead.json']);
  });

  test('classifies from the credential list, not from the quota error', async () => {
    const entries = [
      { file: { name: 'revoked.json', provider: 'antigravity', selfTestVerdict: 'revoked' } as AuthFileItem, type: 'antigravity' as const },
      { file: { name: 'unknown.json', provider: 'antigravity' } as AuthFileItem, type: 'antigravity' as const },
    ];

    const loadQuota = async (targets: QuotaFileEntry[]) => {
      const writes: Record<string, AntigravityQuotaState> = {};
      for (const target of targets) writes[target.file.name] = errorState('auth token refresh failed');
      useQuotaStore.getState().setAntigravityQuota((prev) => ({ ...prev, ...writes }));
    };

    const run = captureRun(loadQuota);
    const outcome = await run(entries, t, FAST);

    const kinds = Object.fromEntries(outcome.failures.map((f) => [f.name, f.kind]));
    // 同一句配额报错，但结构化字段把它们分开了 —— 这是停用默认勾选范围的依据。
    expect(kinds).toEqual({ 'revoked.json': 'definitive', 'unknown.json': 'unconfirmed' });
  });

  test('reports resolved=scanned when every credential answered', async () => {
    const entries = [entry('a.json'), entry('b.json')];

    const loadQuota = async (targets: QuotaFileEntry[]) => {
      const writes: Record<string, AntigravityQuotaState> = {};
      for (const target of targets) writes[target.file.name] = successState();
      useQuotaStore.getState().setAntigravityQuota((prev) => ({ ...prev, ...writes }));
    };

    const run = captureRun(loadQuota);
    const outcome = await run(entries, t, FAST);
    expect(outcome.resolved).toBe(2);
  });

  test('reports resolved below scanned when a batch never produced a result', async () => {
    // 这是「0 个失败」和「根本没取到数」的分界。loadQuota 在自身忙时会直接
    // 返回：await 正常结束、store 没被写入、循环把它记成已完成。没有 resolved
    // 的话，一次实际什么都没做的巡检会报出「全部正常」。
    const entries = [entry('a.json'), entry('b.json'), entry('c.json'), entry('d.json')];
    let call = 0;

    const loadQuota = async (targets: QuotaFileEntry[]) => {
      call += 1;
      // 第一批正常写入，之后模拟被拒绝（什么都不做）。
      if (call > 1) return;
      const writes: Record<string, AntigravityQuotaState> = {};
      for (const target of targets) writes[target.file.name] = successState();
      useQuotaStore.getState().setAntigravityQuota((prev) => ({ ...prev, ...writes }));
    };

    const run = captureRun(loadQuota);
    const outcome = await run(entries, t, FAST);

    expect(outcome.failures).toEqual([]); // 没有失败
    expect(outcome.resolved).toBe(2); // 但只有 2 个真的取到了数
    expect(outcome.scanned).toBe(4);
    expect(outcome.resolved).toBeLessThan(outcome.scanned); // 界面据此报「未得出结论」
  });

  test('a still-loading credential does not count as resolved', async () => {
    const entries = [entry('a.json'), entry('b.json')];

    const loadQuota = async (targets: QuotaFileEntry[]) => {
      const writes: Record<string, AntigravityQuotaState> = {};
      for (const target of targets) {
        writes[target.file.name] = { status: 'loading', groups: [] };
      }
      useQuotaStore.getState().setAntigravityQuota((prev) => ({ ...prev, ...writes }));
    };

    const run = captureRun(loadQuota);
    const outcome = await run(entries, t, FAST);
    expect(outcome.resolved).toBe(0);
  });
});
