import { describe, expect, test } from 'bun:test';
import {
  buildAvailabilityCounts,
  buildFamilyCounts,
  buildFamilyScopeCounts,
  classifyQuotaAvailability,
  collectRemainingPercents,
  familyOfGroupLabel,
  familyWeeklyStatus,
  filterEntriesByAvailability,
  filterEntriesByFamily,
  isQuotaAvailabilityFilter,
  isQuotaFamilyFilter,
  isQuotaFamilyScopeFilter,
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

describe('family filters', () => {
  const entry = (name: string): QuotaFileEntry =>
    ({ file: { name, provider: 'antigravity' } as AuthFileItem, type: 'antigravity' }) as QuotaFileEntry;

  const withGroups = (labels: string[]) => ({
    status: 'success',
    groups: labels.map((label) => ({ id: label, label, buckets: [] })),
  });

  const entries = [entry('both'), entry('gemini-only'), entry('claude-only'), entry('none')];
  const quotas: Record<string, { status?: string; groups?: { label?: string }[] }> = {
    both: withGroups(['gemini models', 'claude and gpt models']),
    'gemini-only': withGroups(['gemini models']),
    'claude-only': withGroups(['claude and gpt models']),
    none: { status: 'error' },
  };
  const quotaFor = (target: QuotaFileEntry) => quotas[target.file.name];

  test('all returns everything', () => {
    expect(filterEntriesByFamily(entries, 'all', quotaFor)).toHaveLength(4);
  });

  test('selects the credentials that have the family', () => {
    const names = (family: 'gemini' | 'claude') =>
      filterEntriesByFamily(entries, family, quotaFor).map((e) => e.file.name);

    expect(names('gemini')).toEqual(['both', 'gemini-only']);
    expect(names('claude')).toEqual(['both', 'claude-only']);
  });

  test('a credential with no loaded quota belongs to no family', () => {
    // 问「给我看 Gemini 的凭证」，返回一个 Gemini 状态未知的凭证是答非所问。
    expect(filterEntriesByFamily([entry('none')], 'gemini', quotaFor)).toHaveLength(0);
  });

  test('counts each family over the given list', () => {
    expect(buildFamilyCounts(entries, quotaFor)).toEqual({ all: 4, gemini: 2, claude: 2 });
  });

  test('maps upstream labels loosely, since they are free text', () => {
    // 分组名是自由文本；写死精确匹配会让上游改个名就整族消失 —— 而且是静默的。
    expect(familyOfGroupLabel('gemini models')).toBe('gemini');
    expect(familyOfGroupLabel('Gemini Models')).toBe('gemini');
    expect(familyOfGroupLabel('claude and gpt models')).toBe('claude');
    expect(familyOfGroupLabel('Claude & GPT')).toBe('claude');
    expect(familyOfGroupLabel('something else')).toBe('other');
    expect(familyOfGroupLabel(undefined)).toBe('other');
    expect(familyOfGroupLabel('')).toBe('other');
  });

  test('GPT is filed with Claude, not with Gemini', () => {
    // 上游把 gpt-oss 和 claude 放在同一个池子里，卡片标签也是这么写的。
    expect(familyOfGroupLabel('gpt-oss models')).toBe('claude');
  });

  test('rejects values outside the contract', () => {
    expect(isQuotaFamilyFilter('gemini')).toBe(true);
    expect(isQuotaFamilyFilter('all')).toBe(true);
    expect(isQuotaFamilyFilter('gpt')).toBe(false);
    expect(isQuotaFamilyFilter(undefined)).toBe(false);
  });
});

describe('family weekly scope filter', () => {
  const entry = (name: string): QuotaFileEntry =>
    ({ file: { name, provider: 'antigravity' } as AuthFileItem, type: 'antigravity' }) as QuotaFileEntry;

  const weekly = (fraction: number) => ({ window: 'weekly', periodHours: 168, remainingFraction: fraction });
  const fiveHour = (fraction: number) => ({ window: '5h', periodHours: 5, remainingFraction: fraction });

  const gemini = (buckets: unknown[]) => ({ label: 'gemini models', buckets });
  const claude = (buckets: unknown[]) => ({ label: 'claude and gpt models', buckets });

  const entries = [entry('full'), entry('gemini-dry'), entry('claude-dry'), entry('loading')];
  const quotas: Record<string, unknown> = {
    full: { status: 'success', groups: [gemini([weekly(1), fiveHour(1)]), claude([weekly(0.5), fiveHour(1)])] },
    'gemini-dry': { status: 'success', groups: [gemini([weekly(0), fiveHour(1)]), claude([weekly(0.9), fiveHour(1)])] },
    'claude-dry': { status: 'success', groups: [gemini([weekly(0.9), fiveHour(1)]), claude([weekly(0), fiveHour(1)])] },
    loading: { status: 'idle' },
  };
  const quotaFor = (target: QuotaFileEntry) => quotas[target.file.name] as never;

  const names = (family: 'gemini' | 'claude', scope: 'all' | 'weekly_available' | 'weekly_exhausted') =>
    filterEntriesByFamily(entries, family, quotaFor, scope).map((e) => e.file.name);

  test('the whole point: find credentials whose family weekly quota is NOT spent', () => {
    // 这是操作者要的列表 —— 还能拿这个家族干活的凭证。
    expect(names('gemini', 'weekly_available')).toEqual(['full', 'claude-dry']);
    expect(names('claude', 'weekly_available')).toEqual(['full', 'gemini-dry']);
  });

  test('the exhausted side excludes them', () => {
    expect(names('gemini', 'weekly_exhausted')).toEqual(['gemini-dry']);
    expect(names('claude', 'weekly_exhausted')).toEqual(['claude-dry']);
  });

  test('a spent 5-hour window does NOT count as the family being exhausted', () => {
    // 5 小时窗口几分钟就恢复了，把它当成「这个家族不可用」会藏掉马上就能用的
    // 凭证 —— 那正是这个筛选要避免的误导。
    const soon: Record<string, unknown> = {
      only5h: { status: 'success', groups: [gemini([weekly(1), fiveHour(0)])] },
    };
    const list = [entry('only5h')];
    expect(
      filterEntriesByFamily(list, 'gemini', (t) => soon[t.file.name] as never, 'weekly_available')
    ).toHaveLength(1);
    expect(
      filterEntriesByFamily(list, 'gemini', (t) => soon[t.file.name] as never, 'weekly_exhausted')
    ).toHaveLength(0);
  });

  test('an unreadable weekly status satisfies neither side', () => {
    // unknown 既不是「有额度」也不是「没额度」。当成有额度会把状态未知的凭证
    // 混进「可用」列表；当成没额度会把健康的标成耗尽。
    expect(names('gemini', 'weekly_available')).not.toContain('loading');
    expect(names('gemini', 'weekly_exhausted')).not.toContain('loading');
  });

  test('a family with several weekly buckets is exhausted if ANY is spent', () => {
    // 乐观方向在这里是错的方向。
    const multi: Record<string, unknown> = {
      multi: { status: 'success', groups: [gemini([weekly(1), weekly(0)])] },
    };
    const list = [entry('multi')];
    expect(
      filterEntriesByFamily(list, 'gemini', (t) => multi[t.file.name] as never, 'weekly_exhausted')
    ).toHaveLength(1);
  });

  test('scope has no effect when the family is 全部', () => {
    // 「全部家族」下这两个细分没有共同含义，所以不参与筛选。
    expect(filterEntriesByFamily(entries, 'all', quotaFor, 'weekly_exhausted')).toHaveLength(4);
  });

  test('counts each side of the split', () => {
    expect(buildFamilyScopeCounts(entries, quotaFor, 'gemini')).toEqual({
      available: 2,
      exhausted: 1,
    });
  });

  test('rejects values outside the scope contract', () => {
    expect(isQuotaFamilyScopeFilter('weekly_available')).toBe(true);
    expect(isQuotaFamilyScopeFilter('weekly_exhausted')).toBe(true);
    expect(isQuotaFamilyScopeFilter('available')).toBe(false);
    expect(isQuotaFamilyScopeFilter(undefined)).toBe(false);
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
