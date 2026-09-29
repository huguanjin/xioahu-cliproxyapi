/**
 * 额度查询页：提供商 tabs + 统一卡网格。
 *
 * 保留的行为契约（重设计不改）：
 * - 点击加载：卡片挂载为 idle，额度只在用户点击/刷新时才打上游；
 * - cacheGeneration 会话隔离 + request-id 去重（见 useQuotaBatchLoader）；
 * - 文件列表变化后按 provider 剪枝额度缓存（已删文件不残留）；
 * - useHeaderRefresh 单槽位：本页唯一注册者，全局刷新 = 重取文件列表。
 */

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { authFilesApi } from '@/services/api';
import { downloadBlob } from '@/utils/download';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { Input } from '@/components/ui/Input';
import { Select } from '@/components/ui/Select';
import { Skeleton } from '@/components/ui/Skeleton';
import { IconSearch } from '@/components/ui/icons';
import { useHeaderRefresh } from '@/hooks/useHeaderRefresh';
import { useNow } from '@/hooks/useNow';
import { useRevealGroup } from '@/hooks/motion';
import { useAuthStore, useNotificationStore, useQuotaStore, useThemeStore } from '@/stores';
import type { AuthFileItem, ResolvedTheme } from '@/types';
import { ProviderTabs } from '@/features/authFiles/components/ProviderTabs';
import { buildWildcardSearch } from '@/features/authFiles/logic';
import { notifyAuthFilesChanged } from '@/features/authFiles/authFilesEvents';
import { AntigravityQuotaSummary } from './components/AntigravityQuotaSummary';
import { QuotaHeader } from './components/QuotaHeader';
import { QuotaCard } from './components/QuotaCard';
import { QuotaBatchActionBar } from './components/QuotaBatchActionBar';
import { QuotaScanDialog } from './components/QuotaScanDialog';
import { QuotaTimeline } from './components/QuotaTimeline';
import {
  CARD_ENTRANCE_BUDGET_MS,
  QUOTA_PAGE_SIZE,
  QUOTA_SORT_MODES,
  QUOTA_TAB_ORDER,
  type QuotaSortMode,
  type QuotaTabId,
} from './constants';
import {
  buildAvailabilityCounts,
  buildFamilyCounts,
  filterEntriesByAvailability,
  filterEntriesByFamily,
  isQuotaAvailabilityFilter,
  isQuotaFamilyFilter,
  QUOTA_AVAILABILITY_FILTERS,
  QUOTA_FAMILY_FILTERS,
  type QuotaAvailabilityFilter,
  type QuotaFamilyFilter,
} from './availability';
import { weeklyRemainingMin } from './providers/antigravity/familyState';
import {
  buildTabCounts,
  classifyQuotaFiles,
  filterEntriesBySearch,
  filterEntriesByTab,
  paginate,
  sortQuotaEntries,
  type QuotaFileEntry,
} from './logic';
import { nextRecoveryMs } from './resetSchedule';
import { QUOTA_ADAPTERS, getQuotaSetter, type QuotaCardState } from './providers';
import type { QuotaProviderType } from './providers/types';
import { useQuotaActions } from './hooks/useQuotaActions';
import { useQuotaBatchLoader } from './hooks/useQuotaBatchLoader';
import { readQuotaUiState, writeQuotaUiState } from './uiState';
import styles from './QuotaPage.module.scss';

const TAB_IDS: string[] = ['all', ...QUOTA_TAB_ORDER];
const SKELETON_CARD_COUNT = 6;

/**
 * 时间线泳道名 = 卡片标题，两者必须一致。卡片显示的就是文件名，所以这里是恒等。
 * 提到模块级是为了引用稳定 —— 它进了泳道 memo 的依赖数组。
 */
const displayNameFor = (name: string) => name;

export function QuotaPage() {
  const { t } = useTranslation();
  const connectionStatus = useAuthStore((state) => state.connectionStatus);
  const resolvedTheme: ResolvedTheme = useThemeStore((state) => state.resolvedTheme);
  const { showNotification, showConfirmation } = useNotificationStore();

  const [files, setFiles] = useState<AuthFileItem[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [tab, setTab] = useState<QuotaTabId>(() => readQuotaUiState()?.tab ?? 'all');
  const [sortMode, setSortMode] = useState<QuotaSortMode>(
    () => readQuotaUiState()?.sortMode ?? 'default'
  );
  const [availabilityFilter, setAvailabilityFilter] = useState<QuotaAvailabilityFilter>(
    () => readQuotaUiState()?.availabilityFilter ?? 'all'
  );
  const [familyFilter, setFamilyFilter] = useState<QuotaFamilyFilter>(
    () => readQuotaUiState()?.familyFilter ?? 'all'
  );
  const [page, setPage] = useState(1);
  const [search, setSearch] = useState('');
  const [selectedFiles, setSelectedFiles] = useState<Set<string>>(new Set());
  const [scanOpen, setScanOpen] = useState(false);
  // 页头 + tabs 的入场级联（标题 → meta → 动作 → tabs，级差 70ms）
  const revealRef = useRevealGroup<HTMLDivElement>();

  const disableControls = connectionStatus !== 'connected';

  /* ---------- 文件列表 ---------- */

  const loadFiles = useCallback(async () => {
    setLoading(true);
    setError('');
    try {
      const data = await authFilesApi.list();
      setFiles(data?.files || []);
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : t('notification.refresh_failed');
      setError(message);
    } finally {
      setLoading(false);
    }
  }, [t]);

  useHeaderRefresh(loadFiles);

  useEffect(() => {
    void loadFiles();
  }, [loadFiles]);

  /* ---------- 额度缓存 ----------
   * 排在归类/排序之前：「最快恢复优先」要读它算排序键。 */

  const antigravityQuota = useQuotaStore((state) => state.antigravityQuota);
  const claudeQuota = useQuotaStore((state) => state.claudeQuota);
  const codexQuota = useQuotaStore((state) => state.codexQuota);
  const kimiQuota = useQuotaStore((state) => state.kimiQuota);
  const xaiQuota = useQuotaStore((state) => state.xaiQuota);

  const quotaByType = useMemo<Record<QuotaProviderType, Record<string, QuotaCardState>>>(
    () =>
      ({
        antigravity: antigravityQuota,
        claude: claudeQuota,
        codex: codexQuota,
        kimi: kimiQuota,
        xai: xaiQuota,
      }) as unknown as Record<QuotaProviderType, Record<string, QuotaCardState>>,
    [antigravityQuota, claudeQuota, codexQuota, kimiQuota, xaiQuota]
  );

  const getQuota = useCallback(
    (entry: QuotaFileEntry): QuotaCardState | undefined => quotaByType[entry.type][entry.file.name],
    [quotaByType]
  );

  /* ---------- 归类 / 过滤 / 排序 / 分页 ---------- */

  // 分钟时钟只在「最快恢复优先」下需要：其余排序与时间无关，订阅它会让
  // pageItems 每分钟换一次身份，空转下面的刷新下降沿 effect。
  const tick = useNow(sortMode === 'soonest');
  const sortNow = sortMode === 'soonest' ? tick : 0;

  const entries = useMemo(() => classifyQuotaFiles(files), [files]);
  const tabCounts = useMemo(() => buildTabCounts(entries), [entries]);

  const normalizedSearch = search.trim();
  const wildcardSearch = useMemo(() => buildWildcardSearch(normalizedSearch), [normalizedSearch]);

  // tab 与搜索词依次收窄，下游（排序/分页/空态/批量条）只认这一个结果集。
  const scopedEntries = useMemo(
    () =>
      filterEntriesBySearch(filterEntriesByTab(entries, tab), normalizedSearch, wildcardSearch),
    [entries, tab, normalizedSearch, wildcardSearch]
  );

  // 可用性再收窄一层。计数建立在 scopedEntries（已过 tab/搜索）之上，
  // 所以每个数字描述的都是它正在筛选的那个列表；'all' 就是列表自身的大小。
  const availabilityCounts = useMemo(
    () => buildAvailabilityCounts(scopedEntries, getQuota),
    [scopedEntries, getQuota]
  );
  // 家族是与可用性正交的一层：两者可叠加（「有配额」且「Gemini」）。
  const familyCounts = useMemo(
    () => buildFamilyCounts(scopedEntries, getQuota),
    [scopedEntries, getQuota]
  );
  const availabilityFilteredEntries = useMemo(
    () => filterEntriesByAvailability(scopedEntries, availabilityFilter, getQuota),
    [scopedEntries, availabilityFilter, getQuota]
  );
  const filteredEntries = useMemo(
    () => filterEntriesByFamily(availabilityFilteredEntries, familyFilter, getQuota),
    [availabilityFilteredEntries, familyFilter, getQuota]
  );

  const resolveNextRecovery = useCallback(
    (entry: QuotaFileEntry) => nextRecoveryMs(entry.type, getQuota(entry), sortNow),
    [getQuota, sortNow]
  );
  // 周限额排序键只看「还剩多少」，与时间无关，所以不依赖 sortNow —— 那个
  // 分钟时钟不该让它每分钟换一次身份。
  const resolveWeeklyRemaining = useCallback(
    (entry: QuotaFileEntry) => {
      const quota = getQuota(entry);
      if (!quota || quota.status !== 'success') return null;
      // 目前只有 antigravity 以「家族 + 窗口」组织数据，也只有它有周限额。
      if (entry.type !== 'antigravity') return null;
      return weeklyRemainingMin((quota as { groups?: never[] }).groups ?? []);
    },
    [getQuota]
  );
  // 排序在分页之前：否则「最快恢复」只在当前页内成立。
  const sortedEntries = useMemo(
    () => sortQuotaEntries(filteredEntries, sortMode, resolveNextRecovery, resolveWeeklyRemaining),
    [filteredEntries, sortMode, resolveNextRecovery, resolveWeeklyRemaining]
  );

  const { pageItems, currentPage, totalPages } = useMemo(
    () => paginate(sortedEntries, page, QUOTA_PAGE_SIZE),
    [sortedEntries, page]
  );

  const handleTabChange = useCallback((next: string) => {
    setTab(next as QuotaTabId);
    setPage(1);
    writeQuotaUiState({ tab: next as QuotaTabId });
  }, []);

  const handleSortModeChange = useCallback((next: string) => {
    setSortMode(next as QuotaSortMode);
    setPage(1);
    writeQuotaUiState({ sortMode: next as QuotaSortMode });
  }, []);

  const handleAvailabilityFilterChange = useCallback((next: string) => {
    if (!isQuotaAvailabilityFilter(next)) return;
    setAvailabilityFilter(next);
    setPage(1);
    writeQuotaUiState({ availabilityFilter: next });
  }, []);

  const handleFamilyFilterChange = useCallback((next: string) => {
    if (!isQuotaFamilyFilter(next)) return;
    setFamilyFilter(next);
    setPage(1);
    writeQuotaUiState({ familyFilter: next });
  }, []);

  // 搜索词不入 sessionStorage：额度页是「点开即看」的巡检页，
  // 残留的关键字会让刷新后的空网格看起来像凭证丢了。
  const handleSearchChange = useCallback((next: string) => {
    setSearch(next);
    setPage(1);
  }, []);

  const clearSearch = useCallback(() => {
    setSearch('');
    setPage(1);
  }, []);

  const sortOptions = useMemo(
    () =>
      QUOTA_SORT_MODES.map((mode) => ({ value: mode, label: t(`quota_management.sort_${mode}`) })),
    [t]
  );

  // 标签带计数，让「有多少个取不到额度」在点开筛选前就能看见。
  const availabilityOptions = useMemo(
    () =>
      QUOTA_AVAILABILITY_FILTERS.map((filter) => ({
        value: filter,
        label: `${t(`quota_management.availability_filter_${filter}`)} (${availabilityCounts[filter]})`,
      })),
    [t, availabilityCounts]
  );

  // 家族筛选。计数只统计已加载额度的凭证 —— 家族是从载荷分组名推出来的，
  // 没加载就没有家族可言。
  const familyOptions = useMemo(
    () =>
      QUOTA_FAMILY_FILTERS.map((filter) => ({
        value: filter,
        label:
          filter === 'all'
            ? t('quota_management.family_filter_all')
            : `${t(`quota_management.family_filter_${filter}`)} (${familyCounts[filter]})`,
      })),
    [t, familyCounts]
  );

  const { loadedCount, attentionCount } = useMemo(() => {
    let loaded = 0;
    let attention = 0;
    entries.forEach((entry) => {
      const status = quotaByType[entry.type][entry.file.name]?.status;
      if (status === 'success') loaded += 1;
      else if (status === 'error') attention += 1;
    });
    return { loadedCount: loaded, attentionCount: attention };
  }, [entries, quotaByType]);

  // 剪枝：文件列表落定后，各 provider 缓存只保留仍存在的凭证
  useEffect(() => {
    if (loading) return;
    const survivorsByType = new Map<QuotaProviderType, Set<string>>(
      QUOTA_TAB_ORDER.map((type) => [type, new Set<string>()])
    );
    entries.forEach((entry) => survivorsByType.get(entry.type)?.add(entry.file.name));

    QUOTA_TAB_ORDER.forEach((type) => {
      const survivors = survivorsByType.get(type) ?? new Set<string>();
      const setQuota = getQuotaSetter(QUOTA_ADAPTERS[type]);
      setQuota((prev) => {
        const staleKeys = Object.keys(prev).filter((name) => !survivors.has(name));
        if (staleKeys.length === 0) return prev;
        const next = { ...prev };
        staleKeys.forEach((name) => delete next[name]);
        return next;
      });
    });
  }, [entries, loading]);

  /* ---------- 多选 / 批量删除 ---------- */

  // 剪枝：文件列表落定后，选中集合只保留仍存在的凭证名
  useEffect(() => {
    if (loading) return;
    const survivors = new Set(entries.map((entry) => entry.file.name));
    setSelectedFiles((prev) => {
      let changed = false;
      const next = new Set<string>();
      prev.forEach((name) => {
        if (survivors.has(name)) {
          next.add(name);
        } else {
          changed = true;
        }
      });
      return changed ? next : prev;
    });
  }, [entries, loading]);

  const toggleSelect = useCallback((name: string) => {
    setSelectedFiles((prev) => {
      const next = new Set(prev);
      if (next.has(name)) {
        next.delete(name);
      } else {
        next.add(name);
      }
      return next;
    });
  }, []);

  const selectPage = useCallback(() => {
    setSelectedFiles((prev) => {
      const next = new Set(prev);
      pageItems.forEach((entry) => next.add(entry.file.name));
      return next;
    });
  }, [pageItems]);

  const selectFiltered = useCallback(() => {
    setSelectedFiles((prev) => {
      const next = new Set(prev);
      filteredEntries.forEach((entry) => next.add(entry.file.name));
      return next;
    });
  }, [filteredEntries]);

  const invertPage = useCallback(() => {
    setSelectedFiles((prev) => {
      const next = new Set(prev);
      pageItems.forEach((entry) => {
        const name = entry.file.name;
        if (next.has(name)) {
          next.delete(name);
        } else {
          next.add(name);
        }
      });
      return next;
    });
  }, [pageItems]);

  const deselectAll = useCallback(() => {
    setSelectedFiles(new Set());
  }, []);

  // 一键全选「额度获取失败」的凭证：跨 tab/跨页，与页头「N 个需关注」同一口径，
  // 只加入 status === 'error' 的凭证，绝不动到正常凭证。
  const selectFailed = useCallback(() => {
    setSelectedFiles((prev) => {
      const next = new Set(prev);
      entries.forEach((entry) => {
        if (quotaByType[entry.type][entry.file.name]?.status === 'error') {
          next.add(entry.file.name);
        }
      });
      return next;
    });
  }, [entries, quotaByType]);

  // 批量下载走客户端的顺序循环：后端只有单凭证下载端点
  // （/auth-files/download?name=），没有 zip 端点，所以每个文件是一次独立请求。
  // 与认证文件页 batchDownload 同一实现，行为一致。
  const handleBatchDownload = useCallback(async () => {
    const names = Array.from(selectedFiles);
    if (names.length === 0) return;

    let successCount = 0;
    let failCount = 0;

    for (const name of names) {
      try {
        const blob = await authFilesApi.download(name);
        downloadBlob({ filename: name, blob });
        successCount++;
      } catch {
        failCount++;
      }
    }

    if (failCount === 0) {
      showNotification(t('auth_files.batch_download_success', { count: successCount }), 'success');
    } else {
      showNotification(
        t('auth_files.batch_download_partial', { success: successCount, failed: failCount }),
        'warning'
      );
    }

    deselectAll();
  }, [deselectAll, selectedFiles, showNotification, t]);

  const handleBatchDelete = useCallback(() => {
    const names = Array.from(selectedFiles);
    if (names.length === 0) return;

    showConfirmation({
      title: t('auth_files.batch_delete_title'),
      message: t('auth_files.batch_delete_confirm', { count: names.length }),
      variant: 'danger',
      confirmText: t('common.confirm'),
      onConfirm: async () => {
        try {
          const result = await authFilesApi.deleteFiles(names);
          const deletedSet = new Set(result.files);
          setFiles((prev) => prev.filter((file) => !deletedSet.has(file.name)));
          setSelectedFiles((prev) => {
            const next = new Set(prev);
            deletedSet.forEach((name) => next.delete(name));
            return next;
          });
          if (result.deleted > 0) notifyAuthFilesChanged();

          if (result.failed.length === 0) {
            showNotification(
              `${t('auth_files.delete_all_success')} (${result.deleted})`,
              'success'
            );
          } else {
            showNotification(
              t('auth_files.delete_filtered_partial', {
                success: result.deleted,
                failed: result.failed.length,
                type: t('auth_files.filter_all'),
              }),
              'warning'
            );
          }
        } catch (err: unknown) {
          const errorMessage = err instanceof Error ? err.message : '';
          showNotification(`${t('notification.delete_failed')}: ${errorMessage}`, 'error');
        }
      },
    });
  }, [selectedFiles, showConfirmation, showNotification, t]);

  /* ---------- 加载与操作 ---------- */

  const { batchLoading, loadQuota } = useQuotaBatchLoader();
  const { resettingQuotaName, refreshQuota, resetQuota } = useQuotaActions(disableControls);

  const pendingRefreshRef = useRef(false);
  const prevLoadingRef = useRef(loading);

  // 刷新全部：先重取文件列表，待其落定（loading 下降沿）再批量拉当前页额度
  const handleRefreshAll = useCallback(() => {
    if (disableControls) return;
    pendingRefreshRef.current = true;
    void loadFiles();
  }, [disableControls, loadFiles]);

  useEffect(() => {
    const wasLoading = prevLoadingRef.current;
    prevLoadingRef.current = loading;

    if (!pendingRefreshRef.current) return;
    if (loading || !wasLoading) return;

    pendingRefreshRef.current = false;
    void loadQuota(pageItems);
  }, [loading, loadQuota, pageItems]);

  const canUseActions = !disableControls && !loading;

  /* ---------- 首屏卡片一次性级联入场 ----------
   * 首批数据渲染后立即翻转 cardsAnimated；已挂载的卡片在挂载时捕获过自己的
   * 延迟（QuotaCard 内 useState 初始化），后续切 tab/翻页/刷新新挂载的卡片
   * 拿到 null —— 不重播。 */

  const [cardsAnimated, setCardsAnimated] = useState(false);
  const enableCardEntrance = !cardsAnimated && !loading && pageItems.length > 0;
  useEffect(() => {
    if (enableCardEntrance) {
      setCardsAnimated(true);
    }
  }, [enableCardEntrance]);
  const cardEntranceDelay = (index: number): number | null => {
    if (!enableCardEntrance) return null;
    if (pageItems.length <= 1) return 0;
    return Math.round((index / (pageItems.length - 1)) * CARD_ENTRANCE_BUDGET_MS);
  };

  /* ---------- 渲染 ---------- */

  const hasSearch = normalizedSearch.length > 0;
  // 空态分因：搜索无命中 ≠ 可用性筛选无命中 ≠ 本就没有可查额度的凭证，
  // 三者文案与出口都不同。
  const isEmpty = !loading && filteredEntries.length === 0;
  const isSearchEmpty = isEmpty && hasSearch;
  // 可用性空态只在「确实有候选被筛掉」时成立：tab 本身就空时该说的是
  // 「没有可查额度的凭证」，而不是「筛掉了」。
  const isAvailabilityEmpty =
    isEmpty && !hasSearch && availabilityFilter !== 'all' && scopedEntries.length > 0;

  return (
    <div className={styles.page} ref={revealRef}>
      <QuotaHeader
        totalCount={entries.length}
        loadedCount={loadedCount}
        attentionCount={attentionCount}
        refreshing={loading || batchLoading}
        disableControls={disableControls}
        onRefreshAll={handleRefreshAll}
        onSelectFailed={selectFailed}
        onOpenScan={() => setScanOpen(true)}
      />

      <AntigravityQuotaSummary />

      <section className={styles.workbench}>
        {/* tabs + 搜索 + 排序作为一个整体入场（useRevealGroup 会给每个 [data-reveal]
            后代加一级级差，所以控件放在同一个节点里而不是做兄弟） */}
        <div className={styles.tabsRow} data-reveal>
          <ProviderTabs
            types={TAB_IDS}
            counts={tabCounts}
            active={tab}
            resolvedTheme={resolvedTheme}
            onChange={handleTabChange}
          />
          <div className={styles.controls}>
            <div className={styles.search}>
              <Input
                value={search}
                onChange={(event) => handleSearchChange(event.target.value)}
                placeholder={t('quota_management.search_placeholder')}
                aria-label={t('quota_management.search_label')}
                rightElement={<IconSearch className={styles.searchIcon} size={15} />}
              />
            </div>
            <div className={styles.sort}>
              <Select
                value={sortMode}
                options={sortOptions}
                onChange={handleSortModeChange}
                ariaLabel={t('quota_management.sort_label')}
                size="sm"
              />
            </div>
            <div className={styles.sort}>
              <Select
                value={availabilityFilter}
                options={availabilityOptions}
                onChange={handleAvailabilityFilterChange}
                ariaLabel={t('quota_management.availability_filter_label')}
                size="sm"
              />
            </div>
            {/* 家族筛选：只看 Gemini 或只看 Claude+GPT。下拉而不是标签，
                因为顶部已经有三排筛选，再加标签会挤。 */}
            <div className={styles.sort}>
              <Select
                value={familyFilter}
                options={familyOptions}
                onChange={handleFamilyFilterChange}
                ariaLabel={t('quota_management.family_filter_label')}
                size="sm"
              />
            </div>
          </div>
        </div>

        {error && (
          <div className={styles.errorBanner} role="alert">
            {error}
          </div>
        )}

        {loading ? (
          <div className={styles.grid} aria-hidden="true">
            {Array.from({ length: SKELETON_CARD_COUNT }, (_, index) => (
              <Skeleton key={index} height={168} rounded={14} />
            ))}
          </div>
        ) : isSearchEmpty ? (
          <EmptyState
            title={t('quota_management.search_empty_title')}
            description={t('quota_management.search_empty_desc')}
            action={
              <Button variant="secondary" size="sm" onClick={clearSearch}>
                {t('quota_management.search_clear')}
              </Button>
            }
          />
        ) : isAvailabilityEmpty ? (
          <EmptyState
            title={t('quota_management.availability_filter_empty_title')}
            description={t('quota_management.availability_filter_empty_desc')}
            action={
              <Button
                variant="secondary"
                size="sm"
                onClick={() => handleAvailabilityFilterChange('all')}
              >
                {t('quota_management.availability_filter_all')}
              </Button>
            }
          />
        ) : isEmpty ? (
          <EmptyState
            title={
              tab === 'all'
                ? t('quota_management.empty_title')
                : t(`${QUOTA_ADAPTERS[tab].i18nPrefix}.empty_title`)
            }
            description={
              tab === 'all'
                ? t('quota_management.empty_desc')
                : t(`${QUOTA_ADAPTERS[tab].i18nPrefix}.empty_desc`)
            }
            action={
              tab === 'all' ? undefined : (
                <Button variant="secondary" size="sm" onClick={() => handleTabChange('all')}>
                  {t('auth_files.filter_all')}
                </Button>
              )
            }
          />
        ) : (
          <div className={styles.grid}>
            {pageItems.map((entry, index) => (
              <QuotaCard
                key={`${entry.type}:${entry.file.name}`}
                entry={entry}
                quota={getQuota(entry)}
                resolvedTheme={resolvedTheme}
                canRefresh={canUseActions && !entry.file.disabled}
                resetting={resettingQuotaName === entry.file.name}
                selected={selectedFiles.has(entry.file.name)}
                entranceDelayMs={cardEntranceDelay(index)}
                onRefresh={() => void refreshQuota(entry.file, QUOTA_ADAPTERS[entry.type])}
                onReset={() => resetQuota(entry.file, QUOTA_ADAPTERS[entry.type])}
                onToggleSelect={toggleSelect}
              />
            ))}
          </div>
        )}

        {!loading && filteredEntries.length > QUOTA_PAGE_SIZE && (
          <div className={styles.pagination}>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => setPage(Math.max(1, currentPage - 1))}
              disabled={currentPage <= 1}
            >
              {t('auth_files.pagination_prev')}
            </Button>
            <div className={styles.pageInfo}>
              {t('auth_files.pagination_info', {
                current: currentPage,
                total: totalPages,
                count: filteredEntries.length,
              })}
            </div>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => setPage(Math.min(totalPages, currentPage + 1))}
              disabled={currentPage >= totalPages}
            >
              {t('auth_files.pagination_next')}
            </Button>
          </div>
        )}

        {/* 时间线只比较当前页凭证，避免大量凭证一次性生成无界泳道。 */}
        <QuotaTimeline
          entries={pageItems}
          quotaFor={getQuota}
          displayNameFor={displayNameFor}
          resolvedTheme={resolvedTheme}
        />
      </section>

      <QuotaBatchActionBar
        selectionCount={selectedFiles.size}
        selectablePageCount={pageItems.length}
        selectableFilteredCount={filteredEntries.length}
        disableControls={disableControls}
        onSelectPage={selectPage}
        onSelectFiltered={selectFiltered}
        onInvertPage={invertPage}
        onDeselectAll={deselectAll}
        onDownload={() => void handleBatchDownload()}
        onDelete={handleBatchDelete}
      />

      {/* 巡检覆盖全部凭证（不是当前页），所以它的取数目标是 entries 而不是 pageItems。
          失败判定由 hook 自己直读 store —— 传 getQuota 进去会让它读到点击那一刻的
          陈旧快照，跑几分钟后必然报 0 个失败。 */}
      <QuotaScanDialog
        open={scanOpen}
        onClose={() => setScanOpen(false)}
        entries={entries}
        loadQuota={loadQuota}
      />
    </div>
  );
}
