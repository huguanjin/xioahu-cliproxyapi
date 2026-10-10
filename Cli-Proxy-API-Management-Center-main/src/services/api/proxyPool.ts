/**
 * 代理池管理（候选代理地址，供全局代理与认证文件代理下拉选择）
 */

import { apiClient } from './client';

export type ProxyPoolEntry = {
  id: string;
  name: string;
  'proxy-url': string;
  account?: string;
  'exit-ip'?: string;
  notes?: string;
  /** 是否标记为住宅 IP（人工标记，不是自动识别）。 */
  residential?: boolean;
  /** 当前绑定到该代理的凭证数量（只读，由服务端统计）。 */
  'bound-count'?: number;
};

export type ProxyProbeResult = {
  id: string;
  name?: string;
  ok: boolean;
  'latency-ms': number;
  'status-code'?: number;
  error?: string;
};

export type ProxyImportReport = {
  added: number;
  duplicate: number;
  invalid?: Array<{ line: number; error: string }>;
};

// 探测每个代理最长 10s，整池并发 8 路，远超默认的 30s 请求超时。
const PROXY_TEST_TIMEOUT_MS = 120 * 1000;

const readProbeResults = (data: Record<string, unknown>): ProxyProbeResult[] => {
  const items = data.results;
  return Array.isArray(items) ? (items as ProxyProbeResult[]) : [];
};

export const proxyPoolApi = {
  async list(): Promise<ProxyPoolEntry[]> {
    const data = await apiClient.get<Record<string, unknown>>('/proxy-pool');
    const items = data['proxy-pool'];
    return Array.isArray(items) ? (items as ProxyPoolEntry[]) : [];
  },

  replace: (entries: ProxyPoolEntry[]) =>
    apiClient.put(
      '/proxy-pool',
      entries.map((entry) => {
        // bound-count 是展示用的统计值，不属于配置，不回写。
        const { 'bound-count': _boundCount, ...rest } = entry;
        void _boundCount;
        return rest;
      })
    ),

  update: (id: string, value: Partial<Omit<ProxyPoolEntry, 'id' | 'bound-count'>>) =>
    apiClient.patch('/proxy-pool', { id, value }),

  delete: (id: string) => apiClient.delete(`/proxy-pool?id=${encodeURIComponent(id)}`),

  /** 经代理访问 Google 端点，检查代理是否可用。 */
  async test(target: { id: string } | { all: true }): Promise<ProxyProbeResult[]> {
    const data = await apiClient.post<Record<string, unknown>>('/proxy-pool/test', target, {
      timeout: PROXY_TEST_TIMEOUT_MS,
    });
    return readProbeResults(data);
  },

  /** 批量导入：text 每行一个代理，已存在的地址会被跳过。 */
  async import(
    text: string,
    options?: { namePrefix?: string; residential?: boolean }
  ): Promise<ProxyImportReport> {
    const data = await apiClient.post<{ report?: ProxyImportReport }>('/proxy-pool/import', {
      text,
      'name-prefix': options?.namePrefix?.trim() || undefined,
      residential: options?.residential === true ? true : undefined,
    });
    return data.report ?? { added: 0, duplicate: 0 };
  },
};
