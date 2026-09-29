/**
 * 一个模型家族的额度状态：是否处于「用尽等待恢复」。
 *
 * Two facts and one prediction live here, and the distinction is load-bearing:
 *
 * - The reset instant is a FACT — it comes verbatim from the upstream quota
 *   payload the browser fetched.
 * - The remaining fraction is a fact AS OF THE LAST FETCH. Nothing on the Go
 *   side refreshes or caches it, so this is only as fresh as the last time
 *   someone opened the quota tab.
 * - "This family cannot be called until then" is a PREDICTION. Nothing in this
 *   proxy enforces it: routing reads only ModelStates, which is written after an
 *   upstream 429/403 comes back, and its backoff is capped at 30 minutes. The
 *   weekly reset is days away, so the two are never the same number.
 *
 * That is why the UI must say 用尽 (a statement about the payload) rather than
 * 无法调用 (a claim about routing the proxy does not implement).
 *
 * Pure and React-free, so `tests/antigravityFamilyState.test.ts` can drive it.
 */

import type { AntigravityQuotaGroup } from '@/types';

/** 家族的额度状态。`none` 表示无可报告的事 —— 缺省即健康。 */
export type AntigravityFamilyState =
  | { kind: 'none' }
  | { kind: 'countdown'; window: FamilyWindow; resetAtMs: number }
  | { kind: 'stale'; window: FamilyWindow }
  | { kind: 'unknownTime'; window: FamilyWindow };

export type FamilyWindow = 'weekly' | '5h';

const WEEKLY_PERIOD_HOURS = 168;

/**
 * 一个桶是否已用尽。
 *
 * Judged on the ROUNDED value the row displays, not the raw fraction. A bucket
 * at 0.004 renders "剩余 0%", so driving the badge off `=== 0` would show 0%
 * with no badge — exactly the inconsistency this exists to remove.
 */
export function isBucketSpent(bucket: { remainingFraction?: number | null }): boolean {
  const fraction = bucket.remainingFraction;
  if (typeof fraction !== 'number' || !Number.isFinite(fraction)) return false;
  const clamped = Math.min(1, Math.max(0, fraction));
  return Math.round(clamped * 100) === 0;
}

/**
 * 这个桶是周限额还是 5 小时限额。
 *
 * Read from periodHours so the classification matches the timeline and the sort,
 * which both key off the same field. Falls back to the window spelling only when
 * the period is absent, because the upstream vocabulary is free text and a
 * miss there would silently file a weekly window as 5-hour.
 */
export function familyWindowOf(bucket: {
  window?: string;
  periodHours?: number | null;
}): FamilyWindow {
  if (typeof bucket.periodHours === 'number' && bucket.periodHours > 0) {
    return bucket.periodHours >= WEEKLY_PERIOD_HOURS ? 'weekly' : '5h';
  }
  const spelling = (bucket.window ?? '').trim().toLowerCase();
  return spelling.includes('week') ? 'weekly' : '5h';
}

/**
 * 一个家族的额度状态。
 *
 * The governing instant is the LATEST reset among the spent buckets: every spent
 * window has to clear before the family serves again, so the binding constraint
 * is the last one. This is correct whether the two windows are independent
 * budgets or a weekly cap layered over the 5h, so it does not depend on
 * resolving that question.
 *
 * `nowMs` is injected rather than read, so the past-reset case is directly
 * testable and the function stays store-free. It must be the SAME clock base the
 * rows use (Date.now() + serverTimeOffsetMs), or the badge and the row it sits
 * above would disagree by the offset.
 */
export function familyQuotaState(
  group: AntigravityQuotaGroup,
  nowMs: number
): AntigravityFamilyState {
  const spent = (group.buckets ?? []).filter(isBucketSpent);
  if (spent.length === 0) return { kind: 'none' };

  const dated = spent.filter(
    (bucket) => typeof bucket.resetAtMs === 'number' && Number.isFinite(bucket.resetAtMs)
  );
  if (dated.length === 0) {
    return { kind: 'unknownTime', window: familyWindowOf(spent[0]) };
  }

  const governing = dated.reduce((latest, bucket) =>
    (bucket.resetAtMs as number) > (latest.resetAtMs as number) ? bucket : latest
  );
  const resetAtMs = governing.resetAtMs as number;
  const window = familyWindowOf(governing);

  // Past instant on the governing bucket means EVERY spent reset is past, since
  // this is the maximum. That is a stale payload — upstream said "spent" about a
  // window that has already rolled — not "0 seconds left". Clamping it would
  // invent a deadline.
  if (resetAtMs <= nowMs) return { kind: 'stale', window };

  return { kind: 'countdown', window, resetAtMs };
}

/**
 * 一个凭证的周限额剩余百分比，取各家族中最小的一档。
 *
 * Min is the only aggregate that never claims room that is not there: the head
 * of a list sorted by it is ample under either reading of the 5h/weekly
 * relationship, and one exhausted family cannot float a credential up. A sum or
 * average would imply a shared budget the payload contradicts; a max is gameable
 * by a single full family.
 *
 * Returns null when there is no readable weekly bucket — no quota loaded, a
 * failed fetch, or a provider that does not report one. Null means unknown and
 * sorts last; treating it as 0 would rank an unqueried credential as exhausted,
 * and treating it as 100 would rank it as ample. Both are wrong.
 */
export function weeklyRemainingMin(groups: readonly AntigravityQuotaGroup[]): number | null {
  const values: number[] = [];
  for (const group of groups) {
    for (const bucket of group.buckets ?? []) {
      if (familyWindowOf(bucket) !== 'weekly') continue;
      const fraction = bucket.remainingFraction;
      if (typeof fraction !== 'number' || !Number.isFinite(fraction)) continue;
      values.push(Math.round(Math.min(1, Math.max(0, fraction)) * 100));
    }
  }
  return values.length > 0 ? Math.min(...values) : null;
}
