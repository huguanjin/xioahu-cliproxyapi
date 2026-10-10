/**
 * 代理筛选的纯逻辑。放在 utils 而不是 features/authFiles：额度页、凭证页和
 * 代理池页共用，不该让它们互相依赖。
 */

/** 去掉协议和账号密码，只留 `host:port`；解析不了时返回空串。 */
export function getProxyHostPort(raw: unknown): string {
  if (typeof raw !== 'string') return '';
  const trimmed = raw.trim();
  if (!trimmed) return '';
  try {
    const host = new URL(trimmed.includes('://') ? trimmed : `socks5://${trimmed}`).host;
    if (host) return host;
  } catch {
    // 落到下面的手工截取
  }
  const withoutScheme = trimmed.replace(/^[a-z0-9+.-]+:\/\//i, '');
  return withoutScheme.slice(withoutScheme.lastIndexOf('@') + 1).replace(/\/.*$/, '');
}

/**
 * 输入框里粘贴了完整代理地址（带协议或账号密码）时，立刻收成 `host:port`。
 *
 * 目的是让密码从不进入筛选状态：状态会被持久化、也会渲染进输入框。普通输入
 * （`91.132`、`:10003`）原样保留，包括大小写和中途的空格。
 */
export function sanitizeProxyFilterInput(raw: string): string {
  if (!raw.includes('@') && !raw.includes('://')) return raw;
  const withoutScheme = raw.trim().replace(/^[a-z0-9+.-]+:\/\//i, '');
  return withoutScheme.slice(withoutScheme.lastIndexOf('@') + 1).replace(/\/.*$/, '');
}

/**
 * 代理筛选的谓词。
 *
 * - 文字为空 = 不筛选。
 * - 文字恰好是某个已知代理的 `host:port` 时精确匹配：点选下拉建议后，
 *   `1.2.3.4:80` 不会连带 `1.2.3.4:8080`。
 * - 否则做子串模糊匹配，输入 `91.132` 或 `:10003` 都能圈出一批。
 *
 * 没有绑定代理的凭证（key 为空）只在不筛选时出现。
 */
export function buildProxyFilterMatcher(
  text: string,
  knownKeys: ReadonlySet<string>
): (proxyKey: string) => boolean {
  const needle = sanitizeProxyFilterInput(text).trim().toLowerCase();
  if (!needle) return () => true;
  if (knownKeys.has(needle)) return (proxyKey) => proxyKey.toLowerCase() === needle;
  return (proxyKey) => proxyKey !== '' && proxyKey.toLowerCase().includes(needle);
}

export interface ProxyFilterOption {
  /** 代理的 `host:port`。 */
  key: string;
  /** 该代理名下、当前范围内的凭证数；代理池里有但范围内没有凭证的为 0。 */
  count: number;
}

/**
 * 代理下拉的选项：代理池里的每个代理 + 凭证里自带但不在池里的代理。
 *
 * 以代理池为准而不是只看凭证：只看凭证时，范围内恰好没有凭证绑定的代理会从
 * 下拉里消失，操作者看到的就是「明明有十几个代理却只有一个选项」。数量为 0 也要
 * 列出来，那本身就是信息（池页说绑了、这里是 0，多半是被停用或不在本页范围）。
 * 同一 host:port 只出现一次（大小写不敏感）；按数量降序，同数量按名称升序。
 */
export function buildProxyFilterOptions(
  boundKeys: ReadonlyArray<string>,
  poolKeys: ReadonlyArray<string>
): ProxyFilterOption[] {
  const counts = new Map<string, { key: string; count: number }>();
  const touch = (key: string, delta: number) => {
    if (!key) return;
    const id = key.toLowerCase();
    const current = counts.get(id);
    if (current) current.count += delta;
    else counts.set(id, { key, count: delta });
  };
  poolKeys.forEach((key) => touch(key, 0));
  boundKeys.forEach((key) => touch(key, 1));
  return Array.from(counts.values()).sort(
    (a, b) => b.count - a.count || a.key.localeCompare(b.key)
  );
}

/**
 * 代理池页的搜索：按名称、`host:port`、出口 IP、账号、备注做不区分大小写的子串匹配。
 *
 * 刻意不拿完整代理地址去比：那里面有密码，输入 `abc` 就能「搜到」某个密码里带 abc
 * 的代理，既是泄露面也是误命中。想按账号搜，用「账号」字段。
 */
export function entryMatchesProxySearch(
  entry: {
    name?: string;
    'proxy-url': string;
    account?: string;
    'exit-ip'?: string;
    notes?: string;
  },
  query: string
): boolean {
  const needle = sanitizeProxyFilterInput(query).trim().toLowerCase();
  if (!needle) return true;
  return [
    entry.name,
    getProxyHostPort(entry['proxy-url']),
    entry['exit-ip'],
    entry.account,
    entry.notes,
  ].some((field) => (field ?? '').toLowerCase().includes(needle));
}

/**
 * 「住宅 IP」筛选的谓词：凭证绑定的代理（host:port）是否被标记为住宅。
 * 绑定了池外代理、或没绑定代理的凭证都不算 —— 池外代理没有标记可查，
 * 把它们算进去等于替操作者做了没人做过的判断。
 */
export function isResidentialProxyKey(
  proxyKey: string,
  residentialKeys: ReadonlySet<string>
): boolean {
  return proxyKey !== '' && residentialKeys.has(proxyKey.toLowerCase());
}
