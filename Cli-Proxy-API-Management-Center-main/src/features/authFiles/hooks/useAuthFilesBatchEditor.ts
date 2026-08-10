import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { authFilesApi, type AuthFileFieldsPatch } from '@/services/api';
import { useNotificationStore } from '@/stores';
import { parsePriorityValue } from '@/features/authFiles/constants';
import {
  parseCredentialWeightText,
  validateCredentialWeightText,
} from '@/utils/credentialWeight';

export type BatchEditorField = 'proxyUrl' | 'priority' | 'weight' | 'disableCooling' | 'note';

export type BatchEditorState = {
  saving: boolean;
  targetNames: string[];
  enabled: Record<BatchEditorField, boolean>;
  proxyUrl: string;
  priority: string;
  priorityError: string | null;
  weight: string;
  weightError: string | null;
  disableCooling: boolean;
  note: string;
};

export type UseAuthFilesBatchEditorOptions = {
  disableControls: boolean;
  loadFiles: () => Promise<void>;
  deselectAll: () => void;
};

export type UseAuthFilesBatchEditorResult = {
  batchEditor: BatchEditorState | null;
  batchEditorDirty: boolean;
  openBatchEditor: (names: string[]) => void;
  closeBatchEditor: () => void;
  handleBatchEditorToggleField: (field: BatchEditorField, enabled: boolean) => void;
  handleBatchEditorChange: (field: BatchEditorField, value: string | boolean) => void;
  handleBatchEditorSave: () => Promise<void>;
};

const createInitialState = (names: string[]): BatchEditorState => ({
  saving: false,
  targetNames: names,
  enabled: {
    proxyUrl: false,
    priority: false,
    weight: false,
    disableCooling: false,
    note: false,
  },
  proxyUrl: '',
  priority: '',
  priorityError: null,
  weight: '',
  weightError: null,
  disableCooling: false,
  note: '',
});

export function useAuthFilesBatchEditor(
  options: UseAuthFilesBatchEditorOptions
): UseAuthFilesBatchEditorResult {
  const { loadFiles, deselectAll } = options;
  const { t } = useTranslation();
  const showNotification = useNotificationStore((state) => state.showNotification);
  const [batchEditor, setBatchEditor] = useState<BatchEditorState | null>(null);

  const batchEditorDirty = Boolean(
    batchEditor && Object.values(batchEditor.enabled).some(Boolean)
  );

  const openBatchEditor = (names: string[]) => {
    if (names.length === 0) return;
    setBatchEditor(createInitialState(names));
  };

  const closeBatchEditor = () => {
    setBatchEditor(null);
  };

  const handleBatchEditorToggleField = (field: BatchEditorField, enabled: boolean) => {
    setBatchEditor((prev) => {
      if (!prev) return prev;
      return { ...prev, enabled: { ...prev.enabled, [field]: enabled } };
    });
  };

  const handleBatchEditorChange = (field: BatchEditorField, value: string | boolean) => {
    setBatchEditor((prev) => {
      if (!prev) return prev;
      switch (field) {
        case 'proxyUrl':
          return { ...prev, proxyUrl: String(value) };
        case 'priority': {
          const priority = String(value);
          const priorityError =
            priority.trim() && parsePriorityValue(priority) === undefined
              ? t('auth_files.priority_hint')
              : null;
          return { ...prev, priority, priorityError };
        }
        case 'weight': {
          const weight = String(value);
          const weightErrorKey = validateCredentialWeightText(weight);
          const weightError = weightErrorKey
            ? t(
                weightErrorKey === 'max'
                  ? 'auth_files.weight_invalid_max'
                  : 'auth_files.weight_invalid_integer'
              )
            : null;
          return { ...prev, weight, weightError };
        }
        case 'disableCooling':
          return { ...prev, disableCooling: Boolean(value) };
        case 'note':
          return { ...prev, note: String(value) };
        default:
          return prev;
      }
    });
  };

  const buildBatchFieldsPatch = (editor: BatchEditorState): AuthFileFieldsPatch => {
    const patch: AuthFileFieldsPatch = {};

    if (editor.enabled.proxyUrl) {
      patch.proxy_url = editor.proxyUrl.trim();
    }
    if (editor.enabled.priority) {
      const priority = parsePriorityValue(editor.priority.trim());
      if (priority === undefined) {
        throw new Error(t('auth_files.priority_hint'));
      }
      patch.priority = priority;
    }
    if (editor.enabled.weight) {
      const weightErrorKey = validateCredentialWeightText(editor.weight);
      if (weightErrorKey) {
        throw new Error(
          t(
            weightErrorKey === 'max'
              ? 'auth_files.weight_invalid_max'
              : 'auth_files.weight_invalid_integer'
          )
        );
      }
      const weight = parseCredentialWeightText(editor.weight);
      patch.weight = weight === undefined ? null : weight;
    }
    if (editor.enabled.disableCooling) {
      patch.disable_cooling = editor.disableCooling;
    }
    if (editor.enabled.note) {
      patch.note = editor.note.trim();
    }

    return patch;
  };

  const handleBatchEditorSave = async () => {
    if (!batchEditor) return;
    if (!batchEditorDirty) return;

    let payload: AuthFileFieldsPatch;
    try {
      payload = buildBatchFieldsPatch(batchEditor);
    } catch (err: unknown) {
      const errorMessage = err instanceof Error ? err.message : 'Invalid format';
      showNotification(errorMessage, 'error');
      return;
    }
    if (Object.keys(payload).length === 0) return;

    const targetNames = batchEditor.targetNames;
    setBatchEditor((prev) => (prev ? { ...prev, saving: true } : prev));

    try {
      const result = await authFilesApi.batchPatchFields(targetNames, payload);
      const successCount = result.updated;
      const failCount = result.failed.length;

      if (failCount === 0) {
        showNotification(t('auth_files.batch_edit_success', { count: successCount }), 'success');
      } else {
        showNotification(
          t('auth_files.batch_edit_partial', { success: successCount, failed: failCount }),
          'warning'
        );
      }

      await loadFiles();
      deselectAll();
      setBatchEditor(null);
    } catch (err: unknown) {
      const errorMessage = err instanceof Error ? err.message : '';
      showNotification(`${t('notification.update_failed')}: ${errorMessage}`, 'error');
      setBatchEditor((prev) => (prev ? { ...prev, saving: false } : prev));
    }
  };

  return {
    batchEditor,
    batchEditorDirty,
    openBatchEditor,
    closeBatchEditor,
    handleBatchEditorToggleField,
    handleBatchEditorChange,
    handleBatchEditorSave,
  };
}
