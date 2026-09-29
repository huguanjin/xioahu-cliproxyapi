/**
 * blocked_models 的前端归一化。
 *
 * The backend omits this field entirely when nothing is blocked, so the UI
 * branches on presence. That means a malformed entry must not silently become an
 * empty array — which would look identical to "everything is fine" — and a row
 * must never carry a guessed reason, because the reason is what tells an operator
 * whether to wait or to investigate.
 */

import { describe, expect, test } from 'bun:test';
import { normalizeAuthFilesResponse } from '@/services/api/authFiles';

const withBlocked = (blocked: unknown) => ({
  files: [{ name: 'a.json', provider: 'antigravity', blocked_models: blocked }],
});

const blockedOf = (blocked: unknown) =>
  normalizeAuthFilesResponse(withBlocked(blocked) as never).files[0].blockedModels;

describe('blocked_models normalization', () => {
  test('keeps a well-formed entry', () => {
    const got = blockedOf([
      {
        id: 'claude-opus-4-6-thinking',
        reason: 'cooldown',
        retry_at: '2026-04-20T09:19:49Z',
        status_message: 'rate limited',
      },
    ]);

    expect(got).toEqual([
      {
        id: 'claude-opus-4-6-thinking',
        reason: 'cooldown',
        retry_at: '2026-04-20T09:19:49Z',
        status_message: 'rate limited',
      },
    ]);
  });

  test('keeps a minimal entry, omitting absent optionals', () => {
    // 缺 retry_at 是正常的（没有已知恢复时刻），不能补成空字符串 ——
    // 那会让渲染层以为「有值」而多画一个分隔符。
    expect(blockedOf([{ id: 'gemini-a', reason: 'blocked' }])).toEqual([
      { id: 'gemini-a', reason: 'blocked' },
    ]);
  });

  test('an unknown reason falls back to blocked, never to cooldown', () => {
    // cooldown 会让操作者「等着就行」。未知原因时这个默认值是危险的。
    expect(blockedOf([{ id: 'x', reason: 'something-new' }])).toEqual([
      { id: 'x', reason: 'blocked' },
    ]);
    expect(blockedOf([{ id: 'x' }])).toEqual([{ id: 'x', reason: 'blocked' }]);
  });

  test('drops entries with no usable id', () => {
    expect(
      blockedOf([{ reason: 'cooldown' }, { id: '   ' }, { id: 42 }, null, 'x'])
    ).toBeUndefined();
  });

  test('drops malformed entries but keeps the good ones', () => {
    expect(
      blockedOf([null, { id: 'good', reason: 'cooldown' }, { reason: 'blocked' }])
    ).toEqual([{ id: 'good', reason: 'cooldown' }]);
  });

  test('omits the field entirely when nothing parses', () => {
    // 与「后端没下发」同一形状。空数组会让 UI 走进一个没有内容的渲染分支。
    expect(blockedOf([])).toBeUndefined();
    expect(blockedOf('not-an-array')).toBeUndefined();
    expect(blockedOf(undefined)).toBeUndefined();
    expect(blockedOf(null)).toBeUndefined();
  });

  test('omits the field when the credential reports no blocked models', () => {
    const normalized = normalizeAuthFilesResponse({
      files: [{ name: 'a.json', provider: 'antigravity' }],
    } as never);
    expect(normalized.files[0].blockedModels).toBeUndefined();
  });

  test('trims whitespace on every carried string', () => {
    expect(
      blockedOf([{ id: '  gemini-a  ', reason: 'cooldown', retry_at: ' 2026-04-20T09:19:49Z ', status_message: '  hi  ' }])
    ).toEqual([
      { id: 'gemini-a', reason: 'cooldown', retry_at: '2026-04-20T09:19:49Z', status_message: 'hi' },
    ]);
  });

  test('preserves multiple entries in backend order', () => {
    // 后端已按 id 排好序；前端重排会让每次轮询的顺序看起来在跳。
    expect(
      blockedOf([
        { id: 'alpha', reason: 'cooldown' },
        { id: 'mid', reason: 'blocked' },
        { id: 'zeta', reason: 'cooldown' },
      ])
    ).toEqual([
      { id: 'alpha', reason: 'cooldown' },
      { id: 'mid', reason: 'blocked' },
      { id: 'zeta', reason: 'cooldown' },
    ]);
  });
});
