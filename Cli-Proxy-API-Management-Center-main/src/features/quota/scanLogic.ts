/**
 * 一键巡检的纯逻辑：分批、重试判定、失败分类。
 *
 * The sweep itself needs the network, but everything that decides *what* it does
 * — how many credentials go in a batch, which failures earn a retry, how a
 * failure is classified once retries are exhausted — is a pure function of data
 * and lives here, so it is directly testable without a browser or an upstream.
 */

import type { AuthFileItem } from '@/types';

/** 扫描的外发节奏。1690 个凭证集中请求可能触发上游限流，这两层是唯一的刹车。 */
export interface QuotaScanTuning {
  /** 每批凭证数。与额度页的页大小同量级。 */
  batchSize: number;
  /** 批与批之间的间隔，毫秒。 */
  batchIntervalMs: number;
  /** 一个失败凭证最多再重试几次。 */
  maxRetries: number;
  /** 重试之间的间隔，毫秒。 */
  retryIntervalMs: number;
}

/**
 * 默认节奏偏保守：这是对 1690 个账号打真实上游，宁可慢也不能把健康账号
 * 打成失败。重试间隔比批间隔长，因为限流窗口通常按分钟计。
 */
export const DEFAULT_QUOTA_SCAN_TUNING: QuotaScanTuning = {
  batchSize: 20,
  batchIntervalMs: 800,
  maxRetries: 2,
  retryIntervalMs: 1500,
};

/** 把条目切成批次。最后一批可以不满。 */
export function chunkEntries<T>(items: readonly T[], size: number): T[][] {
  if (!Number.isFinite(size) || size < 1) return items.length === 0 ? [] : [[...items]];
  const chunks: T[][] = [];
  for (let index = 0; index < items.length; index += size) {
    chunks.push(items.slice(index, index + size));
  }
  return chunks;
}

export const sleep = (ms: number): Promise<void> =>
  ms > 0 ? new Promise((resolve) => setTimeout(resolve, ms)) : Promise.resolve();

/**
 * 失败的两类。这个区分决定了「停用」按钮的默认勾选范围，所以它必须来自
 * 结构化判定，而不是配额页那句不透明的话。
 *
 * - `definitive` — 后端或自检已经判定这个凭证坏了：
 *   `last_error_code === "credential_invalid"`，或自检裁决为 `revoked`
 *   （刷新令牌永久失效）。这两者都不依赖配额请求的成功与否。
 * - `unconfirmed` — 其余。**默认不停用。** 配额页的
 *   `auth token refresh failed` 无法区分「令牌真死」与「一次网络抖动」——
 *   后端把 Google 503、DNS 抖动、代理轮换、60 秒超时与 invalid_grant
 *   压成同一句话，且不记日志。403 的「请检查凭证状态」里还可能混着
 *   auth_index 过期造成的误报。据此停用会误伤健康凭证。
 */
export type QuotaScanFailureKind = 'definitive' | 'unconfirmed';

/**
 * 分类一个扫描失败的凭证。
 *
 * 判据全部取自凭证列表已有的结构化字段。当列表里找不到这个凭证时判为
 * `unconfirmed`：查不到身份不足以断言它坏了，而 default 到「可停用」会
 * 让一次列表刷新把凭证停掉。
 */
export function classifyScanFailure(file: AuthFileItem | undefined): QuotaScanFailureKind {
  if (!file) return 'unconfirmed';
  if (String(file.lastErrorCode ?? '').trim() === 'credential_invalid') return 'definitive';
  if (String(file.selfTestVerdict ?? '').trim() === 'revoked') return 'definitive';
  return 'unconfirmed';
}

export interface QuotaScanFailure {
  name: string;
  kind: QuotaScanFailureKind;
  /** 首次失败时的报错，用于展示。重试成功则不会出现在结果里。 */
  message: string;
}

export function buildScanFailures(
  names: readonly string[],
  messageFor: (name: string) => string,
  fileFor: (name: string) => AuthFileItem | undefined
): QuotaScanFailure[] {
  return names.map((name) => ({
    name,
    kind: classifyScanFailure(fileFor(name)),
    message: messageFor(name),
  }));
}

/**
 * 默认勾选集合：只勾「确定失效」的那类。
 *
 * 这是这个功能里唯一不可逆的一步 —— 停用会落盘，且自检循环此后永久跳过该
 * 凭证。默认勾选范围因此收在最保守的一侧，宁可让操作者手动多勾，也不默认
 * 把可能只是抖了一下的凭证停掉。
 */
export function defaultSelection(failures: readonly QuotaScanFailure[]): Set<string> {
  return new Set(failures.filter((failure) => failure.kind === 'definitive').map((f) => f.name));
}

/** 扫描结果落 sessionStorage，避免刷新后丢掉「下载/停用」这一步。 */
export interface QuotaScanResult {
  finishedAt: string;
  scanned: number;
  failed: number;
  failures: QuotaScanFailure[];
}

const QUOTA_SCAN_RESULT_KEY = 'quotaPage.scanResult';

export const readQuotaScanResult = (): QuotaScanResult | null => {
  if (typeof window === 'undefined') return null;
  try {
    const raw = window.sessionStorage.getItem(QUOTA_SCAN_RESULT_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw) as QuotaScanResult;
    if (!parsed || !Array.isArray(parsed.failures)) return null;
    return parsed;
  } catch {
    return null;
  }
};

export const writeQuotaScanResult = (result: QuotaScanResult | null) => {
  if (typeof window === 'undefined') return;
  try {
    if (result === null) {
      window.sessionStorage.removeItem(QUOTA_SCAN_RESULT_KEY);
      return;
    }
    window.sessionStorage.setItem(QUOTA_SCAN_RESULT_KEY, JSON.stringify(result));
  } catch {
    // ignore
  }
};
