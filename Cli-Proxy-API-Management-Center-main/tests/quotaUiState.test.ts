/**
 * Session-scoped quota page preferences.
 *
 * The merge-on-write case is the one that matters: the tab strip and the sort
 * control each write a single field and know nothing about the other, so a
 * whole-object write would make changing a tab silently reset the sort.
 */

import { afterAll, beforeEach, describe, expect, test } from 'bun:test';
import { readQuotaUiState, writeQuotaUiState } from '@/features/quota/uiState';

const KEY = 'quotaPage.uiState';

/** Test files share one process — leaving a fake `window` behind would leak. */
const originalWindow = (globalThis as { window?: unknown }).window;

/** bun's global has no sessionStorage; a Map-backed stub is enough here. */
function installSessionStorage() {
  const store = new Map<string, string>();
  const storage = {
    getItem: (key: string) => store.get(key) ?? null,
    setItem: (key: string, value: string) => void store.set(key, value),
    removeItem: (key: string) => void store.delete(key),
    clear: () => store.clear(),
    key: (index: number) => [...store.keys()][index] ?? null,
    get length() {
      return store.size;
    },
  };
  (globalThis as unknown as { window: unknown }).window = { sessionStorage: storage };
  return storage;
}

let storage: ReturnType<typeof installSessionStorage>;

beforeEach(() => {
  storage = installSessionStorage();
});

afterAll(() => {
  if (originalWindow === undefined) {
    delete (globalThis as { window?: unknown }).window;
  } else {
    (globalThis as { window?: unknown }).window = originalWindow;
  }
});

describe('quota ui state', () => {
  test('round-trips every preference', () => {
    writeQuotaUiState({
      tab: 'codex',
      sortMode: 'soonest',
      availabilityFilter: 'exhausted',
      familyFilter: 'gemini',
    });
    expect(readQuotaUiState()).toEqual({
      tab: 'codex',
      sortMode: 'soonest',
      availabilityFilter: 'exhausted',
      familyFilter: 'gemini',
    });
  });

  test('accepts the weekly sort mode', () => {
    // 新模式加进 QUOTA_SORT_MODES 的那一刻，这个校验集合就跟着扩展了 ——
    // 不需要单独改 uiState。
    writeQuotaUiState({ sortMode: 'weekly' });
    expect(readQuotaUiState()?.sortMode).toBe('weekly');
  });

  test('writing one preference preserves the others', () => {
    writeQuotaUiState({ sortMode: 'soonest' });
    writeQuotaUiState({ tab: 'kimi' });
    writeQuotaUiState({ availabilityFilter: 'failed' });
    writeQuotaUiState({ familyFilter: 'claude' });

    expect(readQuotaUiState()).toEqual({
      tab: 'kimi',
      sortMode: 'soonest',
      availabilityFilter: 'failed',
      familyFilter: 'claude',
    });
  });

  test('rejects values that are not part of the current contract', () => {
    storage.setItem(
      KEY,
      JSON.stringify({
        tab: 'not-a-tab',
        sortMode: 'by-vibes',
        availabilityFilter: 'maybe',
        familyFilter: 'gpt',
      })
    );
    expect(readQuotaUiState()).toEqual({
      tab: undefined,
      sortMode: undefined,
      availabilityFilter: undefined,
      familyFilter: undefined,
    });
  });

  test('survives absent, malformed, and non-object payloads', () => {
    expect(readQuotaUiState()).toBeNull();

    storage.setItem(KEY, '{not json');
    expect(readQuotaUiState()).toBeNull();

    storage.setItem(KEY, '"a string"');
    expect(readQuotaUiState()).toBeNull();
  });
});
