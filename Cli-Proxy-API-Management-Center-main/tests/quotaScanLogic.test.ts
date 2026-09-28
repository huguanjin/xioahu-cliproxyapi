import { afterAll, beforeEach, describe, expect, test } from 'bun:test';
import {
  buildScanFailures,
  chunkEntries,
  classifyScanFailure,
  defaultSelection,
  DEFAULT_QUOTA_SCAN_TUNING,
  QUOTA_SCAN_RESULT_VERSION,
  readQuotaScanResult,
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
