import { describe, expect, test } from 'bun:test';
import { QUOTA_PAGE_SIZE } from '@/features/quota/constants';
import { buildWildcardSearch } from '@/features/authFiles/logic';
import {
  buildTabCounts,
  classifyQuotaFiles,
  filterEntriesBySearch,
  filterEntriesByTab,
  isQuotaRefreshDisabled,
  paginate,
  resolveQuotaProviderType,
  sortQuotaEntries,
  type QuotaFileEntry,
} from '@/features/quota/logic';
import type { AuthFileItem } from '@/types';

const file = (name: string, provider: string, extra: Partial<AuthFileItem> = {}): AuthFileItem =>
  ({ name, provider, ...extra }) as AuthFileItem;

const FILES: AuthFileItem[] = [
  file('codex-a.json', 'codex'),
  file('claude-a.json', 'claude'),
  file('kimi-a.json', 'kimi'),
  file('codex-b.json', 'codex'),
  file('grok-a.json', 'grok'), // 别名归一到 xai
  file('gemini-a.json', 'gemini'), // 不支持额度
  file('claude-off.json', 'claude', { disabled: true }), // 停用
];

describe('resolveQuotaProviderType', () => {
  test('maps provider aliases and rejects unsupported or disabled files', () => {
    expect(resolveQuotaProviderType(file('a', 'grok'))).toBe('xai');
    expect(resolveQuotaProviderType(file('a', 'antigravity'))).toBe('antigravity');
    expect(resolveQuotaProviderType(file('a', 'gemini'))).toBeNull();
    expect(resolveQuotaProviderType(file('a', 'claude', { disabled: true }))).toBeNull();
  });
});

describe('classifyQuotaFiles', () => {
  test('drops unsupported and disabled files', () => {
    const entries = classifyQuotaFiles(FILES);
    expect(entries.map((entry) => entry.file.name)).not.toContain('gemini-a.json');
    expect(entries.map((entry) => entry.file.name)).not.toContain('claude-off.json');
    expect(entries).toHaveLength(5);
  });

  test('orders entries by provider tab order', () => {
    const entries = classifyQuotaFiles(FILES);
    expect(entries.map((entry) => entry.type)).toEqual(['claude', 'codex', 'codex', 'xai', 'kimi']);
  });
});

describe('buildTabCounts', () => {
  test('counts per provider plus an all total, zero-filling empty tabs', () => {
    expect(buildTabCounts(classifyQuotaFiles(FILES))).toEqual({
      all: 5,
      claude: 1,
      antigravity: 0,
      codex: 2,
      xai: 1,
      kimi: 1,
    });
  });
});

describe('filterEntriesByTab', () => {
  const entries = classifyQuotaFiles(FILES);

  test("passes everything through on the 'all' tab", () => {
    expect(filterEntriesByTab(entries, 'all')).toHaveLength(5);
  });

  test('filters to a single provider', () => {
    expect(filterEntriesByTab(entries, 'codex').map((entry) => entry.file.name)).toEqual([
      'codex-a.json',
      'codex-b.json',
    ]);
    expect(filterEntriesByTab(entries, 'antigravity')).toEqual([]);
  });
});

describe('filterEntriesBySearch', () => {
  // Accounts are what the operator actually recognises, so the matcher has to
  // reach email — not just the file name shown on the card.
  const SEARCHABLE: AuthFileItem[] = [
    file('claude-1.json', 'claude', { email: 'alice@example.com' }),
    file('claude-2.json', 'claude', { email: 'bob@corp.io' }),
    file('codex-1.json', 'codex', { email: 'alice@corp.io' }),
  ];
  const entries = classifyQuotaFiles(SEARCHABLE);
  const byName = (list: QuotaFileEntry[]) => list.map((entry) => entry.file.name);

  const search = (term: string) =>
    filterEntriesBySearch(entries, term, buildWildcardSearch(term));

  test('returns the input untouched for an empty term', () => {
    expect(filterEntriesBySearch(entries, '', null)).toBe(entries);
  });

  test('matches a partial account name case-insensitively', () => {
    expect(byName(search('ALICE'))).toEqual(['claude-1.json', 'codex-1.json']);
  });

  test('matches on the file name too', () => {
    expect(byName(search('claude-2'))).toEqual(['claude-2.json']);
  });

  test('matches on the provider type', () => {
    expect(byName(search('codex'))).toEqual(['codex-1.json']);
  });

  test('supports * as a wildcard instead of a literal', () => {
    expect(byName(search('alice@*'))).toEqual(['claude-1.json', 'codex-1.json']);
    expect(byName(search('*corp.io'))).toEqual(['claude-2.json', 'codex-1.json']);
  });

  test('returns an empty list rather than throwing when nothing matches', () => {
    expect(search('nobody@nowhere')).toEqual([]);
  });

  test('preserves the incoming order', () => {
    expect(byName(search('.'))).toEqual(byName(entries));
  });
});

describe('isQuotaRefreshDisabled', () => {
  test('blocks a single-card refresh while the same quota is resetting', () => {
    expect(isQuotaRefreshDisabled(true, false, true)).toBe(true);
    expect(isQuotaRefreshDisabled(true, false, false)).toBe(false);
  });
});

describe('paginate', () => {
  const items = Array.from({ length: 45 }, (_, index) => index);

  test('uses the configured 20-item page size', () => {
    expect(QUOTA_PAGE_SIZE).toBe(20);
    expect(paginate(items, 2, QUOTA_PAGE_SIZE)).toEqual({
      pageItems: items.slice(20, 40),
      currentPage: 2,
      totalPages: 3,
    });
  });

  test('clamps an out-of-range page instead of returning an empty slice', () => {
    expect(paginate(items, 9, QUOTA_PAGE_SIZE).currentPage).toBe(3);
    expect(paginate(items, 9, QUOTA_PAGE_SIZE).pageItems).toEqual(items.slice(40));
    expect(paginate(items, 0, QUOTA_PAGE_SIZE).currentPage).toBe(1);
  });

  test('keeps at least one page when the list is empty', () => {
    expect(paginate([], 1, QUOTA_PAGE_SIZE)).toEqual({
      pageItems: [],
      currentPage: 1,
      totalPages: 1,
    });
  });
});

describe('sortQuotaEntries', () => {
  const entries = classifyQuotaFiles(FILES);
  const byName = (list: QuotaFileEntry[]) => list.map((entry) => entry.file.name);

  /** Recovery instants keyed by file name; anything absent resolves to null. */
  const resolver = (instants: Record<string, number>) => (entry: QuotaFileEntry) =>
    instants[entry.file.name] ?? null;

  test('default mode preserves order but returns a new array', () => {
    const sorted = sortQuotaEntries(entries, 'default', () => 1);
    expect(byName(sorted)).toEqual(byName(entries));
    expect(sorted).not.toBe(entries);
  });

  test('orders loaded credentials by how soon they recover, across providers', () => {
    const sorted = sortQuotaEntries(
      entries,
      'soonest',
      resolver({
        'codex-a.json': 300,
        'claude-a.json': 100,
        'kimi-a.json': 200,
        'codex-b.json': 400,
        'grok-a.json': 50,
      })
    );
    expect(byName(sorted)).toEqual([
      'grok-a.json',
      'claude-a.json',
      'kimi-a.json',
      'codex-a.json',
      'codex-b.json',
    ]);
  });

  test('sinks credentials with no instant, keeping their provider-grouped order', () => {
    // Loading is click-to-fetch, so an unloaded tail is the normal case.
    const sorted = sortQuotaEntries(
      entries,
      'soonest',
      resolver({ 'codex-b.json': 200, 'kimi-a.json': 100 })
    );
    expect(byName(sorted)).toEqual([
      'kimi-a.json',
      'codex-b.json',
      // unresolved tail, in the order classifyQuotaFiles produced
      'claude-a.json',
      'codex-a.json',
      'grok-a.json',
    ]);
  });

  test('leaves the order untouched when nothing has loaded', () => {
    expect(byName(sortQuotaEntries(entries, 'soonest', () => null))).toEqual(byName(entries));
  });

  test('breaks ties on the original position, so equal instants stay stable', () => {
    const sorted = sortQuotaEntries(entries, 'soonest', () => 500);
    expect(byName(sorted)).toEqual(byName(entries));
  });

  test('does not mutate the input', () => {
    const input = [...entries];
    sortQuotaEntries(input, 'soonest', resolver({ 'codex-b.json': 1 }));
    expect(input).toEqual(entries);
  });

  test('sorts before paginating, so the globally soonest lands on page one', () => {
    // Last in the default order, first to recover.
    const last = entries[entries.length - 1].file.name;
    const sorted = sortQuotaEntries(entries, 'soonest', resolver({ [last]: 1 }));
    expect(paginate(sorted, 1, 2).pageItems[0].file.name).toBe(last);
  });
});

describe('sortQuotaEntries — weekly mode', () => {
  const entries = classifyQuotaFiles(FILES);
  const byName = (list: QuotaFileEntry[]) => list.map((entry) => entry.file.name);
  const weekly = (values: Record<string, number>) => (entry: QuotaFileEntry) =>
    values[entry.file.name] ?? null;

  test('puts the most weekly quota first', () => {
    // 降序：这是「找配额充足的凭证」这个问题本身。
    const sorted = sortQuotaEntries(
      entries,
      'weekly',
      () => null,
      weekly({ 'claude-a.json': 10, 'codex-a.json': 90, 'kimi-a.json': 50 })
    );
    expect(byName(sorted).slice(0, 3)).toEqual([
      'codex-a.json',
      'kimi-a.json',
      'claude-a.json',
    ]);
  });

  test('a zero-weekly credential sinks below a full one', () => {
    const sorted = sortQuotaEntries(
      entries,
      'weekly',
      () => null,
      weekly({ 'claude-a.json': 100, 'codex-a.json': 0 })
    );
    expect(byName(sorted)[0]).toBe('claude-a.json');
    expect(byName(sorted)[1]).toBe('codex-a.json');
  });

  test('sinks credentials with no weekly reading, keeping their grouped order', () => {
    // null 是「未知」，不是「耗尽」。没查过额度的凭证不能被排成充足，
    // 也不能被排成耗尽 —— 沉底，并保持传入的分组顺序。
    const sorted = sortQuotaEntries(
      entries,
      'weekly',
      () => null,
      weekly({ 'codex-b.json': 20, 'kimi-a.json': 80 })
    );
    expect(byName(sorted).slice(0, 2)).toEqual(['kimi-a.json', 'codex-b.json']);
    expect(byName(sorted).slice(2)).toEqual([
      'claude-a.json',
      'codex-a.json',
      'grok-a.json',
    ]);
  });

  test('leaves the order untouched when nothing has loaded', () => {
    expect(byName(sortQuotaEntries(entries, 'weekly', () => null, () => null))).toEqual(
      byName(entries)
    );
  });

  test('breaks ties on the original position', () => {
    const sorted = sortQuotaEntries(entries, 'weekly', () => null, () => 50);
    expect(byName(sorted)).toEqual(byName(entries));
  });

  test('ignores the soonest resolver entirely', () => {
    // 两个键回答相反的问题，绝不能混：soonest 是「何时恢复」，weekly 是
    // 「还剩多少」。一个满额但 20 分钟后重置的凭证在前者里靠前、在后者里
    // 靠最后，两种排序对各自的问题都是对的。
    const sorted = sortQuotaEntries(
      entries,
      'weekly',
      () => 1,
      weekly({ 'claude-a.json': 100 })
    );
    expect(byName(sorted)[0]).toBe('claude-a.json');
  });

  test('does not mutate the input', () => {
    const input = [...entries];
    sortQuotaEntries(input, 'weekly', () => null, weekly({ 'codex-b.json': 1 }));
    expect(input).toEqual(entries);
  });

  test('sorts before paginating, so the fullest lands on page one', () => {
    const last = entries[entries.length - 1].file.name;
    const sorted = sortQuotaEntries(entries, 'weekly', () => null, weekly({ [last]: 100 }));
    expect(paginate(sorted, 1, 2).pageItems[0].file.name).toBe(last);
  });

  test('tolerates a missing weekly resolver', () => {
    // 排序键可选：其它 provider 没有周限额概念。
    expect(byName(sortQuotaEntries(entries, 'weekly', () => null))).toEqual(byName(entries));
  });
});
