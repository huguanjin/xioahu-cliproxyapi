import { apiCallApi, getApiCallErrorMessage } from './apiCall';
import {
  ANTIGRAVITY_CODE_ASSIST_URL,
  ANTIGRAVITY_REQUEST_HEADERS,
  createStatusError,
  normalizeNumberValue,
  normalizeStringValue,
  parseAntigravityPayload,
} from '@/utils/quota';

export type AntigravitySubscriptionPlan = 'free' | 'pro' | 'ultra' | 'ultra-lite' | 'unknown';

/**
 * AI credits balance from `paidTier.availableCredits`.
 *
 * A balance, not a quota window: it is what tops up requests after the plan's
 * rate limits are spent, and `minimumCreditAmountForUsage` is the floor below
 * which it stops being usable at all. Both numbers matter — "480 credits" means
 * something different depending on whether the floor is 1 or 500, so the floor
 * is carried here rather than collapsed into a boolean.
 *
 * `available` is computed here against the floor so callers do not each redo the
 * comparison. Null credits mean the tier carries no GOOGLE_ONE_AI entry, which
 * is common and is not an error.
 */
export type AntigravityCredits = {
  amount: number;
  minAmount: number;
  available: boolean;
};

export type AntigravitySubscriptionSummary = {
  plan: AntigravitySubscriptionPlan;
  tierId: string | null;
  tierName: string | null;
  credits: AntigravityCredits | null;
};

type SubscriptionTier = {
  id: string | null;
  name: string | null;
};

type RawTierPayload = {
  id?: unknown;
  name?: string;
  availableCredits?: unknown;
  available_credits?: unknown;
};

type RawCreditPayload = {
  creditType?: unknown;
  credit_type?: unknown;
  creditAmount?: unknown;
  credit_amount?: unknown;
  minimumCreditAmountForUsage?: unknown;
  minimum_credit_amount_for_usage?: unknown;
};

const CODE_ASSIST_REQUEST_BODY = JSON.stringify({ metadata: { ideType: 'ANTIGRAVITY' } });

const PLAN_BY_TIER_ID = new Map<string, AntigravitySubscriptionPlan>([
  ['free-tier', 'free'],
  ['g1-pro-tier', 'pro'],
  ['g1-ultra-tier', 'ultra'],
  ['g1-ultra-lite-tier', 'ultra-lite'],
]);

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === 'object' && value !== null && !Array.isArray(value);

const normalizeTier = (value: unknown): SubscriptionTier | null => {
  if (!isRecord(value)) return null;
  const rawTier = value as RawTierPayload;
  return {
    id: normalizeStringValue(rawTier.id),
    name: normalizeStringValue(rawTier.name),
  };
};

const resolvePlan = (tierId: string | null): AntigravitySubscriptionPlan => {
  if (!tierId) return 'unknown';
  return PLAN_BY_TIER_ID.get(tierId) ?? 'unknown';
};

/**
 * Pull the GOOGLE_ONE_AI balance out of a tier payload.
 *
 * The array can hold several credit types and the one that governs request
 * capacity is only GOOGLE_ONE_AI — matching the backend, which filters the same
 * way (antigravity_executor_credits.go).
 *
 * A missing or unparsable entry returns null rather than a zero balance: "this
 * tier has no AI credits" and "this tier is out of AI credits" are different
 * facts, and rendering the first as 0 would tell the operator a paid tier is
 * exhausted when it simply is not a credits tier.
 */
const parseCredits = (raw: unknown): AntigravityCredits | null => {
  if (!Array.isArray(raw)) return null;

  for (const entry of raw) {
    if (!isRecord(entry)) continue;
    const credit = entry as RawCreditPayload;
    const kind = normalizeStringValue(credit.creditType ?? credit.credit_type);
    if (kind !== 'GOOGLE_ONE_AI') continue;

    const amount = normalizeNumberValue(credit.creditAmount ?? credit.credit_amount);
    if (amount === null) continue;
    const minAmount =
      normalizeNumberValue(credit.minimumCreditAmountForUsage ?? credit.minimum_credit_amount_for_usage) ??
      0;

    return { amount, minAmount, available: amount >= minAmount };
  }
  return null;
};

export const parseAntigravitySubscriptionSummary = (
  payload: unknown
): AntigravitySubscriptionSummary | null => {
  const parsed = parseAntigravityPayload(payload);
  if (!parsed) return null;

  const rawCurrentTier = parsed.currentTier ?? parsed.current_tier;
  const rawPaidTier = parsed.paidTier ?? parsed.paid_tier;
  const currentTier = normalizeTier(rawCurrentTier);
  const paidTier = normalizeTier(rawPaidTier);
  const effectiveTier = paidTier?.id ? paidTier : currentTier;
  if (!effectiveTier?.id && !effectiveTier?.name) return null;

  // Credits come off the RAW paid-tier object, not the normalized one:
  // normalizeTier narrows to {id, name} and would have dropped the array
  // entirely — reading it there silently yields null for every credential.
  //
  // The balance belongs to what the account actually pays for, so currentTier
  // is deliberately not consulted: it is the plan-label fallback, not a source
  // of credits.
  const credits = parseCredits(
    isRecord(rawPaidTier)
      ? ((rawPaidTier as RawTierPayload).availableCredits ??
        (rawPaidTier as RawTierPayload).available_credits)
      : null
  );

  return {
    plan: resolvePlan(effectiveTier.id),
    tierId: effectiveTier.id,
    tierName: effectiveTier.name,
    credits,
  };
};

export const antigravitySubscriptionApi = {
  async get(authIndex: string): Promise<AntigravitySubscriptionSummary | null> {
    const result = await apiCallApi.request({
      authIndex,
      method: 'POST',
      url: ANTIGRAVITY_CODE_ASSIST_URL,
      header: { ...ANTIGRAVITY_REQUEST_HEADERS },
      data: CODE_ASSIST_REQUEST_BODY,
    });

    if (result.statusCode < 200 || result.statusCode >= 300) {
      throw createStatusError(getApiCallErrorMessage(result), result.statusCode);
    }

    return parseAntigravitySubscriptionSummary(result.body ?? result.bodyText);
  },
};
