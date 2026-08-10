import { useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { Sheet } from '@/components/ui/Sheet';
import { Button } from '@/components/ui/Button';
import { Input } from '@/components/ui/Input';
import { AutocompleteInput } from '@/components/ui/AutocompleteInput';
import { ToggleSwitch } from '@/components/ui/ToggleSwitch';
import { useNotificationStore } from '@/stores';
import { useProxyPoolOptions } from '@/hooks/useProxyPoolOptions';
import type {
  BatchEditorField,
  BatchEditorState,
} from '@/features/authFiles/hooks/useAuthFilesBatchEditor';
import { MAX_CREDENTIAL_WEIGHT } from '@/utils/credentialWeight';
import styles from './AuthFileBatchEditSheet.module.scss';

export type AuthFileBatchEditSheetProps = {
  disableControls: boolean;
  editor: BatchEditorState | null;
  dirty: boolean;
  onClose: () => void;
  onSave: () => void;
  onToggleField: (field: BatchEditorField, enabled: boolean) => void;
  onChange: (field: BatchEditorField, value: string | boolean) => void;
};

/**
 * 批量编辑抽屉：每个字段独立勾选启用，仅启用的字段会覆盖所有选中的认证文件。
 */
export function AuthFileBatchEditSheet(props: AuthFileBatchEditSheetProps) {
  const { t } = useTranslation();
  const { disableControls, editor, dirty, onClose, onSave, onToggleField, onChange } = props;
  const showConfirmation = useNotificationStore((state) => state.showConfirmation);
  const proxyPoolOptions = useProxyPoolOptions();

  const confirmClose = useCallback((): boolean | Promise<boolean> => {
    if (!dirty || editor?.saving === true) return true;
    return new Promise<boolean>((resolve) => {
      showConfirmation({
        title: t('providersPage.unsavedChanges.title'),
        message: t('providersPage.unsavedChanges.message'),
        variant: 'danger',
        confirmText: t('providersPage.unsavedChanges.discard'),
        cancelText: t('providersPage.unsavedChanges.keepEditing'),
        onConfirm: () => resolve(true),
        onCancel: () => resolve(false),
      });
    });
  }, [dirty, editor?.saving, showConfirmation, t]);

  const handleCancelClick = useCallback(() => {
    void Promise.resolve(confirmClose()).then((ok) => {
      if (ok) onClose();
    });
  }, [confirmClose, onClose]);

  const disabledBase = disableControls || editor?.saving === true;
  const hasValidationError = Boolean(editor?.priorityError || editor?.weightError);

  return (
    <Sheet
      open={Boolean(editor)}
      onClose={onClose}
      confirmClose={confirmClose}
      size="md"
      closeDisabled={editor?.saving === true}
      eyebrow={t('auth_files.batch_edit')}
      title={
        editor ? t('auth_files.batch_edit_title', { count: editor.targetNames.length }) : ''
      }
      description={t('auth_files.batch_edit_hint')}
      footer={
        <>
          <Button
            variant="secondary"
            onClick={handleCancelClick}
            disabled={editor?.saving === true}
          >
            {t('common.cancel')}
          </Button>
          <Button
            onClick={onSave}
            loading={editor?.saving === true}
            disabled={disabledBase || !dirty || hasValidationError}
          >
            {t('common.save')}
          </Button>
        </>
      }
    >
      {editor && (
        <div className={styles.editor}>
          <div className={styles.fields}>
            <div className={styles.fieldRow}>
              <div className={styles.fieldToggle}>
                <ToggleSwitch
                  checked={editor.enabled.proxyUrl}
                  onChange={(value) => onToggleField('proxyUrl', value)}
                  disabled={disabledBase}
                  ariaLabel={t('auth_files.batch_edit_apply_proxy_url')}
                />
              </div>
              <div className={styles.fieldBody}>
                <AutocompleteInput
                  label={t('auth_files.proxy_url_label')}
                  value={editor.proxyUrl}
                  placeholder={t('auth_files.proxy_url_placeholder')}
                  options={proxyPoolOptions}
                  disabled={disabledBase || !editor.enabled.proxyUrl}
                  onChange={(value) => onChange('proxyUrl', value)}
                />
              </div>
            </div>

            <div className={styles.fieldRow}>
              <div className={styles.fieldToggle}>
                <ToggleSwitch
                  checked={editor.enabled.priority}
                  onChange={(value) => onToggleField('priority', value)}
                  disabled={disabledBase}
                  ariaLabel={t('auth_files.batch_edit_apply_priority')}
                />
              </div>
              <div className={styles.fieldBody}>
                <Input
                  label={t('auth_files.priority_label')}
                  value={editor.priority}
                  placeholder={t('auth_files.priority_placeholder')}
                  hint={t('auth_files.priority_hint')}
                  error={editor.priorityError ?? undefined}
                  disabled={disabledBase || !editor.enabled.priority}
                  onChange={(e) => onChange('priority', e.target.value)}
                />
              </div>
            </div>

            <div className={styles.fieldRow}>
              <div className={styles.fieldToggle}>
                <ToggleSwitch
                  checked={editor.enabled.weight}
                  onChange={(value) => onToggleField('weight', value)}
                  disabled={disabledBase}
                  ariaLabel={t('auth_files.batch_edit_apply_weight')}
                />
              </div>
              <div className={styles.fieldBody}>
                <Input
                  label={t('auth_files.weight_label')}
                  type="number"
                  step="1"
                  max={MAX_CREDENTIAL_WEIGHT}
                  value={editor.weight}
                  placeholder="1"
                  hint={t('auth_files.weight_hint')}
                  error={editor.weightError ?? undefined}
                  disabled={disabledBase || !editor.enabled.weight}
                  onChange={(e) => onChange('weight', e.target.value)}
                />
              </div>
            </div>

            <div className={styles.fieldRow}>
              <div className={styles.fieldToggle}>
                <ToggleSwitch
                  checked={editor.enabled.disableCooling}
                  onChange={(value) => onToggleField('disableCooling', value)}
                  disabled={disabledBase}
                  ariaLabel={t('auth_files.batch_edit_apply_disable_cooling')}
                />
              </div>
              <div className={styles.fieldBody}>
                <div className={styles.toggleRow}>
                  <label>{t('auth_files.disable_cooling_label')}</label>
                  <ToggleSwitch
                    checked={editor.disableCooling}
                    onChange={(value) => onChange('disableCooling', value)}
                    disabled={disabledBase || !editor.enabled.disableCooling}
                    ariaLabel={t('auth_files.disable_cooling_label')}
                  />
                </div>
                <div className={styles.hint}>{t('auth_files.disable_cooling_hint')}</div>
              </div>
            </div>

            <div className={styles.fieldRow}>
              <div className={styles.fieldToggle}>
                <ToggleSwitch
                  checked={editor.enabled.note}
                  onChange={(value) => onToggleField('note', value)}
                  disabled={disabledBase}
                  ariaLabel={t('auth_files.batch_edit_apply_note')}
                />
              </div>
              <div className={styles.fieldBody}>
                <Input
                  label={t('auth_files.note_label')}
                  value={editor.note}
                  placeholder={t('auth_files.note_placeholder')}
                  hint={t('auth_files.note_hint')}
                  disabled={disabledBase || !editor.enabled.note}
                  onChange={(e) => onChange('note', e.target.value)}
                />
              </div>
            </div>
          </div>
        </div>
      )}
    </Sheet>
  );
}
