/**
 * 家族额度状态：是否处于「用尽等待恢复」，以及周限额排序键。
 *
 * The distinction under test throughout: this badge describes the UPSTREAM
 * PAYLOAD, not routing. Routing never reads a quota bucket — it learns about
 * exhaustion only from a 429/403, with a backoff capped at 30 minutes. So the
 * tests pin facts about the payload (spent, reset instant, staleness), not any
 * claim the proxy will refuse a call.
 */

import { describe, expect, test } from 'bun:test';
import {
  familyQuotaState,
  familyWindowOf,
  isBucketSpent,
  weeklyRemainingMin,
} from '@/features/quota/providers/antigravity/familyState';
import type { AntigravityQuotaGroup } from '@/types';

const WEEKLY = 168;
const FIVE_HOUR = 5;

const bucket = (
  window: 'weekly' | '5h',
  remainingFraction: number,
  resetAtMs?: number | null
) => ({
  id: `${window}-${remainingFraction}`,
  label: window,
  remainingFraction,
  periodHours: window === 'weekly' ? WEEKLY : FIVE_HOUR,
  ...(resetAtMs === undefined ? {} : { resetAtMs }),
});

const group = (buckets: ReturnType<typeof bucket>[]): AntigravityQuotaGroup =>
  ({ id: 'g', label: 'gemini models', buckets }) as unknown as AntigravityQuotaGroup;

const NOW = 1_800_000_000_000;
const HOUR = 3_600_000;

describe('isBucketSpent', () => {
  test('counts a bucket whose ROUNDED percent is 0', () => {
    // 0.004 渲染成「剩余 0%」。若判定用 === 0，这一行会显示 0% 却没有徽章 ——
    // 正是要消除的那种不一致。
    expect(isBucketSpent({ remainingFraction: 0.004 })).toBe(true);
    expect(isBucketSpent({ remainingFraction: 0 })).toBe(true);
  });

  test('does not count a bucket that still renders above 0%', () => {
    expect(isBucketSpent({ remainingFraction: 0.006 })).toBe(false);
    expect(isBucketSpent({ remainingFraction: 1 })).toBe(false);
  });

  test('a missing or unusable fraction is not spent', () => {
    // 未知不是「用尽」。当成用尽会给一个没查到数据的家族挂上倒计时。
    expect(isBucketSpent({})).toBe(false);
    expect(isBucketSpent({ remainingFraction: null })).toBe(false);
    expect(isBucketSpent({ remainingFraction: Number.NaN })).toBe(false);
  });
});

describe('familyWindowOf', () => {
  test('reads the period first', () => {
    expect(familyWindowOf({ periodHours: WEEKLY })).toBe('weekly');
    expect(familyWindowOf({ periodHours: FIVE_HOUR })).toBe('5h');
    expect(familyWindowOf({ periodHours: 24 })).toBe('5h');
  });

  test('falls back to the window spelling only when the period is absent', () => {
    expect(familyWindowOf({ window: 'weekly' })).toBe('weekly');
    expect(familyWindowOf({ window: 'Weekly Limit' })).toBe('weekly');
    expect(familyWindowOf({ window: '5 hour limit' })).toBe('5h');
  });

  test('a period wins over a contradictory spelling', () => {
    // 上游词汇是自由文本，拼写偶尔会漂。周期是数字，更可信。
    expect(familyWindowOf({ window: 'weekly', periodHours: FIVE_HOUR })).toBe('5h');
  });
});

describe('familyQuotaState', () => {
  test('says nothing while both windows have capacity', () => {
    expect(familyQuotaState(group([bucket('weekly', 1, NOW + HOUR), bucket('5h', 0.5, NOW + HOUR)]), NOW)).toEqual({ kind: 'none' });
  });

  test('reports a countdown for a spent weekly window', () => {
    // 操作者截图里的情形：周限额 0。
    const state = familyQuotaState(
      group([bucket('weekly', 0, NOW + 5 * 24 * HOUR), bucket('5h', 1, NOW + HOUR)]),
      NOW
    );
    expect(state).toEqual({ kind: 'countdown', window: 'weekly', resetAtMs: NOW + 5 * 24 * HOUR });
  });

  test('governs on the LATEST reset among spent windows', () => {
    // 两个窗口都空了，必须都恢复才行，所以约束是较晚的那个。
    const weekLater = NOW + 6 * 24 * HOUR;
    const state = familyQuotaState(
      group([bucket('weekly', 0, weekLater), bucket('5h', 0, NOW + HOUR)]),
      NOW
    );
    expect(state).toEqual({ kind: 'countdown', window: 'weekly', resetAtMs: weekLater });
  });

  test('a spent 5h window with a healthy weekly still counts down', () => {
    // 只是几分钟的事，但那几分钟里这个家族确实没额度 —— 隐藏它会让
    // 「为什么现在调用失败」变得无法解释。
    const soon = NOW + 30 * 60 * 1000;
    const state = familyQuotaState(
      group([bucket('weekly', 1, NOW + 5 * 24 * HOUR), bucket('5h', 0, soon)]),
      NOW
    );
    expect(state).toEqual({ kind: 'countdown', window: '5h', resetAtMs: soon });
  });

  test('a spent window whose reset already passed reports stale, not 0 seconds', () => {
    // 过期的载荷：上游说用尽，但那个窗口已经翻篇了。把它当成「还剩 0 秒」
    // 是编造一个期限；真相是这份数据该刷新了。
    const state = familyQuotaState(
      group([bucket('weekly', 0, NOW - HOUR), bucket('5h', 1, NOW + HOUR)]),
      NOW
    );
    expect(state).toEqual({ kind: 'stale', window: 'weekly' });
  });

  test('the boundary instant counts as stale, not as a countdown', () => {
    // resetAtMs === now 时恢复已经开始，不该再显示倒计时。
    expect(familyQuotaState(group([bucket('weekly', 0, NOW)]), NOW)).toEqual({
      kind: 'stale',
      window: 'weekly',
    });
  });

  test('a spent window with no readable reset reports unknownTime', () => {
    // 绝不编造一个期限。
    expect(familyQuotaState(group([bucket('weekly', 0)]), NOW)).toEqual({
      kind: 'unknownTime',
      window: 'weekly',
    });
    expect(familyQuotaState(group([bucket('weekly', 0, null)]), NOW)).toEqual({
      kind: 'unknownTime',
      window: 'weekly',
    });
  });

  test('an unreadable fraction alone does not trigger a state', () => {
    expect(familyQuotaState(group([{ id: 'x', label: 'weekly', periodHours: WEEKLY }] as never), NOW)).toEqual({ kind: 'none' });
  });

  test('handles a group with no buckets', () => {
    expect(familyQuotaState(group([]), NOW)).toEqual({ kind: 'none' });
  });
});

describe('weeklyRemainingMin', () => {
  test('takes the minimum weekly remaining across families', () => {
    // min 是唯一不会「声称并不存在的余量」的聚合：一个家族耗尽就不会被抬上来。
    const groups = [
      group([bucket('weekly', 0.8, NOW + HOUR), bucket('5h', 1, NOW + HOUR)]),
      group([bucket('weekly', 0.1, NOW + HOUR), bucket('5h', 1, NOW + HOUR)]),
    ];
    expect(weeklyRemainingMin(groups)).toBe(10);
  });

  test('ignores non-weekly windows', () => {
    const groups = [group([bucket('5h', 0.02, NOW + HOUR), bucket('weekly', 0.9, NOW + HOUR)])];
    expect(weeklyRemainingMin(groups)).toBe(90);
  });

  test('takes the minimum across a single family with several weekly buckets', () => {
    const groups = [group([bucket('weekly', 0.5, NOW + HOUR), bucket('weekly', 0.25, NOW + HOUR)])];
    expect(weeklyRemainingMin(groups)).toBe(25);
  });

  test('returns null when no weekly bucket is readable', () => {
    // null 意味着「未知」，排序时沉底。当成 0 会把没查过的凭证排成耗尽，
    // 当成 100 会把它排成充足 —— 两者都错。
    expect(weeklyRemainingMin([])).toBeNull();
    expect(weeklyRemainingMin([group([bucket('5h', 0.5, NOW + HOUR)])])).toBeNull();
  });

  test('rounds like the row does, so the sort agrees with what is displayed', () => {
    expect(weeklyRemainingMin([group([bucket('weekly', 0.004, NOW + HOUR)])])).toBe(0);
    expect(weeklyRemainingMin([group([bucket('weekly', 0.996, NOW + HOUR)])])).toBe(100);
  });
});
