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
};

export const proxyPoolApi = {
  async list(): Promise<ProxyPoolEntry[]> {
    const data = await apiClient.get<Record<string, unknown>>('/proxy-pool');
    const items = data['proxy-pool'];
    return Array.isArray(items) ? (items as ProxyPoolEntry[]) : [];
  },

  replace: (entries: ProxyPoolEntry[]) => apiClient.put('/proxy-pool', entries),

  update: (id: string, value: Partial<Omit<ProxyPoolEntry, 'id'>>) =>
    apiClient.patch('/proxy-pool', { id, value }),

  delete: (id: string) => apiClient.delete(`/proxy-pool?id=${encodeURIComponent(id)}`),
};
