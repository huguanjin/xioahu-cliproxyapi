/**
 * 一键巡检的失败判定必须读「当时」的额度状态，而不是点击那一刻的渲染快照。
 *
 * The sweep runs for minutes. During it the store is written to continuously and
 * the component re-renders, but the async function that started on the button
 * press keeps the reference it closed over — which is the store as it was before
 * the first batch landed. Reading through that stale reference reports zero
 * failures no matter how many there actually were, while the cards, the filter
 * counts and the store all show the truth.
 *
 * These tests drive the real store and mutate it mid-flight, so a regression
 * shows up as a wrong classification rather than as a wrong reference.
 */

import { beforeEach, describe, expect, test } from 'bun:test';
import { useQuotaStore } from '@/stores/useQuotaStore';
import { classifyQuotaAvailability } from '@/features/quota/availability';
import { QUOTA_ADAPTERS } from '@/features/quota/providers';
import type { QuotaFileEntry } from '@/features/quota/logic';
import type { AuthFileItem, AntigravityQuotaState } from '@/types';

const entry = (name: string): QuotaFileEntry =>
  ({ file: { name } as AuthFileItem, type: 'antigravity' }) as QuotaFileEntry;

/** 模拟 QuotaPage 的 getQuota：读渲染时捕获的快照。 */
const snapshotReader = (snapshot: Record<string, AntigravityQuotaState>) =>
  (target: QuotaFileEntry) => snapshot[target.file.name];

/** 修复后应当使用的方式：直读 store，不订阅。 */
const liveReader = (target: QuotaFileEntry): { status?: string } | undefined =>
  QUOTA_ADAPTERS[target.type].storeSelector(
    useQuotaStore.getState() as never
  )[target.file.name];

const errorState = (): AntigravityQuotaState => ({
  status: 'error',
  groups: [],
  error: 'auth token refresh failed',
});

const successState = (): AntigravityQuotaState => ({
  status: 'success',
  groups: [],
});

beforeEach(() => {
  useQuotaStore.getState().clearQuotaCache();
});

describe('stale vs live quota reads', () => {
  test('a snapshot taken before the sweep reports no failures', () => {
    // 这就是线上看到的 0：点击那一刻 store 还是空的，判定全程读那份空快照，
    // 尽管 store 早已被写入真实的失败状态。
    const entries = [entry('a.json'), entry('b.json')];
    const snapshotBefore = {};
    const staleReader = snapshotReader(snapshotBefore as never);

    // 巡检开始后才落地的结果
    useQuotaStore.getState().setAntigravityQuota({ 'a.json': errorState(), 'b.json': successState() });

    const viaStale = entries.filter(
      (target) => classifyQuotaAvailability('antigravity', staleReader(target)) === 'error'
    );
    expect(viaStale).toHaveLength(0); // 旧快照：全部漏判

    const viaLive = entries.filter((target) => {
      const state = liveReader(target);
      return state?.status === 'error';
    });
    expect(viaLive.map((target) => target.file.name)).toEqual(['a.json']); // 直读：判对了
  });

  test('the live reader sees writes that land after it was created', () => {
    // 关键性质：reader 本身不携带快照，所以「创建时机」不影响它读到的内容。
    const reader = liveReader;
    useQuotaStore.getState().setAntigravityQuota({ 'late.json': errorState() });
    expect(reader(entry('late.json'))?.status).toBe('error');
  });

  test('the live reader tracks later overwrites of the same credential', () => {
    useQuotaStore.getState().setAntigravityQuota({ 'a.json': errorState() });
    expect(liveReader(entry('a.json'))?.status).toBe('error');

    // 重试把同一个凭证改写成成功 —— 判定必须跟着变，否则重试等于白做。
    useQuotaStore.getState().setAntigravityQuota((prev) => ({
      ...prev,
      'a.json': successState(),
    }));
    expect(liveReader(entry('a.json'))?.status).toBe('success');
  });

  test('the live reader sees the whole map, not just the first write', () => {
    const entries = [entry('a.json'), entry('b.json'), entry('c.json')];
    useQuotaStore.getState().setAntigravityQuota({ 'a.json': successState() });
    useQuotaStore.getState().setAntigravityQuota((prev) => ({ ...prev, 'b.json': errorState() }));
    useQuotaStore.getState().setAntigravityQuota((prev) => ({ ...prev, 'c.json': errorState() }));

    const failures = entries.filter((target) => liveReader(target)?.status === 'error');
    expect(failures.map((target) => target.file.name)).toEqual(['b.json', 'c.json']);
  });

  test('a cleared cache reads as unloaded rather than as a stale failure', () => {
    useQuotaStore.getState().setAntigravityQuota({ 'a.json': errorState() });
    expect(liveReader(entry('a.json'))?.status).toBe('error');

    // clearQuotaCache 会递增 generation 并清空 —— 直读自然跟着清空。
    useQuotaStore.getState().clearQuotaCache();
    expect(liveReader(entry('a.json'))).toBeUndefined();
  });
});
