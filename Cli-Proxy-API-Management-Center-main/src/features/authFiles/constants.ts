import type { TFunction } from 'i18next';
import iconAntigravity from '@/assets/icons/antigravity.svg';
import iconClaude from '@/assets/icons/claude.svg';
import iconCodex from '@/assets/icons/codex.svg';
import iconGemini from '@/assets/icons/gemini.svg';
import iconGrok from '@/assets/icons/grok.svg';
import iconGrokDark from '@/assets/icons/grok-dark.svg';
import iconIflow from '@/assets/icons/iflow.svg';
import iconKimiDark from '@/assets/icons/kimi-dark.svg';
import iconKimiLight from '@/assets/icons/kimi-light.svg';
import iconQwen from '@/assets/icons/qwen.svg';
import iconVertex from '@/assets/icons/vertex.svg';
import type { AuthFileItem, ResolvedTheme, ThemeColors } from '@/types';
import { normalizeOAuthProviderKey } from '@/utils/providerKeys';
import { parseTimestamp } from '@/utils/timestamp';
import { TYPE_COLORS } from '@/utils/quota';

export type { ResolvedTheme, ThemeColors, TypeColorSet } from '@/types';
export type AuthFileModelItem = {
  id: string;
  display_name?: string;
  type?: string;
  owned_by?: string;
};
export type AuthFileIconAsset = string | { light: string; dark: string };

export type QuotaProviderType = 'antigravity' | 'claude' | 'codex' | 'kimi' | 'xai';
export type OAuthConfigLoadError = 'loading' | 'unsupported' | 'load' | null;

export const QUOTA_PROVIDER_TYPES = new Set<QuotaProviderType>([
  'antigravity',
  'claude',
  'codex',
  'kimi',
  'xai',
]);

export const OAUTH_PROVIDER_PRESETS = [
  'vertex',
  'aistudio',
  'antigravity',
  'xai',
  'claude',
  'codex',
  'kimi',
];

const OAUTH_PROVIDER_EXCLUDES = new Set(['all', 'unknown', 'empty']);

export const MIN_CARD_PAGE_SIZE = 3;
/** 单页数量上限的默认值，用户可在显示设置里自行调整（不再写死）。 */
export const DEFAULT_MAX_CARD_PAGE_SIZE = 30;
/** 用户可调上限的安全上限，防止一次渲染过多卡片拖垮页面。 */
export const ABSOLUTE_MAX_CARD_PAGE_SIZE = 300;

export const INTEGER_STRING_PATTERN = /^[+-]?\d+$/;
export const TRUTHY_TEXT_VALUES = new Set(['true', '1', 'yes', 'y', 'on']);
export const FALSY_TEXT_VALUES = new Set(['false', '0', 'no', 'n', 'off']);
export const AUTH_FILE_WEBSOCKET_PROVIDERS = new Set(['codex', 'xai']);
export const AUTH_FILE_USING_API_PROVIDERS = new Set(['xai']);
export const AUTH_FILE_MANUAL_REFRESH_PROVIDERS = new Set([
  'antigravity',
  'claude',
  'codex',
  'kimi',
  'xai',
]);

// 标签类型颜色配置：权威版本在 @/utils/quota/constants.ts，此处仅转发
export { TYPE_COLORS } from '@/utils/quota';

export const AUTH_FILE_ICONS: Record<string, AuthFileIconAsset> = {
  antigravity: iconAntigravity,
  aistudio: iconGemini,
  claude: iconClaude,
  codex: iconCodex,
  gemini: iconGemini,
  xai: { light: iconGrok, dark: iconGrokDark },
  iflow: iconIflow,
  kimi: { light: iconKimiDark, dark: iconKimiLight },
  qwen: iconQwen,
  vertex: iconVertex,
};

/** 将用户自定义的“单页数量上限”钳制在 [MIN_CARD_PAGE_SIZE, ABSOLUTE_MAX_CARD_PAGE_SIZE] 内。 */
export const clampMaxCardPageSizeLimit = (value: number) =>
  Math.min(ABSOLUTE_MAX_CARD_PAGE_SIZE, Math.max(MIN_CARD_PAGE_SIZE, Math.round(value)));

export const clampCardPageSize = (value: number, maxPageSize: number = DEFAULT_MAX_CARD_PAGE_SIZE) =>
  Math.min(clampMaxCardPageSizeLimit(maxPageSize), Math.max(MIN_CARD_PAGE_SIZE, Math.round(value)));

export const normalizeProviderKey = normalizeOAuthProviderKey;

export const supportsAuthFileManualRefresh = (provider: unknown): boolean =>
  AUTH_FILE_MANUAL_REFRESH_PROVIDERS.has(normalizeProviderKey(String(provider ?? '')));

export const buildOAuthProviderOptions = (values: Iterable<unknown>): string[] => {
  const extraProviders = new Set<string>();

  Array.from(values).forEach((value) => {
    const key = normalizeProviderKey(String(value ?? ''));
    if (!key || OAUTH_PROVIDER_EXCLUDES.has(key)) return;
    extraProviders.add(key);
  });

  const baseSet = new Set(OAUTH_PROVIDER_PRESETS.map((value) => normalizeProviderKey(value)));
  const extraList = Array.from(extraProviders)
    .filter((value) => !baseSet.has(value))
    .sort((a, b) => a.localeCompare(b));

  return [...OAUTH_PROVIDER_PRESETS, ...extraList];
};

export const getAuthFileStatusMessage = (file: AuthFileItem): string => {
  const raw = file['status_message'] ?? file.statusMessage;
  if (typeof raw === 'string') return raw.trim();
  if (raw == null) return '';
  return String(raw).trim();
};

/** 这些 status_message 视为健康，不触发告警态。 */
export const HEALTHY_AUTH_FILE_STATUS_MESSAGES = new Set([
  'ok',
  'healthy',
  'ready',
  'success',
  'available',
]);

/** 是否存在非健康的 status_message（卡片告警态 / 谱条琥珀色共用判定）。 */
export const hasAuthFileStatusWarning = (file: AuthFileItem): boolean => {
  const message = getAuthFileStatusMessage(file);
  return Boolean(message) && !HEALTHY_AUTH_FILE_STATUS_MESSAGES.has(message.toLowerCase());
};

/**
 * 自检判定是否说明这张凭证「现在能正常调用」。
 *
 * 只有 healthy 算可用。以下三种都不算，且理由各自不同：
 * - cooling / quarantine：确定性问题，调用会失败
 * - validation：账号待验证，持有者可自行修复，但修复前调不通
 * - 无判定（从未被批量测试过）：没测过不等于可用，归为未知
 *
 * 所以这个函数只回答「已知可用」，不回答「已知不可用」——后者见
 * isAuthFileKnownBad。把未探测过的凭证混进「可用」会让筛选结果骗人。
 */
export const isAuthFileKnownHealthy = (file: AuthFileItem): boolean =>
  file.selfTestVerdict === 'healthy';

/**
 * 自检判定是否说明这张凭证「确定调不通」。
 * 与 isAuthFileKnownHealthy 互补，但两者都不含「从未探测」——那是第三种状态。
 */
export const isAuthFileKnownBad = (file: AuthFileItem): boolean => {
  const verdict = file.selfTestVerdict;
  return verdict === 'cooling' || verdict === 'quarantine' || verdict === 'validation';
};

/** 从未被批量测试过（没有自检判定）。 */
export const isAuthFileUntested = (file: AuthFileItem): boolean =>
  file.selfTestVerdict === undefined;

/**
 * 是否为需要用户处理的问题凭证。
 * 主动停用是独立状态，不应进入“问题”筛选或“删除问题凭证”的批量操作。
 *
 * 这里刻意不把自检判定算进来：该函数同时驱动「删除问题凭证」批量删除，
 * 让全池扫描的结论进入删除集合会扩大一次破坏性操作的范围。健康筛选
 * 走 isAuthFileKnownBad，两者互不影响。
 */
export const isProblemAuthFile = (file: AuthFileItem): boolean => {
  const status = typeof file.status === 'string' ? file.status.trim().toLowerCase() : '';
  if (file.disabled === true || status === 'disabled') return false;
  return file.unavailable === true || status === 'error' || hasAuthFileStatusWarning(file);
};

/**
 * 最近一次上游失败的 HTTP 状态码；没有失败过或值非法时返回 undefined。
 * 只接受 100–599，避免把后端口径外的脏值变成下拉框选项。
 */
export const getAuthFileLastStatusCode = (file: AuthFileItem): number | undefined => {
  const raw = file.lastStatusCode ?? file['last_status_code'];
  let parsed: number | undefined;
  if (typeof raw === 'number') {
    parsed = Number.isSafeInteger(raw) ? raw : undefined;
  } else if (typeof raw === 'string') {
    const trimmed = raw.trim();
    parsed = /^[+-]?\d+$/.test(trimmed) ? Number.parseInt(trimmed, 10) : undefined;
  }
  // 注意不要用裸 Number()：缺失字段会变成 NaN 并混进下拉框选项。
  if (parsed === undefined || !Number.isSafeInteger(parsed)) return undefined;
  return parsed >= 100 && parsed <= 599 ? parsed : undefined;
};

/** 收集列表里实际出现过的状态码，升序，供筛选下拉框动态生成选项。 */
export const collectAuthFileStatusCodes = (files: AuthFileItem[]): number[] => {
  const codes = new Set<number>();
  files.forEach((file) => {
    const code = getAuthFileLastStatusCode(file);
    if (code !== undefined) codes.add(code);
  });
  return Array.from(codes).sort((left, right) => left - right);
};

export const getTypeLabel = (t: TFunction, type: string): string => {
  const providerKey = normalizeProviderKey(type);
  const key = `auth_files.filter_${providerKey}`;
  const translated = t(key);
  if (translated !== key) return translated;
  if (providerKey === 'iflow') return 'iFlow';
  return type.charAt(0).toUpperCase() + type.slice(1);
};

export const getTypeColor = (type: string, resolvedTheme: ResolvedTheme): ThemeColors => {
  const set = TYPE_COLORS[normalizeProviderKey(type)] || TYPE_COLORS.unknown;
  return resolvedTheme === 'dark' && set.dark ? set.dark : set.light;
};

export const getAuthFileIcon = (type: string, resolvedTheme: ResolvedTheme): string | null => {
  const iconEntry = AUTH_FILE_ICONS[normalizeProviderKey(type)];
  if (!iconEntry) return null;
  return typeof iconEntry === 'string'
    ? iconEntry
    : resolvedTheme === 'dark'
      ? iconEntry.dark
      : iconEntry.light;
};

// 与 AI 提供商界面（PROVIDER_LOGOS 的 themeSurface）保持一致：
// 这些提供商的图标底座颜色随主题切换（浅色主题黑底，深色主题白底）
export const THEME_SURFACE_ICON_PROVIDERS = new Set(['kimi']);

export const isThemeSurfaceIconProvider = (type: string): boolean =>
  THEME_SURFACE_ICON_PROVIDERS.has(normalizeProviderKey(type));

export const getThemeSurfaceIconBackground = (resolvedTheme: ResolvedTheme): string =>
  resolvedTheme === 'dark' ? '#ffffff' : '#000000';

export const parsePriorityValue = (value: unknown): number | undefined => {
  if (typeof value === 'number') {
    return Number.isInteger(value) ? value : undefined;
  }

  if (typeof value !== 'string') return undefined;
  const trimmed = value.trim();
  if (!trimmed || !INTEGER_STRING_PATTERN.test(trimmed)) return undefined;
  const parsed = Number.parseInt(trimmed, 10);
  return Number.isSafeInteger(parsed) ? parsed : undefined;
};

export const parseDisableCoolingValue = (value: unknown): boolean | undefined => {
  if (typeof value === 'boolean') return value;
  if (typeof value === 'number' && Number.isFinite(value)) return value !== 0;
  if (typeof value !== 'string') return undefined;

  const normalized = value.trim().toLowerCase();
  if (!normalized) return undefined;
  if (TRUTHY_TEXT_VALUES.has(normalized)) return true;
  if (FALSY_TEXT_VALUES.has(normalized)) return false;
  return undefined;
};

export const readAuthFileDisableCooling = (value: Record<string, unknown>): boolean => {
  const canonical = parseDisableCoolingValue(value.disable_cooling);
  if (canonical !== undefined) return canonical;
  return parseDisableCoolingValue(value['disable-cooling']) ?? false;
};

export const supportsAuthFileWebsockets = (providerKey: string): boolean =>
  AUTH_FILE_WEBSOCKET_PROVIDERS.has(normalizeProviderKey(providerKey));

export const readAuthFileWebsockets = (value: Record<string, unknown>): boolean =>
  parseDisableCoolingValue(value.websockets ?? value.websocket) ?? false;

export const applyAuthFileWebsockets = (
  value: Record<string, unknown>,
  websockets: boolean
): Record<string, unknown> => {
  const next = { ...value };
  delete next.websocket;
  next.websockets = websockets;
  return next;
};

export const supportsAuthFileUsingApi = (providerKey: string): boolean =>
  AUTH_FILE_USING_API_PROVIDERS.has(normalizeProviderKey(providerKey));

export const readAuthFileUsingApi = (value: Record<string, unknown>): boolean =>
  parseDisableCoolingValue(value.using_api) ?? false;

export const applyAuthFileUsingApi = (
  value: Record<string, unknown>,
  usingApi: boolean
): Record<string, unknown> => ({ ...value, using_api: usingApi });

export function isRuntimeOnlyAuthFile(file: AuthFileItem): boolean {
  const raw = file['runtime_only'] ?? file.runtimeOnly;
  if (typeof raw === 'boolean') return raw;
  if (typeof raw === 'string') return raw.trim().toLowerCase() === 'true';
  return false;
}

/** 是否已绑定代理（proxy_url）。后端已下发 has_proxy 布尔字段，proxy_url 作为回退。 */
export function hasAuthFileProxy(file: AuthFileItem): boolean {
  const flag = file['has_proxy'];
  if (typeof flag === 'boolean') return flag;
  const proxyUrl = file['proxy_url'] ?? file.proxyUrl;
  return typeof proxyUrl === 'string' && proxyUrl.trim().length > 0;
}

export const formatModified = (item: AuthFileItem): string => {
  const raw = item['modtime'] ?? item.modified;
  if (!raw) return '-';
  const asNumber = Number(raw);
  const date =
    Number.isFinite(asNumber) && !Number.isNaN(asNumber)
      ? new Date(asNumber < 1e12 ? asNumber * 1000 : asNumber)
      : (parseTimestamp(raw) ?? new Date(String(raw)));
  return Number.isNaN(date.getTime()) ? '-' : date.toLocaleString();
};

// 检查模型是否被 OAuth 排除
export const isModelExcluded = (
  modelId: string,
  providerType: string,
  excluded: Record<string, string[]>
): boolean => {
  const providerKey = normalizeProviderKey(providerType);
  const excludedModels = excluded[providerKey] || excluded[providerType] || [];
  return excludedModels.some((pattern) => {
    if (pattern.includes('*')) {
      // 支持通配符匹配：先转义正则特殊字符，再将 * 视为通配符
      const regexSafePattern = pattern
        .split('*')
        .map((segment) => segment.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'))
        .join('.*');
      const regex = new RegExp(`^${regexSafePattern}$`, 'i');
      return regex.test(modelId);
    }
    return pattern.toLowerCase() === modelId.toLowerCase();
  });
};
