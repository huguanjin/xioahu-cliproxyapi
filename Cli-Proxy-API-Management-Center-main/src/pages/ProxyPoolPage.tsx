import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Card } from '@/components/ui/Card';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import { EmptyState } from '@/components/ui/EmptyState';
import { Modal } from '@/components/ui/Modal';
import { IconPencil, IconPlus, IconSearch, IconTrash2 } from '@/components/ui/icons';
import { SecondaryScreenShell } from '@/components/common/SecondaryScreenShell';
import { useEdgeSwipeBack } from '@/hooks/useEdgeSwipeBack';
import { useAuthStore, useNotificationStore } from '@/stores';
import {
  proxyPoolApi,
  type ProxyImportReport,
  type ProxyPoolEntry,
  type ProxyProbeResult,
} from '@/services/api';
import { getErrorMessage } from '@/utils/helpers';
import { entryMatchesProxySearch } from '@/utils/proxyFilter';
import styles from './ProxyPoolPage.module.scss';

type EntryFormState = {
  name: string;
  proxyUrl: string;
  account: string;
  exitIp: string;
  notes: string;
};

const EMPTY_FORM: EntryFormState = {
  name: '',
  proxyUrl: '',
  account: '',
  exitIp: '',
  notes: '',
};

function entryToForm(entry: ProxyPoolEntry): EntryFormState {
  return {
    name: entry.name ?? '',
    proxyUrl: entry['proxy-url'] ?? '',
    account: entry.account ?? '',
    exitIp: entry['exit-ip'] ?? '',
    notes: entry.notes ?? '',
  };
}

export function ProxyPoolPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const { showConfirmation, showNotification } = useNotificationStore();
  const connectionStatus = useAuthStore((state) => state.connectionStatus);
  const disableControls = connectionStatus !== 'connected';

  const [entries, setEntries] = useState<ProxyPoolEntry[]>([]);
  const [initialLoading, setInitialLoading] = useState(true);
  const [initialLoadError, setInitialLoadError] = useState<string | null>(null);
  const loadRequestRef = useRef(0);

  const [modalOpen, setModalOpen] = useState(false);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [form, setForm] = useState<EntryFormState>(EMPTY_FORM);
  const [saving, setSaving] = useState(false);
  const [search, setSearch] = useState('');

  // 连通性测试结果只在本页内存里保留：代理是否可用是即时状态，不写入配置。
  const [probeResults, setProbeResults] = useState<Record<string, ProxyProbeResult>>({});
  const [testingIds, setTestingIds] = useState<Set<string>>(new Set());
  const [testingAll, setTestingAll] = useState(false);

  const runProbe = useCallback(
    async (target: { id: string } | { all: true }) => {
      const ids = 'all' in target ? entries.map((entry) => entry.id) : [target.id];
      if ('all' in target) {
        setTestingAll(true);
      }
      setTestingIds((prev) => new Set([...prev, ...ids]));
      try {
        const results = await proxyPoolApi.test(target);
        setProbeResults((prev) => {
          const next = { ...prev };
          results.forEach((result) => {
            next[result.id] = result;
          });
          return next;
        });
        if ('all' in target) {
          const okCount = results.filter((result) => result.ok).length;
          showNotification(
            t('proxy_pool.test_all_done', { ok: okCount, total: results.length }),
            okCount === results.length ? 'success' : 'warning'
          );
        }
      } catch (err: unknown) {
        showNotification(`${t('proxy_pool.test_failed')}: ${getErrorMessage(err)}`, 'error');
      } finally {
        setTestingIds((prev) => {
          const next = new Set(prev);
          ids.forEach((id) => next.delete(id));
          return next;
        });
        if ('all' in target) {
          setTestingAll(false);
        }
      }
    },
    [entries, showNotification, t]
  );

  const handleBack = useCallback(() => {
    navigate('/config');
  }, [navigate]);

  const swipeRef = useEdgeSwipeBack({ onBack: handleBack });

  const loadEntries = useCallback(async () => {
    const requestId = ++loadRequestRef.current;
    setInitialLoading(true);
    setInitialLoadError(null);
    try {
      const list = await proxyPoolApi.list();
      if (requestId !== loadRequestRef.current) return;
      setEntries(list);
    } catch (err: unknown) {
      if (requestId === loadRequestRef.current) {
        setInitialLoadError(getErrorMessage(err));
      }
    } finally {
      if (requestId === loadRequestRef.current) {
        setInitialLoading(false);
      }
    }
  }, []);

  useEffect(() => {
    void loadEntries();
    return () => {
      loadRequestRef.current += 1;
    };
  }, [loadEntries]);

  const [importOpen, setImportOpen] = useState(false);
  const [importText, setImportText] = useState('');
  const [importPrefix, setImportPrefix] = useState('');
  const [importing, setImporting] = useState(false);
  const [importInvalid, setImportInvalid] = useState<ProxyImportReport['invalid']>([]);
  const importFileRef = useRef<HTMLInputElement | null>(null);

  const openImport = useCallback(() => {
    setImportText('');
    setImportPrefix('');
    setImportInvalid([]);
    setImportOpen(true);
  }, []);

  const closeImport = useCallback(() => {
    if (importing) return;
    setImportOpen(false);
  }, [importing]);

  // 选文件只是把内容读进文本框，让用户导入前能看到、改到；真正的解析在后端。
  const handleImportFile = useCallback(
    async (event: React.ChangeEvent<HTMLInputElement>) => {
      const file = event.target.files?.[0];
      event.target.value = '';
      if (!file) return;
      try {
        const text = await file.text();
        setImportText((prev) => (prev.trim() ? `${prev.replace(/\s+$/, '')}\n${text}` : text));
      } catch {
        showNotification(t('proxy_pool.import_file_failed'), 'error');
      }
    },
    [showNotification, t]
  );

  const handleImport = useCallback(async () => {
    if (!importText.trim()) {
      showNotification(t('proxy_pool.import_empty'), 'error');
      return;
    }
    setImporting(true);
    try {
      const report = await proxyPoolApi.import(importText, importPrefix);
      const invalid = report.invalid ?? [];
      const message =
        t('proxy_pool.import_done', { added: report.added, duplicate: report.duplicate }) +
        (invalid.length > 0 ? t('proxy_pool.import_done_invalid', { invalid: invalid.length }) : '');
      showNotification(message, invalid.length > 0 ? 'warning' : 'success');
      await loadEntries();
      if (invalid.length > 0) {
        // 留在弹窗里显示无效行，方便改完再导。
        setImportInvalid(invalid);
        setImportText(
          importText
            .split('\n')
            .filter((_, index) => invalid.some((item) => item.line === index + 1))
            .join('\n')
        );
      } else {
        setImportOpen(false);
      }
    } catch (err: unknown) {
      showNotification(`${t('proxy_pool.import_failed')}: ${getErrorMessage(err)}`, 'error');
    } finally {
      setImporting(false);
    }
  }, [importPrefix, importText, loadEntries, showNotification, t]);

  const openCreate = useCallback(() => {
    setEditingId(null);
    setForm(EMPTY_FORM);
    setModalOpen(true);
  }, []);

  const openEdit = useCallback((entry: ProxyPoolEntry) => {
    setEditingId(entry.id);
    setForm(entryToForm(entry));
    setModalOpen(true);
  }, []);

  const closeModal = useCallback(() => {
    if (saving) return;
    setModalOpen(false);
  }, [saving]);

  const handleSave = useCallback(async () => {
    const proxyUrl = form.proxyUrl.trim();
    if (!proxyUrl) {
      showNotification(t('proxy_pool.proxy_url_required'), 'error');
      return;
    }

    setSaving(true);
    try {
      if (editingId) {
        await proxyPoolApi.update(editingId, {
          name: form.name.trim(),
          'proxy-url': proxyUrl,
          account: form.account.trim(),
          'exit-ip': form.exitIp.trim(),
          notes: form.notes.trim(),
        });
      } else {
        const nextEntries: ProxyPoolEntry[] = [
          ...entries,
          {
            id: '',
            name: form.name.trim(),
            'proxy-url': proxyUrl,
            account: form.account.trim(),
            'exit-ip': form.exitIp.trim(),
            notes: form.notes.trim(),
          },
        ];
        await proxyPoolApi.replace(nextEntries);
      }
      showNotification(t('proxy_pool.save_success'), 'success');
      setModalOpen(false);
      await loadEntries();
    } catch (err: unknown) {
      showNotification(`${t('proxy_pool.save_failed')}: ${getErrorMessage(err)}`, 'error');
    } finally {
      setSaving(false);
    }
  }, [editingId, entries, form, loadEntries, showNotification, t]);

  const handleDelete = useCallback(
    (entry: ProxyPoolEntry) => {
      showConfirmation({
        title: t('proxy_pool.delete_confirm_title'),
        message: t('proxy_pool.delete_confirm', {
          name: entry.name || entry['proxy-url'],
        }),
        variant: 'danger',
        confirmText: t('common.delete'),
        cancelText: t('common.cancel'),
        onConfirm: async () => {
          try {
            await proxyPoolApi.delete(entry.id);
            showNotification(t('proxy_pool.delete_success'), 'success');
            await loadEntries();
          } catch (err: unknown) {
            showNotification(`${t('proxy_pool.delete_failed')}: ${getErrorMessage(err)}`, 'error');
          }
        },
      });
    },
    [loadEntries, showConfirmation, showNotification, t]
  );

  const isEditing = editingId !== null;
  const modalTitle = isEditing ? t('proxy_pool.edit_title') : t('proxy_pool.add_title');

  const sortedEntries = useMemo(
    () =>
      [...entries]
        .filter((entry) => entryMatchesProxySearch(entry, search))
        .sort((a, b) => (a.name || a['proxy-url']).localeCompare(b.name || b['proxy-url'])),
    [entries, search]
  );

  return (
    <SecondaryScreenShell
      ref={swipeRef}
      title={t('proxy_pool.title')}
      onBack={handleBack}
      backLabel={t('common.back')}
      backAriaLabel={t('common.back')}
      contentClassName={styles.pageContent}
      rightAction={
        <div className={styles.headerActions}>
          <Button
            variant="secondary"
            size="sm"
            onClick={() => void runProbe({ all: true })}
            loading={testingAll}
            disabled={disableControls || entries.length === 0}
          >
            {t('proxy_pool.test_all')}
          </Button>
          <Button variant="secondary" size="sm" onClick={openImport} disabled={disableControls}>
            {t('proxy_pool.import')}
          </Button>
          <Button size="sm" onClick={openCreate} disabled={disableControls}>
            <IconPlus size={16} />
            {t('proxy_pool.add')}
          </Button>
        </div>
      }
      isLoading={initialLoading}
      loadingLabel={t('common.loading')}
    >
      {initialLoadError !== null ? (
        <Card>
          <EmptyState
            title={t('notification.refresh_failed')}
            description={initialLoadError || t('notification.refresh_failed')}
            action={
              <Button variant="secondary" size="sm" onClick={() => void loadEntries()}>
                {t('common.refresh')}
              </Button>
            }
          />
        </Card>
      ) : entries.length === 0 ? (
        <Card>
          <EmptyState
            title={t('proxy_pool.list_empty')}
            description={t('proxy_pool.description')}
            action={
              <Button size="sm" onClick={openCreate} disabled={disableControls}>
                {t('proxy_pool.add')}
              </Button>
            }
          />
        </Card>
      ) : (
        <>
        <div className={styles.searchRow}>
          <IconSearch className={styles.searchIcon} size={15} />
          <input
            className={styles.searchInput}
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder={t('proxy_pool.search_placeholder')}
            aria-label={t('proxy_pool.search_placeholder')}
            spellCheck={false}
          />
        </div>
        {sortedEntries.length === 0 && (
          <Card>
            <EmptyState title={t('proxy_pool.search_empty')} />
          </Card>
        )}
        <div className={styles.entryList}>
          {sortedEntries.map((entry) => (
            <Card key={entry.id} className={styles.entryCard}>
              <div className={styles.entryRow}>
                <div className={styles.entryInfo}>
                  <div className={styles.entryName}>{entry.name || entry['proxy-url']}</div>
                  <div className={styles.entryUrl}>{entry['proxy-url']}</div>
                  <div className={styles.entryStatus}>
                    <span className={styles.boundBadge}>
                      {t('proxy_pool.bound_count', { count: entry['bound-count'] ?? 0 })}
                    </span>
                    {probeResults[entry.id] && (
                      <span
                        className={probeResults[entry.id].ok ? styles.probeOk : styles.probeFail}
                        title={probeResults[entry.id].error}
                      >
                        {probeResults[entry.id].ok
                          ? t('proxy_pool.probe_ok', {
                              ms: probeResults[entry.id]['latency-ms'],
                            })
                          : t('proxy_pool.probe_fail', {
                              error: probeResults[entry.id].error ?? '',
                            })}
                      </span>
                    )}
                  </div>
                  {(entry.account || entry['exit-ip'] || entry.notes) && (
                    <div className={styles.entryMeta}>
                      {entry.account && (
                        <span>
                          {t('proxy_pool.account_label')}: {entry.account}
                        </span>
                      )}
                      {entry['exit-ip'] && (
                        <span>
                          {t('proxy_pool.exit_ip_label')}: {entry['exit-ip']}
                        </span>
                      )}
                      {entry.notes && <span>{entry.notes}</span>}
                    </div>
                  )}
                </div>
                <div className={styles.entryActions}>
                  <Button
                    variant="secondary"
                    size="sm"
                    onClick={() => void runProbe({ id: entry.id })}
                    loading={testingIds.has(entry.id)}
                    disabled={disableControls || testingAll}
                  >
                    {t('proxy_pool.test')}
                  </Button>
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => openEdit(entry)}
                    disabled={disableControls}
                    aria-label={t('common.edit')}
                  >
                    <IconPencil size={16} />
                  </Button>
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => handleDelete(entry)}
                    disabled={disableControls}
                    aria-label={t('common.delete')}
                  >
                    <IconTrash2 size={16} />
                  </Button>
                </div>
              </div>
            </Card>
          ))}
        </div>
        </>
      )}

      <Modal
        open={importOpen}
        title={t('proxy_pool.import_title')}
        onClose={closeImport}
        closeDisabled={importing}
        footer={
          <>
            <Button variant="secondary" onClick={closeImport} disabled={importing}>
              {t('common.cancel')}
            </Button>
            <Button onClick={() => void handleImport()} loading={importing}>
              {t('proxy_pool.import_submit')}
            </Button>
          </>
        }
      >
        <p className={styles.importHint}>{t('proxy_pool.import_hint')}</p>
        <textarea
          className={styles.importTextarea}
          value={importText}
          onChange={(e) => setImportText(e.target.value)}
          placeholder={t('proxy_pool.import_placeholder')}
          disabled={importing}
          spellCheck={false}
          rows={10}
          aria-label={t('proxy_pool.import_title')}
        />
        <input
          ref={importFileRef}
          type="file"
          accept=".txt,text/plain"
          style={{ display: 'none' }}
          onChange={(e) => void handleImportFile(e)}
        />
        <div>
          <Button
            variant="secondary"
            size="sm"
            onClick={() => importFileRef.current?.click()}
            disabled={importing}
          >
            {t('proxy_pool.import_file')}
          </Button>
        </div>
        <Input
          label={t('proxy_pool.import_prefix')}
          value={importPrefix}
          onChange={(e) => setImportPrefix(e.target.value)}
          disabled={importing}
        />
        {importInvalid && importInvalid.length > 0 && (
          <div className={styles.importInvalid} role="alert">
            <strong>{t('proxy_pool.import_invalid_title')}</strong>
            <ul>
              {importInvalid.slice(0, 20).map((item) => (
                <li key={item.line}>
                  {t('proxy_pool.import_invalid_line', { line: item.line, error: item.error })}
                </li>
              ))}
            </ul>
          </div>
        )}
      </Modal>

      <Modal
        open={modalOpen}
        title={modalTitle}
        onClose={closeModal}
        closeDisabled={saving}
        footer={
          <>
            <Button variant="secondary" onClick={closeModal} disabled={saving}>
              {t('common.cancel')}
            </Button>
            <Button onClick={handleSave} loading={saving}>
              {t('proxy_pool.save')}
            </Button>
          </>
        }
      >
        <Input
          label={t('proxy_pool.name_label')}
          value={form.name}
          onChange={(e) => setForm((prev) => ({ ...prev, name: e.target.value }))}
          disabled={saving}
        />
        <Input
          label={t('proxy_pool.proxy_url_label')}
          placeholder={t('proxy_pool.proxy_url_placeholder')}
          hint={t('proxy_pool.proxy_url_hint')}
          value={form.proxyUrl}
          onChange={(e) => setForm((prev) => ({ ...prev, proxyUrl: e.target.value }))}
          disabled={saving}
        />
        <Input
          label={t('proxy_pool.account_label')}
          value={form.account}
          onChange={(e) => setForm((prev) => ({ ...prev, account: e.target.value }))}
          hint={t('proxy_pool.account_hint')}
          disabled={saving}
        />
        <Input
          label={t('proxy_pool.exit_ip_label')}
          value={form.exitIp}
          onChange={(e) => setForm((prev) => ({ ...prev, exitIp: e.target.value }))}
          hint={t('proxy_pool.exit_ip_hint')}
          disabled={saving}
        />
        <Input
          label={t('proxy_pool.notes_label')}
          value={form.notes}
          onChange={(e) => setForm((prev) => ({ ...prev, notes: e.target.value }))}
          disabled={saving}
        />
      </Modal>
    </SecondaryScreenShell>
  );
}
