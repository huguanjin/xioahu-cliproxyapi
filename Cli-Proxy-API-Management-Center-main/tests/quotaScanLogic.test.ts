import { afterAll, beforeEach, describe, expect, test } from 'bun:test';
import {
  buildScanFailures,
  chunkEntries,
  clampScanRetries,
  classifyScanFailure,
  defaultSelection,
  DEFAULT_QUOTA_SCAN_TUNING,
  MAX_QUOTA_SCAN_RETRIES,
  MIN_QUOTA_SCAN_RETRIES,
  QUOTA_SCAN_RESULT_VERSION,
  readQuotaScanResult,
  retryPhasePercent,
  shouldShowRetryProgress,
  writeQuotaScanResult,
  type QuotaScanFailure,
} from '@/features/quota/scanLogic';
import type { AuthFileItem } from '@/types';

const file = (name: string, extra: Partial<AuthFileItem> = {}): AuthFileItem =>
  ({ name, ...extra }) as AuthFileItem;

describe('chunkEntries', () => {
  test('splits into full batches with a short tail', () => {
    expect(chunkEntries([1, 2, 3, 4, 5], 2)).toEqual([[1, 2], [3, 4], [5]]);
  });

  test('returns nothing for an empty list', () => {
    expect(chunkEntries([], 20)).toEqual([]);
  });

  test('returns one full batch when the list fits', () => {
    expect(chunkEntries([1, 2], 20)).toEqual([[1, 2]]);
  });

  test('does not drop items when the size does not divide the list', () => {
    const items = Array.from({ length: 1690 }, (_, index) => index);
    const chunks = chunkEntries(items, 20);
    expect(chunks.flat()).toHaveLength(1690);
    expect(chunks).toHaveLength(85); // 84 full batches plus a 10-item tail
  });

  test('falls back to a single batch for a nonsensical size', () => {
    // A zero or negative batch size must not produce an infinite loop or an
    // empty result that would silently skip every credential.
    for (const size of [0, -1, Number.NaN]) {
      expect(chunkEntries([1, 2, 3], size)).toEqual([[1, 2, 3]]);
    }
  });
});

describe('classifyScanFailure', () => {
  test('calls a credential_invalid code definitive', () => {
    expect(classifyScanFailure(file('a.json', { lastErrorCode: 'credential_invalid' }))).toBe(
      'definitive'
    );
  });

  test('calls a revoked self-test verdict definitive', () => {
    expect(classifyScanFailure(file('a.json', { selfTestVerdict: 'revoked' }))).toBe('definitive');
  });

  test('calls everything else unconfirmed', () => {
    // This is the safety property: a quota failure on its own is not evidence of
    // a dead credential — the backend flattens a transient 503 and a permanent
    // invalid_grant into the same string, so it cannot be treated as definitive.
    expect(classifyScanFailure(file('a.json'))).toBe('unconfirmed');
    expect(classifyScanFailure(file('a.json', { selfTestVerdict: 'cooling' }))).toBe('unconfirmed');
    expect(classifyScanFailure(file('a.json', { selfTestVerdict: 'validation' }))).toBe(
      'unconfirmed'
    );
    expect(classifyScanFailure(file('a.json', { lastErrorCode: 'rate_limited' }))).toBe(
      'unconfirmed'
    );
  });

  test('treats an unknown credential as unconfirmed, not stoppable', () => {
    // A missing record must not default to "safe to disable": a stale list would
    // then disable credentials nothing had actually judged.
    expect(classifyScanFailure(undefined)).toBe('unconfirmed');
  });

  test('ignores blank and whitespace-only codes', () => {
    expect(classifyScanFailure(file('a.json', { lastErrorCode: '  ' }))).toBe('unconfirmed');
    expect(classifyScanFailure(file('a.json', { selfTestVerdict: '' }))).toBe('unconfirmed');
  });
});

describe('defaultSelection', () => {
  const failures: QuotaScanFailure[] = [
    { name: 'dead.json', kind: 'definitive', message: 'auth token refresh failed' },
    { name: 'maybe.json', kind: 'unconfirmed', message: 'auth token refresh failed' },
    { name: 'verify.json', kind: 'unconfirmed', message: '请检查凭证状态' },
  ];

  test('pre-selects only the definitively dead', () => {
    // Disabling is the irreversible half of this feature, so the default errs
    // toward leaving credentials alone.
    expect(defaultSelection(failures)).toEqual(new Set(['dead.json']));
  });

  test('selects nothing when nothing is definitive', () => {
    expect(
      defaultSelection([{ name: 'a.json', kind: 'unconfirmed', message: 'x' }])
    ).toEqual(new Set());
  });
});

describe('buildScanFailures', () => {
  test('classifies each name from the credential list', () => {
    const files = new Map<string, AuthFileItem>([
      ['dead.json', file('dead.json', { selfTestVerdict: 'revoked' })],
      ['ok.json', file('ok.json')],
    ]);
    const failures = buildScanFailures(
      ['dead.json', 'ok.json', 'gone.json'],
      () => 'auth token refresh failed',
      (name) => files.get(name)
    );

    expect(failures.map((f) => [f.name, f.kind])).toEqual([
      ['dead.json', 'definitive'],
      ['ok.json', 'unconfirmed'],
      ['gone.json', 'unconfirmed'],
    ]);
    expect(failures[0].message).toBe('auth token refresh failed');
  });
});

describe('scan tuning defaults', () => {
  test('is bounded on both axes', () => {
    // The sweep hits a real upstream ~1690 times; both a batch size and a gap
    // must exist or a single press becomes a self-inflicted rate limit.
    expect(DEFAULT_QUOTA_SCAN_TUNING.batchSize).toBeGreaterThan(0);
    expect(DEFAULT_QUOTA_SCAN_TUNING.batchIntervalMs).toBeGreaterThan(0);
    expect(DEFAULT_QUOTA_SCAN_TUNING.retryIntervalMs).toBeGreaterThan(0);
  });
});

describe('clampScanRetries', () => {
  test('accepts zero — not retrying is a real choice', () => {
    // 操作者已知上游在限流时，想要的就是首轮原始结果，而不是一个更慢的版本。
    expect(clampScanRetries(0)).toBe(0);
    expect(clampScanRetries('0')).toBe(0);
  });

  test('accepts values in range, from numbers and strings', () => {
    expect(clampScanRetries(3)).toBe(3);
    expect(clampScanRetries('3')).toBe(3);
    expect(clampScanRetries(MAX_QUOTA_SCAN_RETRIES)).toBe(MAX_QUOTA_SCAN_RETRIES);
  });

  test('clamps past the maximum', () => {
    // 未限幅意味着对 1690 个账号重放几十轮。
    expect(clampScanRetries(MAX_QUOTA_SCAN_RETRIES + 5)).toBe(MAX_QUOTA_SCAN_RETRIES);
    expect(clampScanRetries(9999)).toBe(MAX_QUOTA_SCAN_RETRIES);
  });

  test('clamps negatives up to zero rather than letting them skip silently', () => {
    // 负数会让重试循环直接不执行 —— 行为上等于 0，但界面显示 -1 会是谎话。
    expect(clampScanRetries(-1)).toBe(0);
    expect(clampScanRetries(-100)).toBe(0);
  });

  test('truncates fractions so the round counter stays honest', () => {
    expect(clampScanRetries(2.7)).toBe(2);
    expect(clampScanRetries('2.9')).toBe(2);
  });

  test('falls back to the default for input that is not a number', () => {
    // 输入框可以被清空或填成任意文本，这些都不该变成「0 次重试」的静默决定。
    for (const bad of ['', '   ', 'abc', null, undefined, {}, Number.NaN]) {
      expect(clampScanRetries(bad)).toBe(DEFAULT_QUOTA_SCAN_TUNING.maxRetries);
    }
  });

  test('never returns a value outside the documented range', () => {
    const inputs = [-5, -0.5, 0, 0.5, 1, 2, 9, 10, 11, 1e6, 'x', '', null];
    for (const input of inputs) {
      const got = clampScanRetries(input);
      expect(got).toBeGreaterThanOrEqual(MIN_QUOTA_SCAN_RETRIES);
      expect(got).toBeLessThanOrEqual(MAX_QUOTA_SCAN_RETRIES);
      expect(Number.isInteger(got)).toBe(true);
    }
  });
});

describe('retryPhasePercent', () => {
  test('reports this round progress, not a cumulative one', () => {
    // Given a round of 10 with 4 done, the bar reads 40% — never "rounds so far".
    expect(retryPhasePercent({ total: 10, completed: 4, round: 1, rounds: 2 })).toBe(40);
    expect(retryPhasePercent({ total: 10, completed: 10, round: 2, rounds: 2 })).toBe(100);
  });

  test('a later round with fewer failures restarts from this round own size', () => {
    // 第二轮只有 3 个要补（第一轮救回了 7 个），分母随之变成 3 —— 这正是
    // 不能合并成总进度条的原因。
    expect(retryPhasePercent({ total: 3, completed: 1, round: 2, rounds: 3 })).toBe(33);
  });

  test('returns null when retries are disabled', () => {
    // 0 次重试时不该画一个 0% 的条 —— 那读起来像卡住，而不是「不用重试」。
    expect(retryPhasePercent({ total: 0, completed: 0, round: 0, rounds: 0 })).toBeNull();
  });

  test('returns null before the retry phase starts', () => {
    expect(retryPhasePercent({ total: 5, completed: 0, round: 0, rounds: 2 })).toBeNull();
  });

  test('returns null when there is nothing to retry', () => {
    // 首轮全成功：没有失败项，重试阶段无事可做。
    expect(retryPhasePercent({ total: 0, completed: 0, round: 1, rounds: 2 })).toBeNull();
  });

  test('clamps into 0..100 even if the counters overshoot', () => {
    expect(retryPhasePercent({ total: 4, completed: 9, round: 1, rounds: 2 })).toBe(100);
    expect(retryPhasePercent({ total: 4, completed: -3, round: 1, rounds: 2 })).toBe(0);
  });
});

describe('shouldShowRetryProgress', () => {
  test('agrees with retryPhasePercent', () => {
    expect(shouldShowRetryProgress({ total: 5, completed: 1, round: 1, rounds: 2 })).toBe(true);
    expect(shouldShowRetryProgress({ total: 0, completed: 0, round: 0, rounds: 0 })).toBe(false);
  });
});

describe('scan result persistence', () => {
  const originalWindow = (globalThis as { window?: unknown }).window;

  beforeEach(() => {
    const store = new Map<string, string>();
    (globalThis as unknown as { window: unknown }).window = {
      sessionStorage: {
        getItem: (key: string) => store.get(key) ?? null,
        setItem: (key: string, value: string) => void store.set(key, value),
        removeItem: (key: string) => void store.delete(key),
      },
    };
  });

  afterAll(() => {
    if (originalWindow === undefined) {
      delete (globalThis as { window?: unknown }).window;
    } else {
      (globalThis as { window?: unknown }).window = originalWindow;
    }
  });

  const result = () => ({
    version: QUOTA_SCAN_RESULT_VERSION,
    finishedAt: '2026-09-27T12:00:00Z',
    scanned: 1690,
    failed: 1,
    failures: [{ name: 'a.json', kind: 'definitive' as const, message: 'x' }],
    resolved: 1690,
  });

  test('round-trips a result', () => {
    writeQuotaScanResult(result());
    expect(readQuotaScanResult()).toEqual(result());
  });

  test('clears on null', () => {
    writeQuotaScanResult(result());
    writeQuotaScanResult(null);
    expect(readQuotaScanResult()).toBeNull();
  });

  test('survives absent and malformed payloads', () => {
    expect(readQuotaScanResult()).toBeNull();
    window.sessionStorage.setItem('quotaPage.scanResult', '{not json');
    expect(readQuotaScanResult()).toBeNull();
    window.sessionStorage.setItem('quotaPage.scanResult', '{"failures":"nope"}');
    expect(readQuotaScanResult()).toBeNull();
  });

  test('discards a result written by a different version', () => {
    // 关键安全性：旧版本写下的「全部正常」不能被当成有效结论展示。
    // 上一版的判定恒报 0 个失败，它的结果等于一次假体检。
    const stale = { ...result(), version: QUOTA_SCAN_RESULT_VERSION - 1 };
    window.sessionStorage.setItem('quotaPage.scanResult', JSON.stringify(stale));

    expect(readQuotaScanResult()).toBeNull();
    // 还要把垃圾清掉，否则每次打开都要重新判一遍。
    expect(window.sessionStorage.getItem('quotaPage.scanResult')).toBeNull();
  });

  test('discards a versionless result from before versioning existed', () => {
    const { version: _drop, ...noVersion } = result();
    window.sessionStorage.setItem('quotaPage.scanResult', JSON.stringify(noVersion));
    expect(readQuotaScanResult()).toBeNull();
  });

  test('a result with zero failures is still valid when it is current', () => {
    // 「0 个失败」不是可疑信号本身 —— 造假的是旧版本，不是这个数字。
    const clean = { ...result(), failed: 0, failures: [] };
    writeQuotaScanResult(clean);
    expect(readQuotaScanResult()).toEqual(clean);
  });
});
