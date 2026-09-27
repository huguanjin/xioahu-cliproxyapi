import { describe, expect, test } from 'bun:test';
import {
  buildAvailabilityCounts,
  classifyQuotaAvailability,
  collectRemainingPercents,
  filterEntriesByAvailability,
  isQuotaAvailabilityFilter,
  type QuotaAvailability,
} from '@/features/quota/availability';
import type { QuotaFileEntry } from '@/features/quota/logic';
import type { AuthFileItem } from '@/types';

const entry = (name: string, type: QuotaFileEntry['type']): QuotaFileEntry =>
  ({ file: { name } as AuthFileItem, type }) as QuotaFileEntry;

const success = (extra: Record<string, unknown>) => ({ status: 'success', ...extra });

describe('collectRemainingPercents', () => {
  test('reads claude and codex percent-used as remaining', () => {
    expect(
      collectRemainingPercents('claude', success({ windows: [{ usedPercent: 100 }, { usedPercent: 40 }] }))
    ).toEqual([0, 60]);
    expect(
      collectRemainingPercents('codex', success({ windows: [{ usedPercent: 25 }] }))
    ).toEqual([75]);
  });

  test('reads antigravity remaining fraction, not percent used', () => {
    // Getting this backwards would report a spent credential as full.
    const quota = success({
      groups: [{ buckets: [{ remainingFraction: 1 }, { remainingFraction: 0.02 }] }],
    });
    expect(collectRemainingPercents('antigravity', quota)).toEqual([100, 2]);
  });

  test('derives kimi remaining from raw counts', () => {
    expect(
      collectRemainingPercents('kimi', success({ rows: [{ used: 30, limit: 100 }] }))
    ).toEqual([70]);
  });

  test('reads xai percent-used as remaining', () => {
    expect(collectRemainingPercents('xai', success({ billing: { usagePercent: 10 } }))).toEqual([
      90,
    ]);
  });

  test('skips unreadable windows instead of scoring them as spent', () => {
    // A missing figure is unknown. Counting it as 0 would report a credential
    // with one healthy window as exhausted.
    const quota = success({ windows: [{ usedPercent: null }, { usedPercent: 50 }] });
    expect(collectRemainingPercents('claude', quota)).toEqual([50]);
  });

  test('clamps out-of-range values into 0..100', () => {
    expect(
      collectRemainingPercents('claude', success({ windows: [{ usedPercent: 120 }] }))
    ).toEqual([0]);
    expect(
      collectRemainingPercents('antigravity', success({ groups: [{ buckets: [{ remainingFraction: 1.5 }] }] }))
    ).toEqual([100]);
  });

  test('ignores kimi rows with no positive limit', () => {
    expect(
      collectRemainingPercents('kimi', success({ rows: [{ used: 5, limit: 0 }] }))
    ).toEqual([]);
  });

  test('returns nothing unless the state is success', () => {
    for (const status of ['idle', 'loading', 'error']) {
      expect(collectRemainingPercents('claude', { status, windows: [{ usedPercent: 0 }] })).toEqual(
        []
      );
    }
    expect(collectRemainingPercents('claude', undefined)).toEqual([]);
  });
});

describe('classifyQuotaAvailability', () => {
  test('reports available when any window still has capacity', () => {
    // Windows are independent limits, not one pooled budget, so one empty
    // window must not mark the credential unusable.
    const quota = success({ windows: [{ usedPercent: 100 }, { usedPercent: 99 }] });
    expect(classifyQuotaAvailability('claude', quota)).toBe<QuotaAvailability>('available');
  });

  test('reports exhausted only when every readable window is at zero', () => {
    const quota = success({ windows: [{ usedPercent: 100 }, { usedPercent: 100 }] });
    expect(classifyQuotaAvailability('claude', quota)).toBe<QuotaAvailability>('exhausted');
  });

  test('reports failed when the fetch itself failed', () => {
    expect(classifyQuotaAvailability('claude', { status: 'error' })).toBe<QuotaAvailability>(
      'failed'
    );
  });

  test('reports unloaded before anyone has asked', () => {
    expect(classifyQuotaAvailability('claude', undefined)).toBe<QuotaAvailability>('unloaded');
    expect(classifyQuotaAvailability('claude', { status: 'idle' })).toBe<QuotaAvailability>(
      'unloaded'
    );
    expect(classifyQuotaAvailability('claude', { status: 'loading' })).toBe<QuotaAvailability>(
      'unloaded'
    );
  });

  test('treats a success payload with no readable window as unloaded, not exhausted', () => {
    // "I could not read a capacity figure" is an unknown. Claiming exhausted
    // would tell the user to discard a credential nobody actually measured.
    expect(
      classifyQuotaAvailability('claude', success({ windows: [{ usedPercent: null }] }))
    ).toBe<QuotaAvailability>('unloaded');
    expect(classifyQuotaAvailability('claude', success({}))).toBe<QuotaAvailability>('unloaded');
  });

  test('separates a broken credential from a spent one', () => {
    // The whole point of the filter: these two want opposite actions.
    expect(classifyQuotaAvailability('claude', { status: 'error' })).not.toBe<QuotaAvailability>(
      'exhausted'
    );
  });
});

describe('filterEntriesByAvailability', () => {
  const entries = [
    entry('a', 'claude'),
    entry('b', 'claude'),
    entry('c', 'claude'),
    entry('d', 'claude'),
  ];
  const states: Record<string, { status?: string }> = {
    a: success({ windows: [{ usedPercent: 10 }] }),
    b: success({ windows: [{ usedPercent: 100 }] }),
    c: { status: 'error' },
    // d stays undefined: never fetched
  };
  const quotaFor = (target: QuotaFileEntry) => states[target.file.name];

  test('all returns everything', () => {
    expect(filterEntriesByAvailability(entries, 'all', quotaFor)).toHaveLength(4);
  });

  test('each bucket selects exactly its own credentials', () => {
    const names = (filter: Parameters<typeof filterEntriesByAvailability>[1]) =>
      filterEntriesByAvailability(entries, filter, quotaFor).map((e) => e.file.name);

    expect(names('available')).toEqual(['a']);
    expect(names('exhausted')).toEqual(['b']);
    expect(names('failed')).toEqual(['c']);
    expect(names('unloaded')).toEqual(['d']);
  });

  test('the buckets partition the list', () => {
    const total = (['available', 'exhausted', 'failed', 'unloaded'] as const)
      .map((filter) => filterEntriesByAvailability(entries, filter, quotaFor).length)
      .reduce((sum, count) => sum + count, 0);
    expect(total).toBe(entries.length);
  });
});

describe('buildAvailabilityCounts', () => {
  test('counts each bucket and sizes all to the given list', () => {
    const entries = [entry('a', 'claude'), entry('b', 'claude'), entry('c', 'claude')];
    const states: Record<string, { status?: string }> = {
      a: success({ windows: [{ usedPercent: 10 }] }),
      b: { status: 'error' },
    };
    const counts = buildAvailabilityCounts(entries, (target) => states[target.file.name]);

    expect(counts).toEqual({ all: 3, available: 1, exhausted: 0, failed: 1, unloaded: 1 });
  });

  test('counts only what it was given, so it matches the visible list', () => {
    expect(buildAvailabilityCounts([], () => undefined)).toEqual({
      all: 0,
      available: 0,
      exhausted: 0,
      failed: 0,
      unloaded: 0,
    });
  });
});

describe('isQuotaAvailabilityFilter', () => {
  test('accepts the known filters and rejects anything else', () => {
    expect(isQuotaAvailabilityFilter('available')).toBe(true);
    expect(isQuotaAvailabilityFilter('all')).toBe(true);
    expect(isQuotaAvailabilityFilter('healthy')).toBe(false);
    expect(isQuotaAvailabilityFilter(undefined)).toBe(false);
  });
});
