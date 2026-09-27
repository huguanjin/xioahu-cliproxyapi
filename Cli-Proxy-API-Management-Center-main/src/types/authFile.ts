/**
 * 认证文件相关类型
 * 基于原项目 src/modules/auth-files.js
 */

import type { RecentRequestBucket } from '@/utils/recentRequests';

export type AuthFileType =
  | 'qwen'
  | 'kimi'
  | 'gemini'
  | 'aistudio'
  | 'claude'
  | 'codex'
  | 'antigravity'
  | 'xai'
  | 'iflow'
  | 'vertex'
  | 'empty'
  | 'unknown';

export interface AuthFileItem {
  name: string;
  type?: AuthFileType | string;
  provider?: string;
  /**
   * 凭证账号邮箱（后端 auth_files 两条分支都会填：磁盘扫描读 JSON 的 email 字段，
   * 注册表读 Metadata/Attributes）。卡片主行用它领衔。
   * 注意：后端还会下发 account/account_type，但 api-key 类凭证的 account 就是
   * API key 本身（AccountInfo() → return "api_key", apiKey），**绝不可用于展示或搜索**。
   */
  email?: string;
  /** GCP / Vertex 项目 ID，账号邮箱缺失时作为身份回落。 */
  projectId?: string;
  size?: number;
  authIndex?: string | number | null;
  runtimeOnly?: boolean | string;
  disabled?: boolean;
  unavailable?: boolean;
  status?: string;
  statusMessage?: string;
  /**
   * 最近一次上游失败的 HTTP 状态码（后端 LastError.HTTPStatus）。
   * 只有真正失败过的凭证才有；成功的凭证不带这个字段。
   * 与 statusMessage 不同：402 和 403 的 statusMessage 都是 "payment_required"，
   * 要区分问题账号只能看这个数字码。
   */
  lastStatusCode?: number;
  /** 最近一次上游失败的机器可读错误码（后端 LastError.Code，如 credential_invalid）。 */
  lastErrorCode?: string;
  lastRefresh?: string | number;
  modified?: number;
  priority?: number;
  weight?: number;
  note?: string;
  success?: unknown;
  failed?: unknown;
  /** 归一化后的累计成功/失败计数（由 API 边界从 success/failed 生字段填充）。 */
  successCount?: number;
  failureCount?: number;
  recent_requests?: RecentRequestBucket[];
  recentRequests?: RecentRequestBucket[];
  /** 是否已绑定代理，由后端根据 proxy_url 是否非空下发。 */
  has_proxy?: boolean;
  proxy_url?: string;
  /**
   * 最近一次自检（批量测试）对这张凭证的判定。
   * healthy=最近一次探测通过；cooling=确定性失败已冷却；
   * quarantine=连续失败已自动停用（首次干净探测会自动放回）；
   * validation=账号待验证，持有者可自行修复。
   * 从未被探测过的凭证不带该字段——不填「healthy」，因为没探测过不等于健康。
   */
  selfTestVerdict?: SelfTestVerdict;
  /** 连续确定性失败次数。 */
  selfTestStrikes?: number;
  /** 下次进入探测队列的时间。 */
  selfTestNextProbeAt?: string | number;
  /** 是否由自检循环自动停用（与运维手工停用区分）。 */
  selfTestAutoDisabled?: boolean;
  /** 触发自动停用的上游信息。 */
  selfTestAutoDisableReason?: string;
  [key: string]: unknown;
}

/** 自检判定（后端 SelfTestVerdict）。未探测过时后端不下发该字段。 */
export type SelfTestVerdict = 'healthy' | 'cooling' | 'quarantine' | 'validation';

/** 健康筛选：可正常调用的 / 有问题的 / 全部。 */
export type AuthFilesHealthFilterMode = 'all' | 'healthy' | 'problem';

export interface AuthFilesResponse {
  files: AuthFileItem[];
  total?: number;
}
