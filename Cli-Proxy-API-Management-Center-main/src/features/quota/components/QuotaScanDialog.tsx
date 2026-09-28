import { useCallback, useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Modal } from '@/components/ui/Modal';
import { IconAlertTriangle, IconDownload } from '@/components/ui/icons';
import { authFilesApi } from '@/services/api';
import { useNotificationStore, useAuthStore } from '@/stores';
import { downloadBlob } from '@/utils/download';
import { notifyAuthFilesChanged } from '@/features/authFiles/authFilesEvents';
import type { TFunction } from 'i18next';
import type { AuthFileItem } from '@/types';
import type { QuotaFileEntry } from '../logic';
import { useQuotaScan, pruneQuotaForDisabled } from '../hooks/useQuotaScan';
import {
  defaultSelection,
  QUOTA_SCAN_RESULT_VERSION,
  readQuotaScanResult,
  writeQuotaScanResult,
  type QuotaScanFailure,
  type QuotaScanResult,
} from '../scanLogic';
import styles from './QuotaScanDialog.module.scss';

type QuotaScanDialogProps = {
  open: boolean;
  onClose: () => void;
  entries: QuotaFileEntry[];
  loadQuota: (targets: QuotaFileEntry[]) => Promise<void>;
};

/**
 * 一键巡检弹窗：分批扫全池 → 重试失败 → 分类 → 打包下载 / 批量停用。
 *
 * 与自检弹窗的关键区别：这个扫描跑在浏览器里（配额查询是前端逐条打
 * /api-call），所以关闭弹窗不会中断它 —— 状态留在页面而不是弹窗。但刷新
 * 页面会中断，所以结果落 sessionStorage，避免「扫完了但刷新后找不到」。
 */
export function QuotaScanDialog({ open, onClose, entries, loadQuota }: QuotaScanDialogProps) {
  const { t } = useTranslation();
  const { showNotification, showConfirmation } = useNotificationStore();
  const connectionStatus = useAuthStore((state) => state.connectionStatus);
  const disableControls = connectionStatus !== 'connected';

  const [result, setResult] = useState<QuotaScanResult | null>(() => readQuotaScanResult());
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [downloading, setDownloading] = useState(false);
  const [disabling, setDisabling] = useState(false);

  const { running, progress, run, cancel } = useQuotaScan(loadQuota);

  // 打开时把上次的结果读回来：刷新页面后仍能继续下载/停用。
  useEffect(() => {
    if (!open) return;
    const stored = readQuotaScanResult();
    setResult(stored);
    setSelected(stored ? defaultSelection(stored.failures) : new Set());
  }, [open]);

  const fileFor = useCallback(
    (name: string): AuthFileItem | undefined =>
      entries.find((entry) => entry.file.name === name)?.file,
    [entries]
  );

  const handleStart = useCallback(async () => {
    if (disableControls || entries.length === 0) return;
    setResult(null);
    setSelected(new Set());
    writeQuotaScanResult(null);

    const outcome = await run(entries, t as TFunction);
    if (outcome.cancelled) return;

    const next: QuotaScanResult = {
      version: QUOTA_SCAN_RESULT_VERSION,
      finishedAt: new Date().toISOString(),
      scanned: outcome.scanned,
      failed: outcome.failures.length,
      failures: outcome.failures,
      resolved: outcome.resolved,
    };
    setResult(next);
    setSelected(defaultSelection(next.failures));
    writeQuotaScanResult(next);
  }, [disableControls, entries, run, t]);

  const toggle = useCallback((name: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(name)) next.delete(name);
      else next.add(name);
      return next;
    });
  }, []);

  const toggleGroup = useCallback((failures: QuotaScanFailure[], on: boolean) => {
    setSelected((prev) => {
      const next = new Set(prev);
      failures.forEach((failure) => {
        if (on) next.add(failure.name);
        else next.delete(failure.name);
      });
      return next;
    });
  }, []);

  const groups = useMemo(() => {
    const failures = result?.failures ?? [];
    return {
      definitive: failures.filter((failure) => failure.kind === 'definitive'),
      unconfirmed: failures.filter((failure) => failure.kind === 'unconfirmed'),
    };
  }, [result]);

  const handleDownload = useCallback(async () => {
    const names = Array.from(selected);
    if (names.length === 0) return;
    setDownloading(true);
    try {
      const blob = await authFilesApi.downloadArchive(names, { includeEmails: true });
      downloadBlob({ filename: `auth-files-${Date.now()}.zip`, blob });
      showNotification(t('auth_files.batch_download_success', { count: names.length }), 'success');
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : '';
      showNotification(`${t('quota_management.scan_download_failed')}: ${message}`, 'error');
    } finally {
      setDownloading(false);
    }
  }, [selected, showNotification, t]);

  const handleDisable = useCallback(() => {
    const names = Array.from(selected);
    if (names.length === 0) return;

    // 停用会落盘，且自检循环此后永久跳过该凭证 —— 是这一步里唯一不可逆的动作，
    // 所以必须二次确认并说清数量。
    showConfirmation({
      title: t('quota_management.scan_disable_title'),
      message: t('quota_management.scan_disable_confirm', { count: names.length }),
      variant: 'danger',
      confirmText: t('common.confirm'),
      onConfirm: async () => {
        setDisabling(true);
        try {
          const outcome = await authFilesApi.batchPatchFields(names, { disabled: true });
          const disabledNames = outcome.files ?? [];
          if (disabledNames.length > 0) {
            pruneQuotaForDisabled(disabledNames);
            notifyAuthFilesChanged();
          }

          if (!outcome.failed || outcome.failed.length === 0) {
            showNotification(
              t('quota_management.scan_disable_success', { count: disabledNames.length }),
              'success'
            );
          } else {
            showNotification(
              t('quota_management.scan_disable_partial', {
                success: disabledNames.length,
                failed: outcome.failed.length,
              }),
              'warning'
            );
          }

          // 已停用的从结果里移除，避免重复操作同一个凭证。
          setResult((prev) => {
            if (!prev) return prev;
            const done = new Set(disabledNames);
            const next: QuotaScanResult = {
              ...prev,
              failures: prev.failures.filter((failure) => !done.has(failure.name)),
            };
            writeQuotaScanResult(next);
            return next;
          });
          setSelected((prev) => {
            const next = new Set(prev);
            disabledNames.forEach((name) => next.delete(name));
            return next;
          });
        } catch (err: unknown) {
          const message = err instanceof Error ? err.message : '';
          showNotification(`${t('notification.update_failed')}: ${message}`, 'error');
        } finally {
          setDisabling(false);
        }
      },
    });
  }, [selected, showConfirmation, showNotification, t]);

  const handleClear = useCallback(() => {
    setResult(null);
    setSelected(new Set());
    writeQuotaScanResult(null);
  }, []);

  const total = progress.total;
  const percent = total > 0 ? Math.min(100, Math.round((progress.completed / total) * 100)) : 0;

  // 「几点跑完的」。解析失败时留空而不是显示原始字符串：一个 ISO 时间戳
  // 对操作者没有意义，宁可不显示。
  const finishedLabel = useMemo(() => {
    const raw = result?.finishedAt;
    if (!raw) return '';
    const parsed = Date.parse(raw);
    if (Number.isNaN(parsed)) return '';
    return new Date(parsed).toLocaleTimeString();
  }, [result]);

  const renderGroup = (kind: 'definitive' | 'unconfirmed', failures: QuotaScanFailure[]) => {
    if (failures.length === 0) return null;
    const allSelected = failures.every((failure) => selected.has(failure.name));
    return (
      <section className={styles.group} key={kind}>
        <header className={styles.groupHead}>
          <label className={styles.groupLabel}>
            <input
              type="checkbox"
              checked={allSelected}
              onChange={(event) => toggleGroup(failures, event.target.checked)}
            />
            <span>{t(`quota_management.scan_group_${kind}`, { count: failures.length })}</span>
          </label>
        </header>
        <p className={styles.groupHint}>{t(`quota_management.scan_group_${kind}_hint`)}</p>
        <ul className={styles.failureList}>
          {failures.map((failure) => {
            const file = fileFor(failure.name);
            return (
              <li key={failure.name} className={styles.failureRow}>
                <label className={styles.failureLabel}>
                  <input
                    type="checkbox"
                    checked={selected.has(failure.name)}
                    onChange={() => toggle(failure.name)}
                  />
                  <span className={styles.failureName}>
                    {file?.email || file?.name || failure.name}
                  </span>
                </label>
                <span className={styles.failureMessage} title={failure.message}>
                  {failure.message}
                </span>
              </li>
            );
          })}
        </ul>
      </section>
    );
  };

  const footer = (
    <>
      <Button variant="secondary" size="sm" onClick={onClose}>
        {t('common.close')}
      </Button>
      {running ? (
        <Button variant="danger" size="sm" onClick={cancel}>
          {t('quota_management.scan_cancel')}
        </Button>
      ) : (
        <>
          {/* 只要有结果，就必须同时给出「再扫一次」的出口。结果会持久化到
              sessionStorage，所以关掉再打开仍是完成态 —— 没有这个按钮，弹窗
              就变成只能跑一次，唯一的出路是「清除结果」。 */}
          {result && (
            <Button
              variant="secondary"
              size="sm"
              onClick={() => void handleStart()}
              disabled={disableControls || entries.length === 0}
            >
              {t('quota_management.scan_rerun')}
            </Button>
          )}
          {result && (
            <Button variant="ghost" size="sm" onClick={handleClear}>
              {t('quota_management.scan_clear')}
            </Button>
          )}
          <Button
            variant="secondary"
            size="sm"
            onClick={() => void handleDownload()}
            disabled={disableControls || downloading || selected.size === 0}
          >
            <IconDownload size={14} />
            {t('quota_management.scan_download')}
          </Button>
          <Button
            variant="danger"
            size="sm"
            onClick={handleDisable}
            disabled={disableControls || disabling || selected.size === 0}
          >
            {t('quota_management.scan_disable')}
          </Button>
        </>
      )}
    </>
  );

  return (
    <Modal
      open={open}
      onClose={onClose}
      title={t('quota_management.scan_title')}
      footer={footer}
      width={720}
    >
      <div className={styles.body}>
        {running ? (
          <div className={styles.progressBlock}>
            <div className={styles.progressHead}>
              <span>{t('quota_management.scan_running')}</span>
              <span className={styles.progressCount}>
                {t('quota_management.scan_progress', {
                  completed: progress.completed,
                  total: progress.total,
                })}
              </span>
            </div>
            <div className={styles.track} role="progressbar" aria-valuenow={percent}>
              <span className={styles.fill} style={{ width: `${percent}%` }} />
            </div>
            <div className={styles.progressMeta}>
              {progress.retrying
                ? t('quota_management.scan_retrying')
                : t('quota_management.scan_failed_so_far', { count: progress.failed })}
              {/* 扫描跑在页面里而不是后端，所以必须说清关窗是否安全。 */}
              <span className={styles.hint}>{t('quota_management.scan_closing_hint')}</span>
            </div>
          </div>
        ) : !result ? (
          <div className={styles.intro}>
            <p>{t('quota_management.scan_description')}</p>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => void handleStart()}
              disabled={disableControls || entries.length === 0}
            >
              {t('quota_management.scan_start')}
            </Button>
          </div>
        ) : (
          <>
            <div className={styles.summary}>
              <strong>{t('quota_management.scan_done_title')}</strong>
              <span>
                {t('quota_management.scan_done_summary', {
                  scanned: result.scanned,
                  failed: result.failed,
                })}
              </span>
              {/* 结果会持久化，重新打开弹窗看到的是上一轮。带上完成时间，
                  否则一份昨天的「全部正常」看起来和刚跑完的一模一样。 */}
              {finishedLabel && (
                <span className={styles.finishedAt}>
                  {t('quota_management.scan_finished_at', { time: finishedLabel })}
                </span>
              )}
            </div>

            {/* 「0 个失败」必须能和「根本没取到数」分开。前者是所有凭证都
                有响应，后者是这批请求压根没发出去（例如 loadQuota 自身忙时
                直接返回），把它读成「全部正常」比报错更危险。 */}
            {result.resolved !== undefined && result.resolved < result.scanned && (
              <p className={styles.warning} role="alert">
                {t('quota_management.scan_incomplete', {
                  resolved: result.resolved,
                  scanned: result.scanned,
                })}
              </p>
            )}

            {result.failures.length === 0 ? (
              <p className={styles.allGood}>
                {result.resolved !== undefined && result.resolved < result.scanned
                  ? t('quota_management.scan_no_result_hint')
                  : t('quota_management.scan_no_failures')}
              </p>
            ) : (
              <>
                <div className={styles.selectionBar}>
                  <IconAlertTriangle size={14} />
                  <span>{t('quota_management.scan_selected', { count: selected.size })}</span>
                </div>
                {renderGroup('definitive', groups.definitive)}
                {renderGroup('unconfirmed', groups.unconfirmed)}
              </>
            )}
          </>
        )}
      </div>
    </Modal>
  );
}
