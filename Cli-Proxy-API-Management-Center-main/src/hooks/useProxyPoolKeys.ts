import { useEffect, useMemo, useState } from 'react';
import { proxyPoolApi, type ProxyPoolEntry } from '@/services/api';
import { getProxyHostPort } from '@/utils/proxyFilter';

export interface ProxyPoolKeys {
  /** 代理池里每个代理的 `host:port`（不含协议和账号密码）。 */
  keys: string[];
  /** 被标记为住宅 IP 的代理的 `host:port`，小写。 */
  residentialKeys: ReadonlySet<string>;
}

const EMPTY: ProxyPoolEntry[] = [];

/**
 * 代理池的 `host:port` 视图，供代理筛选下拉列出全部代理、供「住宅 IP」筛选判断。
 * 拉取失败时返回空集合：下拉退回到只列凭证里出现的代理，住宅筛选无命中。
 */
export function useProxyPoolKeys(): ProxyPoolKeys {
  const [entries, setEntries] = useState<ProxyPoolEntry[]>(EMPTY);

  useEffect(() => {
    let cancelled = false;
    proxyPoolApi
      .list()
      .then((list) => {
        if (!cancelled) setEntries(list);
      })
      .catch(() => {
        if (!cancelled) setEntries(EMPTY);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  return useMemo(() => {
    const keys: string[] = [];
    const residential = new Set<string>();
    entries.forEach((entry) => {
      const key = getProxyHostPort(entry['proxy-url']);
      if (!key) return;
      keys.push(key);
      if (entry.residential === true) residential.add(key.toLowerCase());
    });
    return { keys, residentialKeys: residential };
  }, [entries]);
}
