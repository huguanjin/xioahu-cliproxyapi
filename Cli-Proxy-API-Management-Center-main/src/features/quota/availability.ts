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
import { familyWindowOf, isBucketSpent } from './providers/antigravity/familyState';

export const QUOTA_AVAILABILITY_FILTERS = [
  'all',
  'available',
  'exhausted',
  'failed',
  'unloaded',
] as const;

export type QuotaAvailabilityFilter = (typeof QUOTA_AVAILABILITY_FILTERS)[number];

/**
 * 按模型家族筛选凭证。
 *
 * Antigravity is the only provider that partitions quota by model family, and
 * the two pools are independent — a credential can be out of Gemini for the week
 * while its Claude+GPT pool is untouched. "Which credentials can still serve
 * Gemini" is therefore a real question that the availability filter cannot
 * answer: that one asks whether a credential can serve ANYTHING, and a
 * Gemini-exhausted / Claude-healthy credential correctly reads as available
 * there.
 *
 * The family filter is a separate axis rather than a sixth availability value
 * because the two compose: "has quota" AND "Gemini" is a meaningful pair, and
 * folding them into one list would make it unaskable.
 */
export const QUOTA_FAMILY_FILTERS = ['all', 'gemini', 'claude'] as const;

export type QuotaFamilyFilter = (typeof QUOTA_FAMILY_FILTERS)[number];

export const isQuotaFamilyFilter = (value: unknown): value is QuotaFamilyFilter =>
  typeof value === 'string' && (QUOTA_FAMILY_FILTERS as readonly string[]).includes(value);

/**
 * 家族筛选的细分：任一 / 周限额未耗尽 / 周限额已耗尽。
 *
 * Scoped to the WEEKLY window because that is the one that gates the family for
 * days. A spent 5-hour window clears in minutes and is worth waiting out, so
 * treating it as "this family is unavailable" would hide credentials that are
 * about to be perfectly usable.
 *
 * `hasWeeklyQuota` is answered from the payload, not from routing — see
 * familyState.ts for why those are different claims.
 */
export const QUOTA_FAMILY_SCOPE_FILTERS = [
  'all',
  'weekly_available',
  'weekly_exhausted',
] as const;

export type QuotaFamilyScopeFilter = (typeof QUOTA_FAMILY_SCOPE_FILTERS)[number];

export const isQuotaFamilyScopeFilter = (value: unknown): value is QuotaFamilyScopeFilter =>
  typeof value === 'string' && (QUOTA_FAMILY_SCOPE_FILTERS as readonly string[]).includes(value);

/**
 * 上游的分组名 → 家族。匹配是宽松的：上游标签是自由文本。
 *
 * Matched on the family words rather than the exact upstream strings, because
 * the payload's own labels are free text and a renamed group would otherwise
 * vanish from every family — silently, since an unmatched group is simply not
 * filtered.
 */
export function familyOfGroupLabel(label: string | undefined): 'gemini' | 'claude' | 'other' {
  const normalized = (label ?? '').trim().toLowerCase();
  if (!normalized) return 'other';
  if (normalized.includes('claude') || normalized.includes('gpt')) return 'claude';
  if (normalized.includes('gemini')) return 'gemini';
  return 'other';
}

/** 一个家族的周限额状态。unknown 表示没有可读的周限额桶。 */
export type FamilyWeeklyStatus = 'available' | 'exhausted' | 'unknown';

/**
 * 某个家族的周限额是否还有余量。
 *
 * Reads the family's WEEKLY bucket only. A family with several weekly buckets is
 * exhausted when ANY of them is spent — they are the same pool from the caller's
 * point of view, and reporting "available" because one bucket of several still
 * has room would be the optimistic direction, which is the wrong way to be wrong
 * when the answer decides whether a credential is filtered out.
 *
 * Unknown is returned rather than a guess: a family with no readable weekly
 * bucket is neither available nor exhausted, and both defaults are harmful —
 * "available" would leave an unknown credential in a list the operator is
 * using to find usable capacity, and "exhausted" would flag a healthy one.
 */
export function familyWeeklyStatus(
  groups: readonly { label?: string; buckets?: { window?: string; periodHours?: number | null; remainingFraction?: number | null }[] }[] | undefined,
  family: 'gemini' | 'claude'
): FamilyWeeklyStatus {
  let sawWeekly = false;
  for (const group of groups ?? []) {
    if (familyOfGroupLabel(group.label) !== family) continue;
    for (const bucket of group.buckets ?? []) {
      if (familyWindowOf(bucket) !== 'weekly') continue;
      if (typeof bucket.remainingFraction !== 'number' || !Number.isFinite(bucket.remainingFraction)) {
        continue;
      }
      sawWeekly = true;
      if (isBucketSpent(bucket)) return 'exhausted';
    }
  }
  return sawWeekly ? 'available' : 'unknown';
}

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
 * 按家族筛选。没有该家族的凭证在不选 'all' 时会被排除。
 *
 * A credential with no loaded quota counts as belonging to every family it has,
 * and an antigravity credential with no quota loaded yet cannot be classified —
 * so it is excluded from a specific family rather than guessed into one. Asking
 * "show me the Gemini credentials" and getting back credentials whose Gemini
 * state is unknown would be answering a different question.
 */
export function filterEntriesByFamily(
  entries: QuotaFileEntry[],
  family: QuotaFamilyFilter,
  quotaFor: (entry: QuotaFileEntry) => { status?: string; groups?: { label?: string }[] } | undefined,
  scope: QuotaFamilyScopeFilter = 'all'
): QuotaFileEntry[] {
  if (family === 'all') return entries;
  return entries.filter((entry) => {
    if (!entryHasFamily(entry, family, quotaFor)) return false;
    if (scope === 'all') return true;

    const status = familyWeeklyStatus(
      quotaFor(entry)?.groups as Parameters<typeof familyWeeklyStatus>[0],
      family
    );
    // unknown never satisfies either side: a family whose weekly bucket could
    // not be read is not evidence of capacity, and not evidence of exhaustion.
    return scope === 'weekly_available'
      ? status === 'available'
      : status === 'exhausted';
  });
}

/**
 * 每个家族各有多少凭证处于「周限额未耗尽 / 已耗尽」。
 *
 * Counted in one pass over the entries rather than by calling the filter twice,
 * so the numbers on the labels cannot drift from the lists they open.
 */
export function buildFamilyScopeCounts(
  entries: QuotaFileEntry[],
  quotaFor: (entry: QuotaFileEntry) => { status?: string; groups?: { label?: string }[] } | undefined,
  family: Exclude<QuotaFamilyFilter, 'all'>
): { available: number; exhausted: number } {
  let available = 0;
  let exhausted = 0;
  for (const entry of entries) {
    if (!entryHasFamily(entry, family, quotaFor)) continue;
    const status = familyWeeklyStatus(
      quotaFor(entry)?.groups as Parameters<typeof familyWeeklyStatus>[0],
      family
    );
    if (status === 'available') available += 1;
    else if (status === 'exhausted') exhausted += 1;
  }
  return { available, exhausted };
}

export function entryHasFamily(
  entry: QuotaFileEntry,
  family: Exclude<QuotaFamilyFilter, 'all'>,
  quotaFor: (entry: QuotaFileEntry) => { status?: string; groups?: { label?: string }[] } | undefined
): boolean {
  const quota = quotaFor(entry);
  if (!quota || quota.status !== 'success') return false;
  return (quota.groups ?? []).some((group) => familyOfGroupLabel(group.label) === family);
}

/** 每个家族有哪些凭证，用于筛选标签上的计数。 */
export function buildFamilyCounts(
  entries: QuotaFileEntry[],
  quotaFor: (entry: QuotaFileEntry) => { status?: string; groups?: { label?: string }[] } | undefined
): Record<QuotaFamilyFilter, number> {
  const counts: Record<QuotaFamilyFilter, number> = { all: entries.length, gemini: 0, claude: 0 };
  for (const entry of entries) {
    if (entryHasFamily(entry, 'gemini', quotaFor)) counts.gemini += 1;
    if (entryHasFamily(entry, 'claude', quotaFor)) counts.claude += 1;
  }
  return counts;
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
