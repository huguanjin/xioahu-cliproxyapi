import { describe, expect, test } from 'bun:test';
import { getAuthFileProxyKey, getBoundProxyKeys } from '@/features/authFiles/constants';
import { isAuthFilesProxyFilter } from '@/features/authFiles/uiState';
import {
  buildProxyFilterMatcher,
  buildProxyFilterOptions,
  entryMatchesProxySearch,
  isResidentialProxyKey,
  sanitizeProxyFilterInput,
} from '@/utils/proxyFilter';
import type { AuthFileItem } from '@/types';

const f = (name: string, proxy?: string, extra: Record<string, unknown> = {}) =>
  ({ name, ...(proxy === undefined ? {} : { proxy_url: proxy }), ...extra }) as AuthFileItem;

describe('getAuthFileProxyKey', () => {
  test('reduces a proxy URL to host:port without scheme or credentials', () => {
    expect(getAuthFileProxyKey(f('a', 'socks5://user:pass@91.132.137.186:10003'))).toBe(
      '91.132.137.186:10003'
    );
    expect(getAuthFileProxyKey(f('a', 'http://1.2.3.4:8080/'))).toBe('1.2.3.4:8080');
  });

  test('accepts a bare address and keeps IPv6 brackets', () => {
    expect(getAuthFileProxyKey(f('a', 'u:p@1.2.3.4:1080'))).toBe('1.2.3.4:1080');
    expect(getAuthFileProxyKey(f('a', 'socks5://u:p@[2001:db8::1]:1080'))).toBe(
      '[2001:db8::1]:1080'
    );
  });

  test('returns empty for no proxy and never leaks the password', () => {
    expect(getAuthFileProxyKey(f('a'))).toBe('');
    expect(getAuthFileProxyKey(f('a', '  '))).toBe('');
    expect(getAuthFileProxyKey(f('a', 'socks5://user:s3cret@1.2.3.4:1'))).not.toContain('s3cret');
  });
});

describe('getBoundProxyKeys', () => {
  test('lists one key per bound credential, skipping unbound and runtime-only', () => {
    const files = [
      f('1', 'socks5://u:p@1.1.1.1:1'),
      f('2', 'socks5://other:x@1.1.1.1:1'),
      f('3'),
      f('4', 'socks5://u:p@3.3.3.3:3', { runtime_only: true }),
    ];
    expect(getBoundProxyKeys(files)).toEqual(['1.1.1.1:1', '1.1.1.1:1']);
  });
});

describe('buildProxyFilterOptions', () => {
  test('lists every pool proxy even when no credential in scope uses it', () => {
    // The reported bug: ten proxies in the pool, but the dropdown only had "all".
    const pool = ['1.1.1.1:1', '2.2.2.2:2', '3.3.3.3:3'];
    const options = buildProxyFilterOptions([], pool);
    expect(options.map((o) => o.key)).toEqual(['1.1.1.1:1', '2.2.2.2:2', '3.3.3.3:3']);
    expect(options.every((o) => o.count === 0)).toBe(true);
  });

  test('counts bound credentials, merges duplicates case-insensitively, sorts by count', () => {
    const options = buildProxyFilterOptions(
      ['b.example:2', 'B.EXAMPLE:2', 'a.example:1', 'c.example:3', 'c.example:3', 'c.example:3'],
      ['a.example:1', 'b.example:2', 'unused.example:9']
    );
    expect(options).toEqual([
      { key: 'c.example:3', count: 3 },
      { key: 'b.example:2', count: 2 },
      { key: 'a.example:1', count: 1 },
      { key: 'unused.example:9', count: 0 },
    ]);
  });

  test('includes proxies that only credentials know about', () => {
    expect(buildProxyFilterOptions(['9.9.9.9:9'], [])).toEqual([{ key: '9.9.9.9:9', count: 1 }]);
  });
});

describe('buildProxyFilterMatcher', () => {
  const known = new Set(['1.2.3.4:80', '1.2.3.4:8080', '91.132.137.186:10003']);

  test('empty text matches everything, including credentials without a proxy', () => {
    const m = buildProxyFilterMatcher('  ', known);
    expect(m('')).toBe(true);
    expect(m('1.2.3.4:80')).toBe(true);
  });

  test('an exact known host:port does not also match longer ports', () => {
    const m = buildProxyFilterMatcher('1.2.3.4:80', known);
    expect(m('1.2.3.4:80')).toBe(true);
    expect(m('1.2.3.4:8080')).toBe(false);
  });

  test('anything else is a case-insensitive substring match', () => {
    expect(buildProxyFilterMatcher('91.132', known)('91.132.137.186:10003')).toBe(true);
    expect(buildProxyFilterMatcher(':10003', known)('91.132.137.186:10003')).toBe(true);
    expect(buildProxyFilterMatcher('91.132', known)('1.2.3.4:80')).toBe(false);
  });

  test('a credential without a proxy never matches a non-empty filter', () => {
    expect(buildProxyFilterMatcher('1', known)('')).toBe(false);
  });

  test('a pasted full proxy URL filters by host:port, not by password', () => {
    const m = buildProxyFilterMatcher('socks5://user:s3cret@91.132.137.186:10003', known);
    expect(m('91.132.137.186:10003')).toBe(true);
    expect(m('1.2.3.4:80')).toBe(false);
  });
});

describe('sanitizeProxyFilterInput', () => {
  test('keeps ordinary typing untouched', () => {
    expect(sanitizeProxyFilterInput('91.132')).toBe('91.132');
    expect(sanitizeProxyFilterInput(':10003')).toBe(':10003');
  });

  test('strips scheme and credentials from a pasted address', () => {
    expect(sanitizeProxyFilterInput('socks5://u:p@1.2.3.4:1080/')).toBe('1.2.3.4:1080');
    expect(sanitizeProxyFilterInput('u:p@1.2.3.4:1080')).toBe('1.2.3.4:1080');
  });
});

describe('entryMatchesProxySearch', () => {
  const entry = {
    name: '加拿大 91.132.137.186:10003',
    'proxy-url': 'socks5://r1lK:s3cretPW@91.132.137.186:10003',
    account: 'acct-7',
    'exit-ip': '2001:db8::7',
    notes: 'backup line',
  };

  test('matches name, host:port, exit ip, account and notes', () => {
    for (const q of ['加拿大', '91.132', ':10003', '2001:db8', 'acct-7', 'BACKUP']) {
      expect(entryMatchesProxySearch(entry, q)).toBe(true);
    }
    expect(entryMatchesProxySearch(entry, '')).toBe(true);
    expect(entryMatchesProxySearch(entry, '8.8.8.8')).toBe(false);
  });

  test('does not match against the password inside the proxy URL', () => {
    expect(entryMatchesProxySearch(entry, 's3cretPW')).toBe(false);
    expect(entryMatchesProxySearch(entry, 'r1lK')).toBe(false);
  });
});

describe('isAuthFilesProxyFilter', () => {
  test('accepts host:port text, rejects credentials, empty and the legacy "all"', () => {
    expect(isAuthFilesProxyFilter('1.2.3.4:1080')).toBe(true);
    expect(isAuthFilesProxyFilter('91.132')).toBe(true);
    expect(isAuthFilesProxyFilter('all')).toBe(false);
    expect(isAuthFilesProxyFilter('u:p@1.2.3.4:1080')).toBe(false);
    expect(isAuthFilesProxyFilter('')).toBe(false);
    expect(isAuthFilesProxyFilter(5)).toBe(false);
  });
});

describe('isResidentialProxyKey', () => {
  const residential = new Set(['91.132.137.186:10003', '[2001:db8::1]:1080']);

  test('matches a proxy tagged as residential, case-insensitively', () => {
    expect(isResidentialProxyKey('91.132.137.186:10003', residential)).toBe(true);
    expect(isResidentialProxyKey('[2001:DB8::1]:1080', residential)).toBe(true);
  });

  test('does not match an untagged proxy, a proxy outside the pool, or no proxy', () => {
    expect(isResidentialProxyKey('1.2.3.4:80', residential)).toBe(false);
    expect(isResidentialProxyKey('', residential)).toBe(false);
    expect(isResidentialProxyKey('91.132.137.186:10003', new Set())).toBe(false);
  });

  test('the same gateway with another account still counts, because the key is host:port', () => {
    const a = getAuthFileProxyKey(f('a', 'socks5://one:pw@91.132.137.186:10003'));
    const b = getAuthFileProxyKey(f('b', 'socks5://two:pw@91.132.137.186:10003'));
    expect(isResidentialProxyKey(a, residential)).toBe(true);
    expect(isResidentialProxyKey(b, residential)).toBe(true);
  });
});
