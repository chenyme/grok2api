import { useQuery } from "@tanstack/react-query";
import { Activity, ArrowDown, ArrowUp, BrainCircuit, CircleCheck, CircleDollarSign, CornerDownRight, Database, Globe2, Info, Minimize2, RefreshCw, Search, WholeWord, type LucideIcon } from "lucide-react";
import { memo, useCallback, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { listModels } from "@/entities/model/model-api";
import { listClientKeys } from "@/features/client-keys/client-keys-api";
import { listAccounts } from "@/features/accounts/accounts-api";
import { RequestAuditDetailDialog } from "@/features/audits/request-audit-detail-dialog";
import { buildAuditUsageView } from "@/features/audits/audit-usage";
import { getRequestAudits, getRequestAuditSummary, type AuditBillingBreakdownDTO, type AuditBillingComponentDTO, type AuditDTO, type AuditPeriod } from "@/features/audits/request-audits-api";
import { EmptyState, ErrorState, TableLoadingRow } from "@/shared/components/data-state";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { DataTableFilters } from "@/shared/components/data-table-filters";
import { CursorPagination } from "@/shared/components/pagination";
import { PageHeader } from "@/shared/components/page-header";
import { PeriodSelector } from "@/shared/components/period-selector";
import { SortableTableHead } from "@/shared/components/sortable-table-head";
import { VirtualTableBody } from "@/shared/components/virtual-table-body";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { cn } from "@/shared/lib/cn";
import { formatCompactDateTime, formatDateTime, formatDuration, formatNumber } from "@/shared/lib/format";
import { toPeriodValue, type PeriodDays } from "@/shared/lib/period";
import { nextTableSort, type SortOrder, type TableSort } from "@/shared/lib/table-sort";
import { formatUSDTicks, usdTicksToValue } from "@/shared/lib/usd";

const AUDIT_PAGE_CACHE_TIME_MS = 60_000;
const AUDIT_SUMMARY_CACHE_TIME_MS = 120_000;
// 筛选名单始终限制在服务器搜索后的前 50 条，避免大账号池把大量选项累积到浏览器。
const AUDIT_FILTER_PAGE_SIZE = 50;
// 名单高度约 5 行，超出后内部滚动。
const AUDIT_FILTER_MAX_HEIGHT = "max-h-56 overflow-y-auto py-0.5";

type AuditCursorState = { scope: string; values: string[] };

export function RequestAuditsPage() {
  const { t, i18n } = useTranslation();
  const [pageSize, setPageSize] = useState(20);
  const [search, setSearch] = useState("");
  const [modelFilter, setModelFilter] = useState("");
  const [statusFilter, setStatusFilter] = useState("");
  const [modeFilter, setModeFilter] = useState("");
  const [keyFilter, setKeyFilter] = useState("");
  const [accountFilter, setAccountFilter] = useState("");
  const [periodDays, setPeriodDays] = useState<PeriodDays>(1);
  const [sort, setSort] = useState<TableSort>({ field: "createdAt", order: "desc" });
  const [manualRefreshing, setManualRefreshing] = useState(false);
  const [selectedAudit, setSelectedAudit] = useState<AuditDTO | null>(null);
  const forceSummaryRefresh = useRef(false);
  const debouncedSearch = useDebouncedValue(search);
  const debouncedKeyFilter = useDebouncedValue(keyFilter);
  const debouncedAccountFilter = useDebouncedValue(accountFilter);
  const period: AuditPeriod = toPeriodValue(periodDays);
  const cursorScope = useMemo(() => JSON.stringify([
    pageSize, debouncedSearch, modelFilter, statusFilter, modeFilter,
    debouncedKeyFilter, debouncedAccountFilter, period, sort.field, sort.order,
  ]), [pageSize, debouncedSearch, modelFilter, statusFilter, modeFilter, debouncedKeyFilter, debouncedAccountFilter, period, sort.field, sort.order]);
  const [cursorState, setCursorState] = useState<AuditCursorState>(() => ({ scope: cursorScope, values: [""] }));
  if (cursorState.scope !== cursorScope) {
    setCursorState({ scope: cursorScope, values: [""] });
  }
  const cursors = cursorState.scope === cursorScope ? cursorState.values : [""];
  const cursor = cursors[cursors.length - 1];

  const updateCursors = useCallback((update: (values: string[]) => string[]) => {
    setCursorState((current) => {
      const values = current.scope === cursorScope ? current.values : [""];
      return { scope: cursorScope, values: update(values) };
    });
  }, [cursorScope]);

  const auditsQuery = useQuery({
    queryKey: ["request-audits", "cursor", cursorScope, cursor],
    queryFn: ({ signal }) => getRequestAudits({ cursor, pageSize, search: debouncedSearch, model: modelFilter, status: statusFilter, mode: modeFilter, key: debouncedKeyFilter, account: debouncedAccountFilter, period, sortBy: sort.field, sortOrder: sort.order }, signal),
    placeholderData: (previous, previousQuery) => previousQuery?.queryKey[2] === cursorScope ? previous : undefined,
    gcTime: AUDIT_PAGE_CACHE_TIME_MS,
    structuralSharing: false,
  });
  const summaryQuery = useQuery({
    queryKey: ["request-audits", "summary", debouncedSearch, modelFilter, statusFilter, modeFilter, debouncedKeyFilter, debouncedAccountFilter, period],
    queryFn: ({ signal }) => getRequestAuditSummary({ search: debouncedSearch, model: modelFilter, status: statusFilter, mode: modeFilter, key: debouncedKeyFilter, account: debouncedAccountFilter, period }, forceSummaryRefresh.current, signal),
    placeholderData: (previous) => previous,
    gcTime: AUDIT_SUMMARY_CACHE_TIME_MS,
  });
  const modelOptionsQuery = useQuery({
    queryKey: ["models", "audit-filter"],
    queryFn: () => listModels({ page: 1, pageSize: 100 }),
    staleTime: 60_000,
  });
  // 密钥/账号筛选名单只在对应三级菜单展开时懒加载，输入后重新按匹配查询。
  const [keyFilterOptionsOpen, setKeyFilterOptionsOpen] = useState(false);
  const [accountFilterOptionsOpen, setAccountFilterOptionsOpen] = useState(false);
  const [keyFilterOptionsSearch, setKeyFilterOptionsSearch] = useState("");
  const [accountFilterOptionsSearch, setAccountFilterOptionsSearch] = useState("");
  const debouncedKeyFilterOptionsSearch = useDebouncedValue(keyFilterOptionsSearch);
  const debouncedAccountFilterOptionsSearch = useDebouncedValue(accountFilterOptionsSearch);
  const keyFilterOptionsQuery = useQuery({
    queryKey: ["client-keys", "audit-filter", debouncedKeyFilterOptionsSearch],
    queryFn: () => listClientKeys({ page: 1, pageSize: AUDIT_FILTER_PAGE_SIZE, search: auditFilterOptionSearch(debouncedKeyFilterOptionsSearch) }),
    enabled: keyFilterOptionsOpen,
    staleTime: 60_000,
  });
  const accountFilterOptionsQuery = useQuery({
    queryKey: ["accounts", "audit-filter", debouncedAccountFilterOptionsSearch],
    queryFn: () => listAccounts({ page: 1, pageSize: AUDIT_FILTER_PAGE_SIZE, search: auditFilterOptionSearch(debouncedAccountFilterOptionsSearch) }),
    enabled: accountFilterOptionsOpen,
    staleTime: 60_000,
  });
  const keyFilterOptionsFailed = keyFilterOptionsQuery.isError;
  const keyFilterOptionsFetching = keyFilterOptionsQuery.isFetching;
  const accountFilterOptionsFailed = accountFilterOptionsQuery.isError;
  const accountFilterOptionsFetching = accountFilterOptionsQuery.isFetching;
  // 账号范围覆盖三种 provider，审计记录可能来自任一 provider 的账号。
  const keyFilterOptions = keyFilterOptionsQuery.data?.items ?? [];
  const accountFilterOptions = accountFilterOptionsQuery.data?.items ?? [];
  const keyFilterGroups = [
    {
      id: "keys", label: t("audits.key"),
      emptyLabel: keyFilterOptionsFailed ? t("audits.filterOptionsLoadFailed") : keyFilterOptionsFetching ? t("common.loading") : t("audits.filterOptionsEmpty"),
      options: keyFilterOptions.map((key) => ({
        value: String(key.id),
        label: key.name || key.prefix,
        description: `#${key.id} · ${key.prefix}`,
      })),
      loading: keyFilterOptionsFetching, hasMore: keyFilterOptionsFailed,
      actionLabel: t("common.retry"), onAction: () => { void keyFilterOptionsQuery.refetch(); },
      noteLabel: !keyFilterOptionsFailed && (keyFilterOptionsQuery.data?.total ?? 0) > keyFilterOptions.length ? t("audits.filterOptionsTruncated") : undefined,
      hideLabel: true,
      maxHeightClassName: AUDIT_FILTER_MAX_HEIGHT,
    },
  ];
  const accountFilterGroups = [
    {
      id: "accounts", label: t("audits.account"),
      emptyLabel: accountFilterOptionsFailed ? t("audits.filterOptionsLoadFailed") : accountFilterOptionsFetching ? t("common.loading") : t("audits.filterOptionsEmpty"),
      options: accountFilterOptions.map((account) => ({
        value: String(account.id),
        label: account.name || account.email || `#${account.id}`,
        description: `#${account.id}`,
        badge: providerShortLabel(account.provider),
      })),
      loading: accountFilterOptionsFetching, hasMore: accountFilterOptionsFailed,
      actionLabel: t("common.retry"), onAction: () => { void accountFilterOptionsQuery.refetch(); },
      noteLabel: !accountFilterOptionsFailed && (accountFilterOptionsQuery.data?.total ?? 0) > accountFilterOptions.length ? t("audits.filterOptionsTruncated") : undefined,
      hideLabel: true,
      maxHeightClassName: AUDIT_FILTER_MAX_HEIGHT,
    },
  ];
  const result = auditsQuery.data;
  const nextCursor = result?.nextCursor ?? "";
  const summary = summaryQuery.data;
  const summaryLoading = summaryQuery.isPending || summaryQuery.isPlaceholderData;
  const cacheRate = summary?.usage.inputTokens ? summary.usage.cachedInputTokens / summary.usage.inputTokens * 100 : 0;
  const estimatedCostTicks = summary?.usage.estimatedCostInUsdTicks ?? 0;
  const hasEstimatedCost = (summary?.pricing.pricedRequests ?? 0) > 0;
  const modelOptions = useMemo(() => [...new Map((modelOptionsQuery.data?.items ?? []).map((model) => [model.publicId, { value: model.publicId, label: model.publicId }])).values()], [modelOptionsQuery.data?.items]);
  const openAudit = useCallback((audit: AuditDTO) => setSelectedAudit(audit), []);
  const renderAuditRow = useCallback((audit: AuditDTO) => <AuditRow key={audit.id} audit={audit} locale={i18n.language} onOpen={openAudit} />, [i18n.language, openAudit]);

  function refreshAll(): void {
    setManualRefreshing(true);
    forceSummaryRefresh.current = true;
    void Promise.all([
      auditsQuery.refetch(),
      summaryQuery.refetch(),
      new Promise<void>((resolve) => window.setTimeout(resolve, 400)),
    ]).finally(() => {
      forceSummaryRefresh.current = false;
      setManualRefreshing(false);
    });
  }

  const changeSort = useCallback((field: string, initialOrder: SortOrder): void => {
    setSort((current) => nextTableSort(current, field, initialOrder));
  }, []);

  return (
    <div className="space-y-5">
      <PageHeader
        title={t("audits.title")}
        description={t("audits.description")}
        actions={(
          <>
            <PeriodSelector value={periodDays} onChange={setPeriodDays} ariaLabel={t("audits.usageSummary")} />
            <Button variant="secondary" size="sm" onClick={refreshAll} disabled={auditsQuery.isFetching || summaryQuery.isFetching || manualRefreshing}><RefreshCw className={manualRefreshing || auditsQuery.isFetching || summaryQuery.isFetching ? "animate-spin" : undefined} />{t("common.refresh")}</Button>
          </>
        )}
      />

      <section className="space-y-2" aria-label={t("audits.usageSummary")}>
        <div className="grid gap-2 sm:grid-cols-2 xl:grid-cols-4">
          <AuditMetric icon={Activity} loading={summaryLoading} label={t("audits.totalRequests")} value={formatNumber(summary?.usage.requests ?? 0, i18n.language, 0)} detail={t("audits.requestBreakdown", { success: formatNumber(summary?.usage.successfulRequests ?? 0, i18n.language, 0), failed: formatNumber(summary?.usage.failedRequests ?? 0, i18n.language, 0) })} />
          <AuditMetric icon={WholeWord} loading={summaryLoading} label={t("audits.totalTokens")} value={formatNumber(summary?.usage.totalTokens ?? 0, i18n.language, 0)} detail={t("audits.tokenEfficiency", { cacheRate: formatNumber(cacheRate, i18n.language, 1) })} />
          <AuditMetric icon={CircleCheck} loading={summaryLoading} label={t("audits.successRate")} value={`${formatNumber(summary?.usage.successRate ?? 0, i18n.language, 1)}%`} detail={t("audits.averageDuration", { duration: formatDuration(summary?.usage.averageDurationMs ?? 0) })} />
          <AuditMetric
            icon={CircleDollarSign}
            loading={summaryLoading}
            label={t("audits.estimatedCost")}
            value={hasEstimatedCost ? formatUSDTicks(estimatedCostTicks, 2) : "-"}
            fullValue={hasEstimatedCost ? formatUSDTicks(estimatedCostTicks, 10) : undefined}
            detail={t("audits.pricingCoverage", { priced: formatNumber(summary?.pricing.pricedRequests ?? 0, i18n.language, 0), unpriced: formatNumber(summary?.pricing.unpricedRequests ?? 0, i18n.language, 0) })}
            tooltip={t("audits.pricingDescription")}
          />
        </div>
        <div className="grid grid-cols-2 gap-2 xl:grid-cols-4">
          <AuditTokenMetric icon={ArrowUp} loading={summaryLoading} label={t("audits.input")} value={formatNumber(summary?.usage.inputTokens ?? 0, i18n.language, 0)} />
          <AuditTokenMetric icon={ArrowDown} loading={summaryLoading} label={t("audits.output")} value={formatNumber(summary?.usage.outputTokens ?? 0, i18n.language, 0)} />
          <AuditTokenMetric icon={Database} loading={summaryLoading} label={t("audits.cached")} value={formatNumber(summary?.usage.cachedInputTokens ?? 0, i18n.language, 0)} />
          <AuditTokenMetric icon={BrainCircuit} loading={summaryLoading} label={t("audits.reasoning")} value={formatNumber(summary?.usage.reasoningTokens ?? 0, i18n.language, 0)} />
        </div>
      </section>

      <DataTableShell
        toolbar={(
          <>
            <div className="flex w-full items-center gap-2 sm:w-auto">
              <div className="relative min-w-0 flex-1 sm:w-64 sm:flex-none">
                <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                <Input className="h-8 pl-9 text-xs" value={search} onChange={(event) => setSearch(event.target.value)} placeholder={t("audits.search")} aria-label={t("audits.search")} />
              </div>
              <DataTableFilters filters={[
                { id: "model", label: t("audits.model"), value: modelFilter, onChange: setModelFilter, options: modelOptions },
                { id: "status", label: t("audits.status"), value: statusFilter, onChange: setStatusFilter, options: [
                  { value: "2xx", label: `2xx · ${t("audits.statusSuccess")}` },
                  { value: "4xx", label: `4xx · ${t("audits.statusClientError")}` },
                  { value: "5xx", label: `5xx · ${t("audits.statusServerError")}` },
                  { value: "other", label: t("audits.statusOtherError") },
                ] },
                { id: "mode", label: t("audits.mode"), value: modeFilter, onChange: setModeFilter, options: [
                  { value: "stream", label: t("audits.stream") },
                  { value: "nonStream", label: t("audits.nonStream") },
                ] },
                {
                  id: "key", label: t("audits.key"), value: keyFilter,
                  onChange: setKeyFilter, options: [
                    {
                      value: "any", label: t("audits.key"), groups: keyFilterGroups,
                      onGroupsOpenChange: setKeyFilterOptionsOpen,
                      groupSearch: { value: keyFilterOptionsSearch, placeholder: t("audits.keyFilterPlaceholder"), onChange: (value) => {
                        setKeyFilterOptionsSearch(value);
                      } },
                    },
                  ],
                },
                {
                  id: "account", label: t("audits.account"), value: accountFilter,
                  onChange: setAccountFilter, options: [
                    {
                      value: "any", label: t("audits.account"), groups: accountFilterGroups,
                      onGroupsOpenChange: setAccountFilterOptionsOpen,
                      groupSearch: { value: accountFilterOptionsSearch, placeholder: t("audits.accountFilterPlaceholder"), onChange: (value) => {
                        setAccountFilterOptionsSearch(value);
                      } },
                    },
                  ],
                },
              ]} />
            </div>
          </>
        )}
        footer={(result?.items.length ?? 0) > 0 || cursors.length > 1 ? (
          <CursorPagination
            page={cursors.length}
            pageSize={pageSize}
            hasMore={Boolean(result?.hasMore && nextCursor)}
            disabled={auditsQuery.isFetching}
            onFirstPage={() => updateCursors(() => [""])}
            onPreviousPage={() => updateCursors((values) => values.length > 1 ? values.slice(0, -1) : values)}
            onNextPage={() => { if (nextCursor) updateCursors((values) => [...values, nextCursor]); }}
            onPageSizeChange={setPageSize}
          />
        ) : undefined}
      >
        {auditsQuery.isError ? <ErrorState message={auditsQuery.error.message} onRetry={() => void auditsQuery.refetch()} /> : null}
        {result && result.items.length === 0 ? <EmptyState /> : null}
        {auditsQuery.isPending || (result && result.items.length > 0) ? (
          <Table viewportRows={20} rowHeight={96} aria-busy={auditsQuery.isFetching} className={cn("min-w-[1008px] table-fixed text-xs transition-opacity", auditsQuery.isPlaceholderData && "pointer-events-none opacity-60")}>
            <colgroup>
              <col className="w-36" />
              <col className="w-24" />
              <col className="w-24" />
              <col className="w-64" />
              <col className="w-24" />
              <col className="w-40" />
              <col className="w-40" />
            </colgroup>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <SortableTableHead field="model" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("audits.model")}</SortableTableHead>
                <TableHead className="text-center">{t("audits.egress")}</TableHead>
                <SortableTableHead field="billing" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("audits.billing")}</SortableTableHead>
                <SortableTableHead field="tokens" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" className="px-3" onSort={changeSort}>{t("audits.tokens")}</SortableTableHead>
                <SortableTableHead field="status" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort}>{t("audits.status")}</SortableTableHead>
                <SortableTableHead field="duration" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("audits.responsePerformance")}</SortableTableHead>
                <SortableTableHead field="createdAt" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("audits.createdAt")}</SortableTableHead>
              </TableRow>
            </TableHeader>
            {auditsQuery.isPending ? (
              <TableBody><TableLoadingRow colSpan={7} /></TableBody>
            ) : (
              <VirtualTableBody items={result?.items ?? []} colSpan={7} rowHeight={96} overscan={6} renderRow={renderAuditRow} />
            )}
          </Table>
        ) : null}
      </DataTableShell>
      <RequestAuditDetailDialog key={selectedAudit?.id ?? "closed"} audit={selectedAudit} open={selectedAudit !== null} onOpenChange={(open) => !open && setSelectedAudit(null)} />
    </div>
  );
}

const AuditRow = memo(function AuditRow({ audit, locale, onOpen }: { audit: AuditDTO; locale: string; onOpen: (audit: AuditDTO) => void }) {
  const createdAt = formatCompactDateTime(audit.createdAt, locale);
  const createdAtLabel = formatDateTime(audit.createdAt, locale);
  return (
    <TableRow className="h-[96px]">
      <TableCell>
        <ModelRouteValue
          model={audit.modelPublicId || `#${audit.modelRouteId}`}
          upstreamModel={audit.modelUpstreamModel || "-"}
          account={audit.accountName || (audit.accountId ? `#${audit.accountId}` : "-")}
          clientKey={audit.clientKeyName || `#${audit.clientKeyId}`}
          clientIp={audit.clientIp}
          requestId={audit.requestId}
          provider={audit.provider}
          operation={audit.operation}
          sources={audit.numSourcesUsed}
        />
      </TableCell>
      <TableCell className="text-center"><EgressValue audit={audit} /></TableCell>
      <TableCell><BillingValue audit={audit} /></TableCell>
      <TableCell className="px-3"><UsageDetails audit={audit} locale={locale} /></TableCell>
      <TableCell className="text-center"><AuditStatus audit={audit} onOpen={() => onOpen(audit)} /></TableCell>
      <TableCell><ResponsePerformance audit={audit} locale={locale} /></TableCell>
      <TableCell className="whitespace-nowrap text-xs text-muted-foreground tabular-nums">
        <time dateTime={audit.createdAt} title={createdAtLabel}>{createdAt}</time>
      </TableCell>
    </TableRow>
  );
});

function ResponsePerformance({ audit, locale }: { audit: AuditDTO; locale: string }) {
  const { t } = useTranslation();
  const duration = splitDuration(formatDuration(audit.durationMs));
  const firstToken = audit.firstTokenMs === undefined ? { value: "—", unit: "" } : splitDuration(formatDuration(audit.firstTokenMs));
  const throughput = audit.outputTokensPerSecond === undefined ? "—" : formatNumber(audit.outputTokensPerSecond, locale, 1);
  return (
    <div className="grid w-fit max-w-full grid-cols-[auto_auto] gap-x-2.5 gap-y-0.5 whitespace-nowrap text-[11px] leading-4 tabular-nums">
      <span className="text-muted-foreground">{t("audits.durationMetric")}</span>
      <PerformanceValue value={duration.value} unit={duration.unit} />
      <span className="text-muted-foreground">{t("audits.firstTokenMetric")}</span>
      <PerformanceValue value={firstToken.value} unit={firstToken.unit} />
      <span className="text-muted-foreground">{t("audits.throughputMetric")}</span>
      <PerformanceValue value={throughput} unit={t("audits.tokensPerSecondUnit")} />
    </div>
  );
}

function PerformanceValue({ value, unit }: { value: string; unit: string }) {
  return <span className="font-medium">{value}{unit ? <> <span className="font-normal">{unit}</span></> : null}</span>;
}

function splitDuration(value: string): { value: string; unit: string } {
  const separator = value.lastIndexOf(" ");
  if (separator < 0) {
    return { value, unit: "" };
  }
  return { value: value.slice(0, separator), unit: value.slice(separator + 1) };
}

function EgressValue({ audit }: { audit: AuditDTO }) {
  const { t } = useTranslation();
  if (!audit.egressMode) {
    return <span className="text-muted-foreground">-</span>;
  }
  const proxied = audit.egressMode === "proxy";
  const node = audit.egressNodeName || (proxied ? t("audits.egressUnknown") : t("audits.egressDirect"));
  const details = [audit.egressScope, audit.egressNodeId ? `#${audit.egressNodeId}` : ""].filter(Boolean).join(" · ");
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button type="button" className="inline-block min-w-0 max-w-full cursor-help text-center" aria-label={`${proxied ? t("audits.egressProxy") : t("audits.egressDirect")}: ${node}`}>
          <span className={cn("inline-flex items-center gap-1.5 text-xs", proxied ? "text-emerald-700 dark:text-emerald-300" : "text-muted-foreground")}>
            <span className={cn("size-1.5 rounded-full", proxied ? "bg-emerald-500" : "bg-muted-foreground/50")} />
            {proxied ? t("audits.egressProxy") : t("audits.egressDirect")}
          </span>
        </button>
      </TooltipTrigger>
      <TooltipContent className="max-w-72" side="top" align="center">
        <div>{node}</div>
        {details ? <div className="mt-1 text-primary-foreground/65">{details}</div> : null}
      </TooltipContent>
    </Tooltip>
  );
}

function BillingValue({ audit }: { audit: AuditDTO }) {
  const { t, i18n } = useTranslation();
  const billing = audit.billing ?? fallbackBillingBreakdown(audit);
  const amount = billing ? formatUSDTicks(billing.totalInUsdTicks, 2) : t("audits.unbilled");
  return (
    <div className="max-w-full text-left">
      {billing ? (
        <Tooltip>
          <TooltipTrigger asChild><span className="block cursor-help whitespace-nowrap text-xs tabular-nums" tabIndex={0}>{amount}</span></TooltipTrigger>
          <TooltipContent className="w-96 max-w-[calc(100vw-2rem)] p-3" side="top" align="start">
            <BillingBreakdown billing={billing} locale={i18n.language} />
          </TooltipContent>
        </Tooltip>
      ) : <span className="block whitespace-nowrap text-xs text-muted-foreground">{amount}</span>}
      {audit.numServerSideToolsUsed > 0 ? (
        <span className="mt-0.5 block whitespace-nowrap text-[10px] text-muted-foreground">
          {t("audits.serverTools", { count: audit.numServerSideToolsUsed })}
        </span>
      ) : null}
    </div>
  );
}

function fallbackBillingBreakdown(audit: AuditDTO): AuditBillingBreakdownDTO | undefined {
  if (audit.costInUsdTicks > 0) {
    return { source: "upstream", method: "upstream_reported", components: [], totalInUsdTicks: audit.costInUsdTicks };
  }
  if (!audit.pricingModel) {
    return undefined;
  }
  return {
    source: "official",
    method: "stored_estimate",
    model: audit.pricingModel,
    version: audit.pricingVersion,
    components: [],
    totalInUsdTicks: audit.estimatedCostInUsdTicks,
  };
}

function BillingBreakdown({ billing, locale }: { billing: AuditBillingBreakdownDTO; locale: string }) {
  const { t } = useTranslation();
  return (
    <div className="space-y-2.5 text-xs leading-5">
      <div className="space-y-1">
        <BillingDetailRow label={t("audits.billingSource")} value={billing.source === "upstream" ? t("audits.billingSourceUpstream") : t("audits.billingSourceOfficial")} />
        {billing.model ? <BillingDetailRow label={t("audits.billingModel")} value={billing.model} mono /> : null}
        {billing.version ? <BillingDetailRow label={t("audits.billingVersion")} value={billing.version} /> : null}
        {billing.tier === "long_context" ? <BillingDetailRow label={t("audits.billingRateTier")} value={t("audits.billingLongContextTier")} /> : null}
      </div>
      <div className="border-t border-primary-foreground/15 pt-2">
        <div className="mb-1 text-primary-foreground/65">{t("audits.billingFormula")}</div>
        {billing.method === "upstream_reported" ? (
          <p>{t("audits.billingUpstreamFormula")}</p>
        ) : billing.method === "stored_estimate" ? (
          <p>{t("audits.billingStoredFormulaUnavailable")}</p>
        ) : billing.components.length === 0 ? (
          <p>{t("audits.billingZeroFormula")}</p>
        ) : (
          <div className="space-y-1">
            {billing.components.map((component) => <BillingFormula key={component.kind} component={component} locale={locale} />)}
          </div>
        )}
      </div>
      <div className="flex items-baseline justify-between gap-4 border-t border-primary-foreground/15 pt-2 font-medium">
        <span>{t("audits.billingConclusion")}</span>
        <span className="font-mono tabular-nums">{formatUSDTicks(billing.totalInUsdTicks, 10)}</span>
      </div>
    </div>
  );
}

function BillingDetailRow({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-3">
      <span className="text-primary-foreground/65">{label}</span>
      <span className={cn("break-all text-right", mono && "font-mono")}>{value}</span>
    </div>
  );
}

function BillingFormula({ component, locale }: { component: AuditBillingComponentDTO; locale: string }) {
  const { t } = useTranslation();
  const quantity = formatNumber(component.quantity, locale, 0);
  const formula = component.unit === "token"
    ? `${quantity} / 1M × ${formatUSDCostCompact(component.unitPriceInUsdTicks * 1_000_000)}`
    : `${quantity} × ${formatUSDCostCompact(component.unitPriceInUsdTicks)} / ${t(`audits.billingUnits.${component.unit}`)}`;
  return (
    <div className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-3">
      <span className="text-primary-foreground/65">{t(`audits.billingComponents.${component.kind}`)}</span>
      <span className="break-words text-right font-mono tabular-nums">{formula} = {formatUSDTicks(component.subtotalInUsdTicks, 10)}</span>
    </div>
  );
}

function AuditMetric({ icon: Icon, label, value, detail, tooltip, fullValue, loading }: { icon: LucideIcon; label: string; value: string; detail?: string; tooltip?: string; fullValue?: string; loading: boolean }) {
  const { t } = useTranslation();
  return (
    <article className="min-h-28 rounded-lg bg-card p-4" aria-busy={loading}>
      <header className="flex min-h-5 items-center justify-between gap-3">
        <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
          <span>{label}</span>
          {tooltip ? (
            <Tooltip>
              <TooltipTrigger asChild><button type="button" className="cursor-help" aria-label={tooltip}><Info className="size-3.5" /></button></TooltipTrigger>
              <TooltipContent className="max-w-72 leading-5">{tooltip}</TooltipContent>
            </Tooltip>
          ) : null}
        </div>
        <Icon className="size-4 shrink-0 text-muted-foreground" />
      </header>
      <div className="mt-3 flex min-h-8 items-center text-2xl font-medium tracking-tight tabular-nums">
        {loading ? <Spinner /> : fullValue ? (
          <Tooltip>
            <TooltipTrigger asChild><span className="cursor-help" tabIndex={0}>{value}</span></TooltipTrigger>
            <TooltipContent side="top"><span className="text-primary-foreground/65">{t("audits.exactBilling")}</span> <span className="font-mono">{fullValue}</span></TooltipContent>
          </Tooltip>
        ) : value}
      </div>
      {detail ? <p className={cn("mt-1.5 min-h-4 truncate text-[11px] text-muted-foreground", loading && "invisible")} title={detail}>{detail}</p> : null}
    </article>
  );
}

function AuditTokenMetric({ icon: Icon, label, value, loading }: { icon: LucideIcon; label: string; value: string; loading: boolean }) {
  return (
    <div className="flex min-h-11 min-w-0 items-center justify-between gap-3 rounded-lg bg-muted/45 px-4 py-2">
      <span className="flex min-w-0 items-center gap-2 text-xs text-muted-foreground"><Icon className="size-3.5 shrink-0" />{label}</span>
      <span className="flex min-h-5 min-w-8 items-center justify-end truncate text-sm font-medium tabular-nums" title={loading ? undefined : value}>{loading ? <Spinner className="size-3.5" /> : value}</span>
    </div>
  );
}

function ModelRouteValue({ model, upstreamModel, account, clientKey, clientIp, requestId, provider, operation, sources }: {
  model: string;
  upstreamModel: string;
  account: string;
  clientKey: string;
  clientIp?: string;
  requestId: string;
  provider: AuditDTO["provider"];
  operation: AuditDTO["operation"];
  sources: number;
}) {
  const { t } = useTranslation();
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button type="button" className="block w-full min-w-0 cursor-help text-left" aria-label={t("audits.routeDetails")}>
          <span className="block truncate text-xs font-medium" title={model}>{model}</span>
          <span className="mt-0.5 flex min-w-0 items-center gap-1 text-[11px] text-muted-foreground">
            <CornerDownRight className="size-3 shrink-0" />
            <span className="truncate" title={upstreamModel}>{upstreamModel}</span>
          </span>
          {clientIp ? (
            <span className="mt-0.5 flex min-w-0 items-center gap-1 text-[10px] text-muted-foreground/80">
              <Globe2 className="size-3 shrink-0" />
              <span className="truncate font-mono" title={clientIp}>{clientIp}</span>
            </span>
          ) : null}
        </button>
      </TooltipTrigger>
      <TooltipContent className="w-72 max-w-[calc(100vw-2rem)] space-y-1.5 py-2" side="top" align="start">
        <RouteDetailRow label={t("audits.channelProtocol")} value={`${providerLabel(provider)} · ${auditProtocolLabel(operation)}`} />
        <RouteDetailRow label={t("audits.requestId")} value={requestId} breakAll />
        {clientIp ? <RouteDetailRow label={t("audits.clientIp")} value={clientIp} breakAll /> : null}
        <RouteDetailRow label={t("audits.actualModel")} value={upstreamModel} breakAll />
        <RouteDetailRow label={t("audits.owningAccount")} value={account} />
        <RouteDetailRow label={t("audits.owningKey")} value={clientKey} />
        {sources > 0 ? (
          <RouteDetailRow label={t("audits.sourcesLabel")} value={String(sources)} />
        ) : null}
      </TooltipContent>
    </Tooltip>
  );
}

function RouteDetailRow({ label, value, breakAll = false }: { label: string; value: string; breakAll?: boolean }) {
  return (
    <div className="grid grid-cols-[auto_minmax(0,1fr)] items-start gap-x-3 text-xs font-normal leading-4">
      <span className="text-primary-foreground/65">{label}</span>
      <span className={cn("text-right", breakAll ? "break-all" : "truncate")} title={value}>{value}</span>
    </div>
  );
}

function UsageDetails({ audit, locale }: { audit: AuditDTO; locale: string }) {
  const { t } = useTranslation();
  const view = buildAuditUsageView(audit, (value) => formatNumber(value, locale), {
    input: t("audits.input"),
    output: t("audits.output"),
    cached: t("audits.cached"),
    reasoning: t("audits.reasoning"),
    mediaInput: t("audits.mediaInput"),
    mediaOutput: t("audits.mediaOutput"),
    imageCount: (count) => t("audits.imageCount", { count }),
    secondsCount: (count) => t("audits.secondsCount", { count }),
  });
  if (view.mode === "compaction") {
    return (
      <div className="flex h-[52px] w-full items-center gap-2 rounded-md bg-muted/45 px-2.5 text-[11px]">
        <Minimize2 className="size-3.5 shrink-0 text-muted-foreground" />
        <div className="min-w-0">
          <p className="truncate font-medium">{t("audits.operations.compaction")}</p>
          <p className="truncate text-muted-foreground">{t("audits.compactionUsageUnavailable")}</p>
        </div>
      </div>
    );
  }
  if (view.mode === "duration") {
    return (
      <div className="flex h-[52px] w-full items-center gap-2 rounded-md bg-muted/45 px-2.5 text-[11px]">
        <div className="min-w-0">
          <p className="truncate font-medium">{t(`audits.operations.${audit.operation}`)}</p>
          <p className="truncate text-muted-foreground">{view.durationSeconds}s</p>
        </div>
      </div>
    );
  }
  return (
    <div className="w-full space-y-1">
      {view.mediaItems?.length ? (
        <div className="grid grid-cols-2 gap-1">
          {view.mediaItems.map((item) => (
            <UsageMetric key={item.key} label={item.label} value={item.value} />
          ))}
        </div>
      ) : null}
      {view.tokenItems?.length ? (
        <div className="grid grid-cols-2 gap-1">
          {view.tokenItems.map((item) => (
            <UsageMetric
              key={item.key}
              label={item.label}
              value={item.value}
              reasoningEffort={item.key === "reasoning" ? audit.reasoningEffort : undefined}
            />
          ))}
        </div>
      ) : null}
    </div>
  );
}

function UsageMetric({ label, value, reasoningEffort }: {
  label: string;
  value: string;
  reasoningEffort?: AuditDTO["reasoningEffort"];
}) {
  const { t } = useTranslation();
  const fullLabel = reasoningEffort ? `${label} · ${t(`audits.reasoningEfforts.${reasoningEffort}`)}` : label;
  return (
    <div className="flex h-6 min-w-0 items-center justify-between gap-2 rounded-md bg-muted/45 px-2 text-[11px]">
      <span className="flex min-w-0 items-center gap-1" title={fullLabel}>
        <span className="truncate text-muted-foreground">{label}</span>
        {reasoningEffort ? (
          <span className={cn("shrink-0 font-medium", reasoningEffortTone(reasoningEffort))}>
            · {t(`audits.reasoningEfforts.${reasoningEffort}`)}
          </span>
        ) : null}
      </span>
      <span className="truncate font-medium tabular-nums" title={value}>{value}</span>
    </div>
  );
}

function reasoningEffortTone(effort: NonNullable<AuditDTO["reasoningEffort"]>): string {
  switch (effort) {
    case "none": return "text-muted-foreground";
    case "low": return "text-sky-600 dark:text-sky-400";
    case "medium": return "text-amber-600 dark:text-amber-400";
    case "high": return "text-orange-600 dark:text-orange-400";
    case "xhigh": return "text-rose-600 dark:text-rose-400";
    case "auto": return "text-violet-600 dark:text-violet-400";
    case "fixed": return "text-indigo-600 dark:text-indigo-400";
  }
}

function StatusCode({ statusCode, hasError = false }: { statusCode: number; hasError?: boolean }) {
  const tone = statusTone(statusCode, hasError);
  return (
    <span className={cn("inline-flex items-center gap-1 text-[10px] leading-4 tabular-nums", tone.text)}>
      <span className={cn("size-1.5 rounded-full", tone.dot)} />
      {statusCode || "-"}
    </span>
  );
}

function AuditStatus({ audit, onOpen }: { audit: AuditDTO; onOpen: () => void }) {
  const { t } = useTranslation();
  const mode = audit.operation === "compaction" ? t("audits.operations.compaction") : audit.streaming ? t("audits.stream") : t("audits.nonStream");
  const hasError = Boolean(audit.errorCode);
  // 保留真实 HTTP 状态，同时明确标识 2xx 响应头之后发生的流式失败。
  // statusCode 0 仅兼容曾运行过早期实现的开发数据库。
  const showErrorLabel = hasError && (audit.statusCode === 0 || (audit.statusCode >= 200 && audit.statusCode < 300));
  const content = (
    <>
      {showErrorLabel ? (
        <span className="inline-flex items-center gap-1 text-[10px] leading-4 tabular-nums text-amber-700 dark:text-amber-300">
          <span className="size-1.5 rounded-full bg-amber-500" />
          {audit.statusCode > 0 ? `${audit.statusCode} · ` : ""}{t("audits.errorLabel")}
        </span>
      ) : (
        <StatusCode statusCode={audit.statusCode} hasError={hasError} />
      )}
      <span className="block whitespace-nowrap text-[10px] text-muted-foreground">{mode}</span>
    </>
  );
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button
          type="button"
          className="group inline-flex flex-col items-center justify-center space-y-0.5 rounded-md px-2 py-1 text-center outline-none transition-colors hover:bg-muted/80 focus-visible:ring-2 focus-visible:ring-ring/50 [&>span:last-child]:underline-offset-2 hover:[&>span:last-child]:text-foreground hover:[&>span:last-child]:underline cursor-pointer"
          aria-label={t("audits.viewDetails")}
          onClick={onOpen}
        >
          {content}
        </button>
      </TooltipTrigger>
      <TooltipContent className="max-w-80 whitespace-normal break-words text-left leading-5" side="top">
        {audit.errorCode || t("audits.viewDetails")}
      </TooltipContent>
    </Tooltip>
  );
}

function statusTone(statusCode: number, hasError = false): { dot: string; text: string } {
  if (hasError) return { dot: "bg-amber-500", text: "text-amber-700 dark:text-amber-300" };
  if (statusCode >= 500) return { dot: "bg-red-500", text: "text-red-700 dark:text-red-300" };
  if (statusCode >= 400) return { dot: "bg-amber-500", text: "text-amber-700 dark:text-amber-300" };
  if (statusCode >= 200 && statusCode < 300) return { dot: "bg-emerald-500", text: "text-emerald-700 dark:text-emerald-300" };
  return { dot: "bg-muted-foreground/50", text: "text-muted-foreground" };
}

function providerLabel(provider: AuditDTO["provider"]): string {
  switch (provider) {
    case "grok_build":
      return "Grok Build";
    case "grok_web":
      return "Grok Web";
    case "grok_console":
      return "Grok Console";
  }
}

function auditProtocolLabel(operation: AuditDTO["operation"]): string {
  switch (operation) {
    case "responses": return "Responses";
    case "compaction": return "Responses Compact";
    case "chat": return "Chat Completions";
    case "messages": return "Anthropic Messages";
    case "image":
    case "image_edit": return "Images";
    case "video": return "Videos";
    case "tts": return "Audio Speech";
    case "stt": return "Audio Transcriptions";
    case "realtime": return "Realtime";
    case "voice": return "Voice";
  }
}

function providerShortLabel(provider: AuditDTO["provider"]): string {
  switch (provider) {
    case "grok_build":
      return "Build";
    case "grok_web":
      return "Web";
    case "grok_console":
      return "Console";
  }
}

function auditFilterOptionSearch(value: string): string {
  const trimmed = value.trim();
  return /^\d+$/.test(trimmed) ? `#${trimmed}` : trimmed;
}

function formatUSDCostCompact(ticks: number): string {
  const value = usdTicksToValue(ticks).toFixed(10).replace(/0+$/, "").replace(/\.$/, "");
  return `$${value}`;
}
