/**
 * 额度可用性：区分「有配额」「已用尽」「取不到」和「还没查」。
 *
 * A grid of quota cards makes three genuinely different outcomes look alike.
 * The fetch failed (the credential is broken and probably wants downloading or
 * deleting), the fetch succeeded but every window is spent (the credential is
 * fine, it just has nothing left until a reset), and nobody has asked yet. A
 * user scanning for "which of these can I actually use right now" wants those
 * told apart, so they are read apart here rather than collapsed into a single
 * "not available".
 *
 * Deliberately does NOT route through `buildTimelineLane`, despite that model
 * already normalizing a remaining percentage for all five providers. It is
 * built for drawing: it picks ONE window to anchor a bar and returns nothing at
 * all when no window carries a parseable reset instant. A quota payload can
 * report remaining capacity without one — antigravity's buckets and a rolling
 * claude window both can — and calling such a credential "no data" would hide a
 * perfectly usable credential from the filter, which is the one direction this
 * must not get wrong.
 *
 * Pure and React-free, so `tests/quotaAvailability.test.ts` can consume it.
 */

import type { QuotaProviderType } from './providers/types';
import type { QuotaFileEntry } from './logic';

export const QUOTA_AVAILABILITY_FILTERS = [
  'all',
  'available',
  'exhausted',
  'failed',
  'unloaded',
] as const;

export type QuotaAvailabilityFilter = (typeof QUOTA_AVAILABILITY_FILTERS)[number];

/**
 * What the quota state says about one credential's capacity.
 *
 * - `available`  — fetched, and at least one window still has capacity.
 * - `exhausted`  — fetched, and every readable window is at zero.
 * - `failed`     — the fetch itself failed; the credential is the suspect.
 * - `unloaded`   — no answer yet: never fetched, still in flight, or the
 *                  payload carried no readable capacity figure.
 */
export type QuotaAvailability = 'available' | 'exhausted' | 'failed' | 'unloaded';

export const isQuotaAvailabilityFilter = (value: unknown): value is QuotaAvailabilityFilter =>
  typeof value === 'string' && (QUOTA_AVAILABILITY_FILTERS as readonly string[]).includes(value);

const clampPercent = (value: number) => Math.min(100, Math.max(0, value));

const isFiniteNumber = (value: unknown): value is number =>
  typeof value === 'number' && Number.isFinite(value);

/**
 * Every window's remaining percentage on one credential, each in 0..100.
 *
 * Windows with no readable figure are skipped rather than counted as zero: an
 * absent number is unknown, and scoring it as spent would report a healthy
 * credential as exhausted.
 *
 * The five providers disagree about both the location and the direction of the
 * figure, so each is read on its own terms — claude, codex and xai store
 * percent USED, antigravity reports the fraction REMAINING, and kimi gives raw
 * counts with no percentage at all. Normalizing those into one number is the
 * whole job here; anything that reads the state structurally has to do it
 * anyway, so it is done once, in one place.
 */
export function collectRemainingPercents(
  provider: QuotaProviderType,
  quota: unknown
): number[] {
  const state = quota as { status?: string } | undefined;
  if (!state || state.status !== 'success') return [];

  if (provider === 'claude' || provider === 'codex') {
    const windows = (quota as { windows?: { usedPercent?: number | null }[] }).windows ?? [];
    return windows
      .filter((window) => isFiniteNumber(window.usedPercent))
      .map((window) => clampPercent(100 - (window.usedPercent as number)));
  }

  if (provider === 'antigravity') {
    // Buckets live one level down, inside groups; the grouping is a display
    // concern the filter does not care about.
    const buckets = (
      (quota as { groups?: { buckets?: { remainingFraction?: number | null }[] }[] }).groups ?? []
    ).flatMap((group) => group.buckets ?? []);
    return buckets
      .filter((bucket) => isFiniteNumber(bucket.remainingFraction))
      .map((bucket) => clampPercent((bucket.remainingFraction as number) * 100));
  }

  if (provider === 'kimi') {
    const rows = (quota as { rows?: { used?: number; limit?: number }[] }).rows ?? [];
    return rows
      .filter(
        (row) =>
          isFiniteNumber(row.limit) && (row.limit as number) > 0 && isFiniteNumber(row.used)
      )
      .map((row) =>
        clampPercent((((row.limit as number) - (row.used as number)) / (row.limit as number)) * 100)
      );
  }

  if (provider === 'xai') {
    const billing = (quota as { billing?: { usagePercent?: number | null } | null }).billing;
    if (!billing || !isFiniteNumber(billing.usagePercent)) return [];
    return [clampPercent(100 - billing.usagePercent)];
  }

  return [];
}

/**
 * Classify one credential's quota state.
 *
 * `loading` is reported as `unloaded`: the answer is not in yet, and saying so
 * is honest where guessing a status would put a credential in a bucket the user
 * then acts on. A `success` payload with no readable window lands there too, for
 * the same reason — it is an unknown, not a claim about capacity.
 */
export function classifyQuotaAvailability(
  provider: QuotaProviderType,
  quota: { status?: string } | undefined
): QuotaAvailability {
  if (!quota || quota.status === undefined || quota.status === 'idle') return 'unloaded';
  if (quota.status === 'error') return 'failed';
  if (quota.status !== 'success') return 'unloaded';

  const percents = collectRemainingPercents(provider, quota);
  if (percents.length === 0) return 'unloaded';

  // Any window with room left means the credential can still serve a request:
  // the windows are independent limits, not a single pooled budget.
  return percents.some((percent) => percent > 0) ? 'available' : 'exhausted';
}

export function filterEntriesByAvailability(
  entries: QuotaFileEntry[],
  filter: QuotaAvailabilityFilter,
  quotaFor: (entry: QuotaFileEntry) => { status?: string } | undefined
): QuotaFileEntry[] {
  if (filter === 'all') return entries;
  return entries.filter(
    (entry) => classifyQuotaAvailability(entry.type, quotaFor(entry)) === filter
  );
}

/**
 * How many credentials sit in each bucket, for the filter labels.
 *
 * Counted over whatever the caller was already going to render — the tab- and
 * search-filtered set — so a count always describes the list it is filtering.
 * `all` is that list's own size, not the pool's.
 */
export function buildAvailabilityCounts(
  entries: QuotaFileEntry[],
  quotaFor: (entry: QuotaFileEntry) => { status?: string } | undefined
): Record<QuotaAvailabilityFilter, number> {
  const counts: Record<QuotaAvailabilityFilter, number> = {
    all: entries.length,
    available: 0,
    exhausted: 0,
    failed: 0,
    unloaded: 0,
  };
  for (const entry of entries) {
    counts[classifyQuotaAvailability(entry.type, quotaFor(entry))] += 1;
  }
  return counts;
}
