/**
 * AI credits 余额解析。
 *
 * The frontend already POSTs loadCodeAssist for the plan label, and that same
 * response carries `paidTier.availableCredits`. The Go executor has parsed and
 * cached those numbers for a while (antigravity_executor_credits.go), but no UI
 * ever saw them — so an operator could not tell a credential that still has
 * fallback credits from one that is genuinely spent.
 *
 * The distinction these tests protect: "this tier has no credits" and "this tier
 * is out of credits" are different facts. Collapsing the first into 0 would
 * render a perfectly healthy free-tier account as exhausted.
 */

import { describe, expect, test } from 'bun:test';
import { parseAntigravitySubscriptionSummary } from '@/services/api/antigravitySubscription';

const payload = (paidTier: unknown) => ({
  body: JSON.stringify({ currentTier: { id: 'free-tier' }, paidTier }),
});

describe('credits parsing', () => {
  test('reads the GOOGLE_ONE_AI balance', () => {
    const summary = parseAntigravitySubscriptionSummary(
      payload({
        id: 'g1-pro-tier',
        availableCredits: [
          {
            creditType: 'GOOGLE_ONE_AI',
            creditAmount: 480,
            minimumCreditAmountForUsage: 1,
          },
        ],
      })
    );

    expect(summary?.credits).toEqual({ amount: 480, minAmount: 1, available: true });
  });

  test('accepts snake_case field names', () => {
    const summary = parseAntigravitySubscriptionSummary(
      payload({
        id: 'g1-pro-tier',
        availableCredits: [
          {
            credit_type: 'GOOGLE_ONE_AI',
            credit_amount: '12.5',
            minimum_credit_amount_for_usage: '1',
          },
        ],
      })
    );

    expect(summary?.credits).toEqual({ amount: 12.5, minAmount: 1, available: true });
  });

  test('reports unavailable when the balance is below the usage floor', () => {
    // 余额还在，但低于可用门槛 —— 这与「余额为 0」不同，但结果一样：用不了。
    const summary = parseAntigravitySubscriptionSummary(
      payload({
        id: 'g1-pro-tier',
        availableCredits: [
          { creditType: 'GOOGLE_ONE_AI', creditAmount: 0.4, minimumCreditAmountForUsage: 1 },
        ],
      })
    );

    expect(summary?.credits?.available).toBe(false);
    expect(summary?.credits?.amount).toBe(0.4);
  });

  test('ignores credit types other than GOOGLE_ONE_AI', () => {
    // 与后端同一过滤口径（antigravity_executor_credits.go）。
    const summary = parseAntigravitySubscriptionSummary(
      payload({
        id: 'g1-pro-tier',
        availableCredits: [
          { creditType: 'SOMETHING_ELSE', creditAmount: 9999, minimumCreditAmountForUsage: 1 },
        ],
      })
    );

    expect(summary?.credits).toBeNull();
  });

  test('picks GOOGLE_ONE_AI out of a mixed list', () => {
    const summary = parseAntigravitySubscriptionSummary(
      payload({
        id: 'g1-ultra-tier',
        availableCredits: [
          { creditType: 'OTHER', creditAmount: 5000, minimumCreditAmountForUsage: 1 },
          { creditType: 'GOOGLE_ONE_AI', creditAmount: 42, minimumCreditAmountForUsage: 1 },
        ],
      })
    );

    expect(summary?.credits?.amount).toBe(42);
  });

  test('returns null when the tier carries no credits at all', () => {
    // 免费号很常见。这里是 null 而不是 0 —— 界面据此整块隐藏，而不是显示
    // 「已用尽」。
    const summary = parseAntigravitySubscriptionSummary(payload({ id: 'free-tier' }));
    expect(summary?.credits).toBeNull();
  });

  test('returns null rather than throwing on malformed entries', () => {
    const cases: unknown[] = [
      { id: 'g1-pro-tier', availableCredits: 'not-an-array' },
      { id: 'g1-pro-tier', availableCredits: [] },
      { id: 'g1-pro-tier', availableCredits: [null, 42, 'x'] },
      { id: 'g1-pro-tier', availableCredits: [{ creditType: 'GOOGLE_ONE_AI' }] },
      { id: 'g1-pro-tier', availableCredits: [{ creditType: 'GOOGLE_ONE_AI', creditAmount: 'abc' }] },
    ];

    for (const paidTier of cases) {
      const summary = parseAntigravitySubscriptionSummary(payload(paidTier));
      // 套餐本身仍然解析出来；只是没有点数信息。
      expect(summary?.plan).toBe('pro');
      expect(summary?.credits).toBeNull();
    }
  });

  test('defaults the usage floor to 0 when absent', () => {
    // 缺门槛时按「>0 即可用」理解，而不是当成缺数据整条丢掉 —— 余额本身是
    // 有信息量的。
    const summary = parseAntigravitySubscriptionSummary(
      payload({
        id: 'g1-pro-tier',
        availableCredits: [{ creditType: 'GOOGLE_ONE_AI', creditAmount: 5 }],
      })
    );

    expect(summary?.credits).toEqual({ amount: 5, minAmount: 0, available: true });
  });

  test('still resolves the plan when credits are present', () => {
    const summary = parseAntigravitySubscriptionSummary(
      payload({
        id: 'g1-ultra-tier',
        name: 'Ultra',
        availableCredits: [
          { creditType: 'GOOGLE_ONE_AI', creditAmount: 100, minimumCreditAmountForUsage: 1 },
        ],
      })
    );

    expect(summary?.plan).toBe('ultra');
    expect(summary?.tierName).toBe('Ultra');
    expect(summary?.credits?.amount).toBe(100);
  });
});
