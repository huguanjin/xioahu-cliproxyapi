import { describe, expect, test } from 'bun:test';
import { collectAuthFileProxies, getAuthFileProxyKey } from '@/features/authFiles/constants';
import { isAuthFilesProxyFilter } from '@/features/authFiles/uiState';
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

  test('returns empty for no proxy', () => {
    expect(getAuthFileProxyKey(f('a'))).toBe('');
    expect(getAuthFileProxyKey(f('a', '  '))).toBe('');
  });

  test('never leaks the password', () => {
    expect(getAuthFileProxyKey(f('a', 'socks5://user:s3cret@1.2.3.4:1'))).not.toContain('s3cret');
  });
});

describe('collectAuthFileProxies', () => {
  test('counts per proxy, most used first, skipping runtime-only and unbound', () => {
    const files = [
      f('1', 'socks5://u:p@1.1.1.1:1'),
      f('2', 'socks5://other:x@1.1.1.1:1'),
      f('3', 'socks5://u:p@2.2.2.2:2'),
      f('4'),
      f('5', 'socks5://u:p@3.3.3.3:3', { runtime_only: true }),
    ];
    expect(collectAuthFileProxies(files)).toEqual([
      { key: '1.1.1.1:1', count: 2 },
      { key: '2.2.2.2:2', count: 1 },
    ]);
  });
});

describe('isAuthFilesProxyFilter', () => {
  test('accepts host:port and rejects anything carrying credentials', () => {
    expect(isAuthFilesProxyFilter('1.2.3.4:1080')).toBe(true);
    expect(isAuthFilesProxyFilter('u:p@1.2.3.4:1080')).toBe(false);
    expect(isAuthFilesProxyFilter('')).toBe(false);
    expect(isAuthFilesProxyFilter(5)).toBe(false);
  });
});
