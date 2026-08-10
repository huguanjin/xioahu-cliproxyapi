import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { Card } from '@/components/ui/Card';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import { EmptyState } from '@/components/ui/EmptyState';
import { Modal } from '@/components/ui/Modal';
import { IconPencil, IconPlus, IconTrash2 } from '@/components/ui/icons';
import { SecondaryScreenShell } from '@/components/common/SecondaryScreenShell';
import { useEdgeSwipeBack } from '@/hooks/useEdgeSwipeBack';
import { useAuthStore, useNotificationStore } from '@/stores';
import { proxyPoolApi, type ProxyPoolEntry } from '@/services/api';
import { getErrorMessage } from '@/utils/helpers';
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
    () => [...entries].sort((a, b) => (a.name || a['proxy-url']).localeCompare(b.name || b['proxy-url'])),
    [entries]
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
        <Button size="sm" onClick={openCreate} disabled={disableControls}>
          <IconPlus size={16} />
          {t('proxy_pool.add')}
        </Button>
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
      ) : sortedEntries.length === 0 ? (
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
        <div className={styles.entryList}>
          {sortedEntries.map((entry) => (
            <Card key={entry.id} className={styles.entryCard}>
              <div className={styles.entryRow}>
                <div className={styles.entryInfo}>
                  <div className={styles.entryName}>{entry.name || entry['proxy-url']}</div>
                  <div className={styles.entryUrl}>{entry['proxy-url']}</div>
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
      )}

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
