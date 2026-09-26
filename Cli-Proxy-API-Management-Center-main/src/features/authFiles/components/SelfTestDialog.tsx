import { useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { Modal } from '@/components/ui/Modal';
import { ToggleSwitch } from '@/components/ui/ToggleSwitch';
import { IconAlertTriangle, IconRefreshCw } from '@/components/ui/icons';
import { authFilesApi, type SelfTestReport } from '@/services/api/authFiles';
import styles from './SelfTestDialog.module.scss';

type SelfTestDialogProps = {
  open: boolean;
  onClose: () => void;
};

/**
 * 读取自检状态时用的轮询间隔。后端一次运行可能持续数十秒（并发 8、超时 90s），
 * 所以按钮点击后仅在此弹窗打开期间轮询，避免空转。
 */
const RUNNING_POLL_INTERVAL_MS = 2500;

/** ISO 时间戳转本地可读文本；解析失败时原样返回。 */
const formatLocalTime = (value: string | undefined): string => {
  if (!value) return '';
  const parsed = Date.parse(value);
  if (Number.isNaN(parsed)) return value;
  return new Date(parsed).toLocaleString();
};

/** 单条失败记录的展示行。 */
function FailureRow({
  entry,
}: {
  entry: NonNullable<SelfTestReport['failures']>[number];
}) {
  const { t } = useTranslation();
  const isDeterministic = entry.kind === 'deterministic';
  return (
    <div className={`${styles.failure} ${isDeterministic ? styles.failureDeterministic : ''}`}>
      <div className={styles.failureHead}>
        <span className={styles.failureStatus}>{entry.status_code || '—'}</span>
        <span className={styles.failureLabel} title={entry.auth_id}>
          {entry.label || entry.auth_id}
        </span>
        <span className={styles.failureStrikes}>
          {t('auth_files.selftest_strikes', { count: entry.strikes })}
        </span>
      </div>
      <div className={styles.failureMeta}>
        <span className={styles.failureKind}>
          {isDeterministic
            ? t('auth_files.selftest_kind_deterministic')
            : t('auth_files.selftest_kind_transient')}
        </span>
        {entry.cooldown_until && (
          <span className={styles.failureCooldown}>
            {t('auth_files.selftest_cooldown_until', {
              time: formatLocalTime(entry.cooldown_until),
            })}
          </span>
        )}
      </div>
      {entry.message && <div className={styles.failureMessage}>{entry.message}</div>}
    </div>
  );
}

/**
 * 凭证批量测试弹窗：手动触发一次全池探测，展示后端返回的可用性汇总。
 *
 * 后端接口为同步返回——一次运行可能耗时数十秒，因此按钮进入 loading 并禁用，
 * 同时轮询状态接口以便展示「正在运行」。定时开关同样在此控制，关闭定时后
 * 手动触发依然可用（后端循环不会因此停摆）。
 */
export function SelfTestDialog({ open, onClose }: SelfTestDialogProps) {
  const { t } = useTranslation();
  const [report, setReport] = useState<SelfTestReport | null>(null);
  const [scheduleEnabled, setScheduleEnabled] = useState(false);
  const [loadingStatus, setLoadingStatus] = useState(false);
  const [running, setRunning] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [schedulePending, setSchedulePending] = useState(false);

  const loadStatus = useCallback(async () => {
    setLoadingStatus(true);
    try {
      const payload = await authFilesApi.getSelfTestStatus();
      setScheduleEnabled(Boolean(payload?.schedule_enabled));
      setReport(payload?.last_report ?? null);
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : t('auth_files.selftest_run_failed'));
    } finally {
      setLoadingStatus(false);
    }
  }, [t]);

  useEffect(() => {
    if (!open) return;
    void loadStatus();
  }, [open, loadStatus]);

  // 运行期间轮询状态：后端是同步接口，这里只用于确认仍在进行并在结束后刷新报告。
  useEffect(() => {
    if (!open || !running) return;
    const timer = window.setInterval(() => {
      void loadStatus();
    }, RUNNING_POLL_INTERVAL_MS);
    return () => window.clearInterval(timer);
  }, [open, running, loadStatus]);

  const handleRun = useCallback(async () => {
    setRunning(true);
    setError(null);
    try {
      const payload = await authFilesApi.runSelfTest();
      setReport(payload ?? null);
    } catch (err) {
      const status = (err as { status?: number })?.status;
      setError(
        status === 409
          ? t('auth_files.selftest_already_running')
          : err instanceof Error
            ? err.message
            : t('auth_files.selftest_run_failed')
      );
    } finally {
      setRunning(false);
    }
  }, [t]);

  const handleToggleSchedule = useCallback(
    async (next: boolean) => {
      setSchedulePending(true);
      setError(null);
      try {
        const payload = await authFilesApi.setSelfTestSchedule(next);
        setScheduleEnabled(Boolean(payload?.schedule_enabled));
      } catch (err) {
        setError(err instanceof Error ? err.message : t('auth_files.selftest_run_failed'));
      } finally {
        setSchedulePending(false);
      }
    },
    [t]
  );

  const failures = report?.failures ?? [];

  return (
    <Modal
      open={open}
      onClose={onClose}
      title={t('auth_files.selftest_title')}
      width={640}
      closeDisabled={running}
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={running}>
            {t('auth_files.selftest_close')}
          </Button>
          <Button onClick={handleRun} disabled={running || loadingStatus}>
            {running ? <LoadingSpinner size={14} /> : <IconRefreshCw size={14} />}
            {running ? t('auth_files.selftest_running') : t('auth_files.selftest_run_button')}
          </Button>
        </>
      }
    >
      <div className={styles.body}>
        <p className={styles.description}>{t('auth_files.selftest_description')}</p>

        <div className={styles.scheduleRow}>
          <span className={styles.scheduleLabel}>{t('auth_files.selftest_schedule_label')}</span>
          <ToggleSwitch
            checked={scheduleEnabled}
            onChange={(next) => void handleToggleSchedule(next)}
            ariaLabel={t('auth_files.selftest_schedule_label')}
          />
          <span className={styles.scheduleState}>
            {scheduleEnabled
              ? t('auth_files.selftest_schedule_enabled')
              : t('auth_files.selftest_schedule_disabled')}
          </span>
          {schedulePending && <LoadingSpinner size={13} />}
        </div>

        {error && (
          <div className={styles.errorBanner} role="alert">
            <IconAlertTriangle size={14} />
            <span>{error}</span>
          </div>
        )}

        {!report && !loadingStatus && (
          <div className={styles.empty}>{t('auth_files.selftest_never_run')}</div>
        )}

        {report && (
          <>
            <div className={styles.meta}>
              <span>{t('auth_files.selftest_concurrency', { count: report.concurrency })}</span>
              <span>{t('auth_files.selftest_finished_at', { time: formatLocalTime(report.finished_at) })}</span>
            </div>

            <div className={styles.summaryGrid}>
              <div className={styles.summaryCell}>
                <span className={styles.summaryValue}>{report.probed}</span>
                <span className={styles.summaryLabel}>
                  {t('auth_files.selftest_summary_probed')}
                </span>
              </div>
              <div className={styles.summaryCell}>
                <span className={styles.summaryValue}>{report.skipped}</span>
                <span className={styles.summaryLabel}>
                  {t('auth_files.selftest_summary_skipped')}
                </span>
              </div>
              <div className={`${styles.summaryCell} ${styles.summaryHealthy}`}>
                <span className={styles.summaryValue}>{report.healthy}</span>
                <span className={styles.summaryLabel}>
                  {t('auth_files.selftest_summary_healthy')}
                </span>
              </div>
              <div className={`${styles.summaryCell} ${styles.summaryCooling}`}>
                <span className={styles.summaryValue}>{report.cooling}</span>
                <span className={styles.summaryLabel}>
                  {t('auth_files.selftest_summary_cooling')}
                </span>
              </div>
              <div className={styles.summaryCell}>
                <span className={styles.summaryValue}>{report.deterministic}</span>
                <span className={styles.summaryLabel}>
                  {t('auth_files.selftest_summary_deterministic')}
                </span>
              </div>
              <div className={`${styles.summaryCell} ${styles.summaryEscalated}`}>
                <span className={styles.summaryValue}>{report.escalated}</span>
                <span className={styles.summaryLabel}>
                  {t('auth_files.selftest_summary_escalated')}
                </span>
              </div>
              <div className={styles.summaryCell}>
                <span className={styles.summaryValue}>{report.transient}</span>
                <span className={styles.summaryLabel}>
                  {t('auth_files.selftest_summary_transient')}
                </span>
              </div>
            </div>

            {failures.length > 0 && (
              <div className={styles.failures}>
                <div className={styles.failuresTitle}>
                  {t('auth_files.selftest_failures_title', { count: failures.length })}
                </div>
                {failures.map((entry, index) => (
                  <FailureRow key={`${entry.auth_id}-${index}`} entry={entry} />
                ))}
              </div>
            )}
          </>
        )}
      </div>
    </Modal>
  );
}
