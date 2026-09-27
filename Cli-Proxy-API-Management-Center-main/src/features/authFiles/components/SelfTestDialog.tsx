import { useCallback, useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';
import { Modal } from '@/components/ui/Modal';
import { ToggleSwitch } from '@/components/ui/ToggleSwitch';
import { IconAlertTriangle, IconRefreshCw } from '@/components/ui/icons';
import {
  authFilesApi,
  type SelfTestFailureEntry,
  type SelfTestProgress,
  type SelfTestReport,
} from '@/services/api/authFiles';
import styles from './SelfTestDialog.module.scss';

type SelfTestDialogProps = {
  open: boolean;
  onClose: () => void;
};

/**
 * 运行期间的轮询间隔。本轮是全池扫描（生产规模下可能持续数小时），
 * 因此只在弹窗打开时轮询，关闭或结束后立刻停止，避免空转。
 */
const RUNNING_POLL_INTERVAL_MS = 1500;

/** ISO 时间戳转本地可读文本；解析失败时原样返回。 */
const formatLocalTime = (value: string | undefined): string => {
  if (!value) return '';
  const parsed = Date.parse(value);
  if (Number.isNaN(parsed)) return value;
  return new Date(parsed).toLocaleString();
};

/** 秒数转「1时02分」/「3分05秒」/「42秒」。 */
const formatDuration = (seconds: number): string => {
  const safe = Math.max(0, Math.floor(seconds));
  const hours = Math.floor(safe / 3600);
  const minutes = Math.floor((safe % 3600) / 60);
  const secs = safe % 60;
  const pad = (value: number) => String(value).padStart(2, '0');
  if (hours > 0) return `${hours}:${pad(minutes)}:${pad(secs)}`;
  if (minutes > 0) return `${minutes}:${pad(secs)}`;
  return `${secs}s`;
};

/** 已运行的秒数；运行结束后锁定为总耗时。 */
const elapsedSeconds = (startedAt: string, finishedAt?: string): number => {
  const start = Date.parse(startedAt);
  if (Number.isNaN(start)) return 0;
  const end = finishedAt ? Date.parse(finishedAt) : Date.now();
  if (Number.isNaN(end)) return 0;
  return (end - start) / 1000;
};

/** 单条失败记录的展示行。 */
function FailureRow({ entry }: { entry: SelfTestFailureEntry }) {
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

/** 汇总网格的单元格。 */
function SummaryCell({
  value,
  label,
  variant,
}: {
  value: number;
  label: string;
  variant?: string;
}) {
  return (
    <div className={`${styles.summaryCell} ${variant ?? ''}`}>
      <span className={styles.summaryValue}>{value}</span>
      <span className={styles.summaryLabel}>{label}</span>
    </div>
  );
}

/**
 * 凭证批量测试弹窗：手动触发一次全池探测，实时展示进度，结束后保留汇总。
 *
 * 后端是异步接口（POST 返回 202），所以「进行中」完全由状态接口的 progress
 * 字段驱动：这里在运行期间轮询，把已完成/总数画成进度条。定时开关同样在此
 * 控制，关闭定时后手动触发依然可用（后端循环不会因此停摆）。
 */
export function SelfTestDialog({ open, onClose }: SelfTestDialogProps) {
  const { t } = useTranslation();
  const [report, setReport] = useState<SelfTestReport | null>(null);
  const [progress, setProgress] = useState<SelfTestProgress | null>(null);
  const [scheduleEnabled, setScheduleEnabled] = useState(false);
  const [running, setRunning] = useState(false);
  const [loadingStatus, setLoadingStatus] = useState(false);
  const [starting, setStarting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [schedulePending, setSchedulePending] = useState(false);
  // 每秒重算运行时长；轮询本身不足以让读数平滑。
  const [, setClockTick] = useState(0);
  // 运行结束后停止轮询，避免残留的定时器继续打接口。
  const wasRunningRef = useRef(false);

  const loadStatus = useCallback(async () => {
    try {
      const payload = await authFilesApi.getSelfTestStatus();
      setScheduleEnabled(Boolean(payload?.schedule_enabled));
      const nextProgress = payload?.progress ?? null;
      setProgress(nextProgress);
      setRunning(Boolean(payload?.running));
      setReport(payload?.last_report ?? null);
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : t('auth_files.selftest_run_failed'));
    }
  }, [t]);

  useEffect(() => {
    if (!open) return;
    setLoadingStatus(true);
    void loadStatus().finally(() => setLoadingStatus(false));
  }, [open, loadStatus]);

  // 运行期间轮询：进度条和计数由此推进。
  useEffect(() => {
    if (!open || !running) return;
    const timer = window.setInterval(() => {
      void loadStatus();
    }, RUNNING_POLL_INTERVAL_MS);
    return () => window.clearInterval(timer);
  }, [open, running, loadStatus]);

  // 运行时长每秒走动。
  useEffect(() => {
    if (!open || !running) return;
    const timer = window.setInterval(() => setClockTick((tick) => tick + 1), 1000);
    return () => window.clearInterval(timer);
  }, [open, running]);

  // 轮询到的 running=false 说明本轮已结束，补一次读取把最终报告取回来。
  useEffect(() => {
    if (!open) return;
    if (wasRunningRef.current && !running) {
      void loadStatus();
    }
    wasRunningRef.current = running;
  }, [open, running, loadStatus]);

  const handleRun = useCallback(async () => {
    setStarting(true);
    setError(null);
    try {
      await authFilesApi.runSelfTest();
      // 立刻置为运行中并拉一次状态，进度条不必等到下一个轮询周期才出现。
      setRunning(true);
      setProgress(null);
      void loadStatus();
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
      setStarting(false);
    }
  }, [t, loadStatus]);

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

  // 运行中优先展示进度，否则展示最近一次报告。两者字段同名，可共用一套渲染。
  const counts = running ? progress : report;
  const failures = counts?.failures ?? [];
  const total = progress?.total ?? report?.probed ?? 0;
  const completed = progress?.completed ?? report?.probed ?? 0;
  const percent = total > 0 ? Math.min(100, Math.round((completed / total) * 100)) : 0;
  const elapsed = counts
    ? elapsedSeconds(counts.started_at, running ? undefined : report?.finished_at)
    : 0;
  const eta =
    running && completed > 0 && total > completed
      ? (elapsed / completed) * (total - completed)
      : null;

  return (
    <Modal
      open={open}
      onClose={onClose}
      title={t('auth_files.selftest_title')}
      width={640}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            {t('auth_files.selftest_close')}
          </Button>
          <Button onClick={handleRun} disabled={running || starting || loadingStatus}>
            {running || starting ? <LoadingSpinner size={14} /> : <IconRefreshCw size={14} />}
            {running
              ? t('auth_files.selftest_running')
              : starting
                ? t('auth_files.selftest_starting')
                : t('auth_files.selftest_run_button')}
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

        {running && (
          <div className={styles.progressBlock}>
            <div className={styles.progressHead}>
              <span className={styles.progressLabel}>
                {t('auth_files.selftest_progress_label', { completed, total })}
              </span>
              <span className={styles.progressPercent}>{percent}%</span>
            </div>
            <div
              className={styles.progressTrack}
              role="progressbar"
              aria-valuemin={0}
              aria-valuemax={total || 100}
              aria-valuenow={completed}
              aria-label={t('auth_files.selftest_progress_label', { completed, total })}
            >
              <div
                className={styles.progressBar}
                style={{ width: `${percent}%` }}
              />
            </div>
            <div className={styles.progressMeta}>
              <span>{t('auth_files.selftest_elapsed', { time: formatDuration(elapsed) })}</span>
              <span>
                {eta === null
                  ? t('auth_files.selftest_eta_unknown')
                  : t('auth_files.selftest_eta', { time: formatDuration(eta) })}
              </span>
            </div>
          </div>
        )}

        {counts && (
          <>
            <div className={styles.meta}>
              <span>{t('auth_files.selftest_concurrency', { count: counts.concurrency })}</span>
              {running ? (
                <span>
                  {t('auth_files.selftest_started_at', { time: formatLocalTime(counts.started_at) })}
                </span>
              ) : (
                <span>
                  {t('auth_files.selftest_finished_at', {
                    time: formatLocalTime(report?.finished_at),
                  })}
                </span>
              )}
              {!running && report && (
                <span>{t('auth_files.selftest_duration', { time: formatDuration(elapsed) })}</span>
              )}
            </div>

            {running ? (
              <div className={styles.summaryGrid}>
                <SummaryCell
                  value={counts.healthy}
                  label={t('auth_files.selftest_summary_healthy')}
                />
                <SummaryCell
                  value={counts.deterministic}
                  label={t('auth_files.selftest_summary_deterministic')}
                />
                <SummaryCell
                  value={counts.cooling}
                  label={t('auth_files.selftest_summary_cooling')}
                  variant={styles.summaryCooling}
                />
                <SummaryCell
                  value={counts.escalated}
                  label={t('auth_files.selftest_summary_escalated')}
                  variant={styles.summaryEscalated}
                />
                <SummaryCell
                  value={counts.transient}
                  label={t('auth_files.selftest_summary_transient')}
                />
              </div>
            ) : (
              report && (
                <div className={styles.summaryGrid}>
                  <SummaryCell
                    value={report.probed}
                    label={t('auth_files.selftest_summary_probed')}
                  />
                  <SummaryCell
                    value={report.skipped}
                    label={t('auth_files.selftest_summary_skipped')}
                  />
                  <SummaryCell
                    value={report.healthy}
                    label={t('auth_files.selftest_summary_healthy')}
                    variant={styles.summaryHealthy}
                  />
                  <SummaryCell
                    value={report.cooling}
                    label={t('auth_files.selftest_summary_cooling')}
                    variant={styles.summaryCooling}
                  />
                  <SummaryCell
                    value={report.deterministic}
                    label={t('auth_files.selftest_summary_deterministic')}
                  />
                  <SummaryCell
                    value={report.escalated}
                    label={t('auth_files.selftest_summary_escalated')}
                    variant={styles.summaryEscalated}
                  />
                  <SummaryCell
                    value={report.transient}
                    label={t('auth_files.selftest_summary_transient')}
                  />
                </div>
              )
            )}
          </>
        )}

        {!counts && !loadingStatus && (
          <div className={styles.empty}>{t('auth_files.selftest_never_run')}</div>
        )}

        {failures.length > 0 && (
          <div className={styles.failures}>
            <div className={styles.failuresTitle}>
              {running
                ? t('auth_files.selftest_failures_live', { count: failures.length })
                : t('auth_files.selftest_failures_title', { count: failures.length })}
            </div>
            {failures.map((entry, index) => (
              <FailureRow key={`${entry.auth_id}-${index}`} entry={entry} />
            ))}
          </div>
        )}
      </div>
    </Modal>
  );
}
