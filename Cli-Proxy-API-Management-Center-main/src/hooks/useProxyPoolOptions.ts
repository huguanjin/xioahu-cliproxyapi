import { useEffect, useState } from 'react';
import { proxyPoolApi } from '@/services/api';

export function useProxyPoolOptions(): { value: string; label: string }[] {
  const [options, setOptions] = useState<{ value: string; label: string }[]>([]);

  useEffect(() => {
    let cancelled = false;
    proxyPoolApi
      .list()
      .then((entries) => {
        if (cancelled) return;
        setOptions(
          entries
            .filter((entry) => entry['proxy-url'])
            .map((entry) => ({
              value: entry['proxy-url'],
              label: entry.name ? `${entry.name} (${entry['proxy-url']})` : entry['proxy-url'],
            }))
        );
      })
      .catch(() => {
        if (!cancelled) setOptions([]);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  return options;
}
