import { describe, expect, test } from 'bun:test';
import {
  collectAuthFileStatusCodes,
  getAuthFileLastStatusCode,
} from '../src/features/authFiles/constants';
import { isAuthFilesStatusCodeFilter } from '../src/features/authFiles/uiState';
import type { AuthFileItem } from '../src/types';

const authFile = (overrides: Partial<AuthFileItem> = {}): AuthFileItem => ({
  name: 'credential.json',
  type: 'antigravity',
  ...overrides,
});

describe('auth file last status code', () => {
  test('reads the normalized numeric field', () => {
    expect(getAuthFileLastStatusCode(authFile({ lastStatusCode: 403 }))).toBe(403);
  });

  test('reads the raw backend field when normalization did not run', () => {
    expect(getAuthFileLastStatusCode(authFile({ last_status_code: 403 }))).toBe(403);
  });

  test('accepts a numeric string', () => {
    expect(getAuthFileLastStatusCode(authFile({ last_status_code: ' 429 ' }))).toBe(429);
  });

  test('returns undefined when the credential never failed', () => {
    expect(getAuthFileLastStatusCode(authFile())).toBeUndefined();
  });

  test('rejects values outside the HTTP range', () => {
    expect(getAuthFileLastStatusCode(authFile({ lastStatusCode: 0 }))).toBeUndefined();
    expect(getAuthFileLastStatusCode(authFile({ lastStatusCode: 99 }))).toBeUndefined();
    expect(getAuthFileLastStatusCode(authFile({ lastStatusCode: 600 }))).toBeUndefined();
  });

  test('rejects non numeric values instead of producing NaN', () => {
    expect(getAuthFileLastStatusCode(authFile({ last_status_code: 'abc' }))).toBeUndefined();
    expect(getAuthFileLastStatusCode(authFile({ last_status_code: '' }))).toBeUndefined();
    expect(getAuthFileLastStatusCode(authFile({ last_status_code: null }))).toBeUndefined();
    expect(getAuthFileLastStatusCode(authFile({ lastStatusCode: Number.NaN }))).toBeUndefined();
    expect(getAuthFileLastStatusCode(authFile({ lastStatusCode: 403.5 }))).toBeUndefined();
  });
});

describe('collect auth file status codes', () => {
  test('returns an empty list when nothing has a status code', () => {
    expect(collectAuthFileStatusCodes([authFile(), authFile({ name: 'other.json' })])).toEqual([]);
  });

  test('dedupes and sorts ascending', () => {
    const codes = collectAuthFileStatusCodes([
      authFile({ lastStatusCode: 429 }),
      authFile({ lastStatusCode: 403 }),
      authFile({ last_status_code: '403' }),
      authFile({ lastStatusCode: 401 }),
      authFile(),
    ]);
    expect(codes).toEqual([401, 403, 429]);
  });

  test('never lets a missing field pollute the options with NaN', () => {
    const codes = collectAuthFileStatusCodes([
      authFile(),
      authFile({ last_status_code: 'not-a-code' }),
      authFile({ lastStatusCode: 403 }),
    ]);
    expect(codes).toEqual([403]);
    codes.forEach((code) => expect(Number.isNaN(code)).toBe(false));
  });
});

describe('persisted status code filter guard', () => {
  test('accepts all and plain numeric strings', () => {
    expect(isAuthFilesStatusCodeFilter('all')).toBe(true);
    expect(isAuthFilesStatusCodeFilter('403')).toBe(true);
  });

  test('rejects anything else from a stale storage blob', () => {
    expect(isAuthFilesStatusCodeFilter(undefined)).toBe(false);
    expect(isAuthFilesStatusCodeFilter(null)).toBe(false);
    expect(isAuthFilesStatusCodeFilter(403)).toBe(false);
    expect(isAuthFilesStatusCodeFilter('403abc')).toBe(false);
    expect(isAuthFilesStatusCodeFilter('4 03')).toBe(false);
  });
});
