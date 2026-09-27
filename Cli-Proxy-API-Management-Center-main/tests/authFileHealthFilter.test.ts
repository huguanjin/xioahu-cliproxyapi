import { describe, expect, test } from 'bun:test';
import {
  isAuthFileKnownBad,
  isAuthFileKnownHealthy,
  isAuthFileUntested,
  isProblemAuthFile,
} from '../src/features/authFiles/constants';
import type { AuthFileItem } from '../src/types';

const authFile = (overrides: Partial<AuthFileItem> = {}): AuthFileItem => ({
  name: 'credential.json',
  type: 'antigravity',
  ...overrides,
});

describe('auth file health classification', () => {
  test('only a healthy verdict counts as callable', () => {
    expect(isAuthFileKnownHealthy(authFile({ selfTestVerdict: 'healthy' }))).toBe(true);
    for (const verdict of ['cooling', 'quarantine', 'validation'] as const) {
      expect(isAuthFileKnownHealthy(authFile({ selfTestVerdict: verdict }))).toBe(false);
    }
  });

  test('every non-healthy verdict counts as not callable', () => {
    for (const verdict of ['cooling', 'quarantine', 'validation'] as const) {
      expect(isAuthFileKnownBad(authFile({ selfTestVerdict: verdict }))).toBe(true);
    }
    expect(isAuthFileKnownBad(authFile({ selfTestVerdict: 'healthy' }))).toBe(false);
  });

  test('never-tested is its own state, not a synonym for healthy', () => {
    const untested = authFile();
    expect(isAuthFileUntested(untested)).toBe(true);
    // The whole point: a credential nothing was asked of must not read as callable.
    expect(isAuthFileKnownHealthy(untested)).toBe(false);
    expect(isAuthFileKnownBad(untested)).toBe(false);
  });

  test('the three states are mutually exclusive and cover every credential', () => {
    const cases: Array<Partial<AuthFileItem>> = [
      {},
      { selfTestVerdict: 'healthy' },
      { selfTestVerdict: 'cooling' },
      { selfTestVerdict: 'quarantine' },
      { selfTestVerdict: 'validation' },
    ];
    for (const overrides of cases) {
      const file = authFile(overrides);
      const matched = [
        isAuthFileKnownHealthy(file),
        isAuthFileKnownBad(file),
        isAuthFileUntested(file),
      ].filter(Boolean).length;
      expect(matched).toBe(1);
    }
  });

  test('a credential that is both disabled and marked bad stays filterable', () => {
    // Disabling does not erase what the sweep concluded; an operator looking at
    // "not callable" should still see it.
    const file = authFile({ disabled: true, selfTestVerdict: 'quarantine' });
    expect(isAuthFileKnownBad(file)).toBe(true);
    expect(isAuthFileKnownHealthy(file)).toBe(false);
  });
});

describe('problem classification is independent of self-test', () => {
  test('a self-test failure alone does not enter the problem set that feeds bulk delete', () => {
    // isProblemAuthFile also drives 「删除问题凭证」. Letting sweep results in
    // would widen a destructive bulk action, so it must stay untouched by them.
    const file = authFile({ selfTestVerdict: 'quarantine', selfTestAutoDisabled: true });
    expect(isAuthFileKnownBad(file)).toBe(true);
    expect(isProblemAuthFile(file)).toBe(false);
  });

  test('the existing problem rules still work', () => {
    expect(isProblemAuthFile(authFile({ unavailable: true }))).toBe(true);
    expect(isProblemAuthFile(authFile({ statusMessage: 'payment_required' }))).toBe(true);
    expect(isProblemAuthFile(authFile({ status: 'disabled', disabled: true }))).toBe(false);
    expect(isProblemAuthFile(authFile({ status: 'active' }))).toBe(false);
  });
});
