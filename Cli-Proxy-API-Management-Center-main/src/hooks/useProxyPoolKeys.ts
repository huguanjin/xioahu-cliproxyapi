import { useEffect, useState } from 'react';
import { proxyPoolApi } from '@/services/api';
import { getProxyHostPort } from '@/utils/proxyFilter';

/**
 * 代理池里每个代理的 `host:port`（不含协议和账号密码），供代理筛选下拉列出全部代理。
 * 拉取失败时返回空数组：下拉退回到只列凭证里出现的代理，筛选本身照常可用。
 */
export function useProxyPoolKeys(): string[] {
  const [keys, setKeys] = useState<string[]>([]);

  useEffect(() => {
    let cancelled = false;
    proxyPoolApi
      .list()
      .then((entries) => {
        if (cancelled) return;
        setKeys(entries.map((entry) => getProxyHostPort(entry['proxy-url'])).filter(Boolean));
      })
      .catch(() => {
        if (!cancelled) setKeys([]);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  return keys;
}
