import { describe, expect, test } from 'bun:test';
import {
  LOCATION_UNSUPPORTED_ERROR_CODE,
  isAuthFileLocationRestricted,
  isProblemAuthFile,
} from '../src/features/authFiles/constants';
import { normalizeAuthFilesResponse } from '../src/services/api/authFiles';
import type { AuthFileItem } from '../src/types';

const authFile = (overrides: Partial<AuthFileItem> = {}): AuthFileItem => ({
  name: 'credential.json',
  type: 'antigravity',
  ...overrides,
});

describe('region restricted auth file', () => {
  test('matches only the exact backend marker', () => {
    expect(LOCATION_UNSUPPORTED_ERROR_CODE).toBe('location_unsupported');
    expect(isAuthFileLocationRestricted(authFile({ lastErrorCode: 'location_unsupported' }))).toBe(
      true
    );
  });

  test('reads the raw backend field when normalization did not run', () => {
    expect(isAuthFileLocationRestricted(authFile({ last_error_code: 'location_unsupported' }))).toBe(
      true
    );
  });

  test('tolerates surrounding whitespace in the code', () => {
    expect(
      isAuthFileLocationRestricted(authFile({ lastErrorCode: '  location_unsupported ' }))
    ).toBe(true);
  });

  test('is false for a credential that never failed', () => {
    expect(isAuthFileLocationRestricted(authFile())).toBe(false);
  });

  test('is false for every other error code', () => {
    for (const code of ['credential_invalid', 'request_scoped', 'payment_required', 'location']) {
      expect(isAuthFileLocationRestricted(authFile({ lastErrorCode: code }))).toBe(false);
    }
  });

  test('never infers the restriction from status text', () => {
    // Free text is whatever the upstream said; only the machine code is trusted.
    expect(
      isAuthFileLocationRestricted(
        authFile({
          lastStatusCode: 400,
          statusMessage: 'User location is not supported for the API use.',
        })
      )
    ).toBe(false);
    expect(isAuthFileLocationRestricted(authFile({ statusMessage: 'location_unsupported' }))).toBe(
      false
    );
  });
});

describe('region restricted auth file and the problem set', () => {
  // isProblemAuthFile drives the bulk "delete problem credentials" action, so a
  // credential that a proxy change recovers must never fall into it.
  const restrictedAfterFailure = authFile({
    status: 'error',
    unavailable: true,
    statusMessage: 'location_unsupported',
    lastStatusCode: 400,
    lastErrorCode: 'location_unsupported',
  });

  test('is excluded from the problem set', () => {
    expect(isProblemAuthFile(restrictedAfterFailure)).toBe(false);
  });

  test('an ordinary failing credential is still a problem', () => {
    expect(
      isProblemAuthFile(
        authFile({
          status: 'error',
          unavailable: true,
          statusMessage: 'payment_required',
          lastStatusCode: 403,
          lastErrorCode: 'payment_required',
        })
      )
    ).toBe(true);
  });

  test('a credential whose latest failure is something else is a problem again', () => {
    // LastError is overwritten by the most recent failure, so a credential that was
    // region restricted earlier but is now rejected for another reason is judged on
    // that reason.
    expect(
      isProblemAuthFile(
        authFile({
          status: 'error',
          unavailable: true,
          statusMessage: 'unauthorized',
          lastStatusCode: 401,
          lastErrorCode: 'credential_invalid',
        })
      )
    ).toBe(true);
  });

  test('stays out of the problem set when disabled either way', () => {
    expect(isProblemAuthFile({ ...restrictedAfterFailure, disabled: true })).toBe(false);
  });
});

describe('region restricted marker survives the API boundary', () => {
  test('the list normalizer keeps last_error_code as lastErrorCode', () => {
    const normalized = normalizeAuthFilesResponse({
      files: [
        {
          name: 'blocked.json',
          type: 'antigravity',
          status: 'error',
          status_message: 'location_unsupported',
          last_status_code: 400,
          last_error_code: 'location_unsupported',
        },
        { name: 'healthy.json', type: 'antigravity', status: 'active' },
      ],
    });
    const blocked = normalized.files.find((file) => file.name === 'blocked.json');
    const healthy = normalized.files.find((file) => file.name === 'healthy.json');
    expect(blocked && isAuthFileLocationRestricted(blocked)).toBe(true);
    expect(healthy && isAuthFileLocationRestricted(healthy)).toBe(false);
  });
});
