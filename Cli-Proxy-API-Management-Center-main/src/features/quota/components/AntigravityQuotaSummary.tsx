import { useTranslation } from 'react-i18next';
import { IconRefreshCw } from '@/components/ui/icons';
import { useNow } from '@/hooks/useNow';
import { formatRelativeInstant } from '@/utils/quota/relativeTime';
import { useAntigravityQuotaSummary } from '../hooks/useAntigravityQuotaSummary';
import styles from './AntigravityQuotaSummary.module.scss';

/**
 * Page-wide summary: how many antigravity credentials currently have
 * schedulable Gemini quota vs. Claude quota, per local cooldown state
 * (not a live upstream call — see useAntigravityQuotaSummary).
 */
export function AntigravityQuotaSummary() {
  const { t, i18n } = useTranslation();
  const { summary, isLoading, refresh } = useAntigravityQuotaSummary();
  const now = useNow();

  if (!summary || summary.total_antigravity_credentials === 0) {
    return null;
  }

  const generatedAtMs = Date.parse(summary.generated_at);
  const updatedLabel = Number.isFinite(generatedAtMs)
    ? formatRelativeInstant(generatedAtMs, now, i18n.resolvedLanguage)
    : null;

  return (
    <section className={styles.summary} aria-label={t('antigravity_quota.quota_summary_title')}>
      <div className={styles.heading}>
        <span className={styles.title}>{t('antigravity_quota.quota_summary_title')}</span>
        {updatedLabel && (
          <span className={styles.updated}>
            {t('antigravity_quota.quota_summary_updated_at', { relative: updatedLabel })}
          </span>
        )}
        <button
          type="button"
          className={styles.refreshButton}
          onClick={() => void refresh()}
          disabled={isLoading}
          aria-label={t('antigravity_quota.quota_summary_refresh')}
        >
          <IconRefreshCw size={13} className={isLoading ? styles.spinning : undefined} />
        </button>
      </div>

      <div className={styles.tiles}>
        <div className={styles.tile}>
          <span className={styles.tileLabel}>{t('antigravity_quota.quota_summary_gemini')}</span>
          <span className={styles.tileValue}>
            {summary.gemini.available}
            <span className={styles.tileValueTotal}>/{summary.gemini.total}</span>
          </span>
        </div>

        <div className={styles.tile}>
          <span className={styles.tileLabel}>{t('antigravity_quota.quota_summary_claude')}</span>
          <span className={styles.tileValue}>
            {summary.claude.available}
            <span className={styles.tileValueTotal}>/{summary.claude.total}</span>
          </span>
          {summary.claude.credits_fallback_enabled &&
            summary.claude.available_via_credits_fallback_only > 0 && (
              <span className={styles.tileHint}>
                {t('antigravity_quota.quota_summary_credits_only', {
                  count: summary.claude.available_via_credits_fallback_only,
                })}
              </span>
            )}
        </div>
      </div>
    </section>
  );
}
