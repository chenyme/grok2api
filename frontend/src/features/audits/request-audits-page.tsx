import { useQuery } from "@tanstack/react-query";
import { Activity, ArrowDown, ArrowUp, BrainCircuit, CircleCheck, CircleDollarSign, CornerDownRight, Database, Info, Minimize2, RefreshCw, Search, WholeWord, type LucideIcon } from "lucide-react";
import { memo, useCallback, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { listModels } from "@/entities/model/model-api";
import { RequestAuditDetailDialog } from "@/features/audits/request-audit-detail-dialog";
import { getRequestAudits, getRequestAuditSummary, type AuditDTO, type AuditPeriod } from "@/features/audits/request-audits-api";
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
import { formatDateTime, formatDuration, formatNumber } from "@/shared/lib/format";
import { toPeriodValue, type PeriodDays } from "@/shared/lib/period";
import { nextTableSort, type SortOrder, type TableSort } from "@/shared/lib/table-sort";

const AUDIT_PAGE_CACHE_TIME_MS = 60_000;
const AUDIT_SUMMARY_CACHE_TIME_MS = 120_000;

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
            value={hasEstimatedCost ? formatUSDCost(estimatedCostTicks, 2) : "-"}
            fullValue={hasEstimatedCost ? formatUSDCost(estimatedCostTicks, 10) : undefined}
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
                ] },
                { id: "mode", label: t("audits.mode"), value: modeFilter, onChange: setModeFilter, options: [
                  { value: "stream", label: t("audits.stream") },
                  { value: "nonStream", label: t("audits.nonStream") },
                ] },
                { id: "key", type: "text", label: t("audits.key"), value: keyFilter, placeholder: t("audits.keyFilterPlaceholder"), onChange: setKeyFilter },
                { id: "account", type: "text", label: t("audits.account"), value: accountFilter, placeholder: t("audits.accountFilterPlaceholder"), onChange: setAccountFilter },
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
          <Table viewportRows={20} rowHeight={72} aria-busy={auditsQuery.isFetching} className={cn("min-w-[1136px] table-fixed text-xs transition-opacity", auditsQuery.isPlaceholderData && "pointer-events-none opacity-60")}>
            <colgroup>
              <col className="w-36" />
              <col className="w-44" />
              <col className="w-20" />
              <col className="w-24" />
              <col className="w-76" />
              <col className="w-20" />
              <col className="w-20" />
              <col className="w-44" />
            </colgroup>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <SortableTableHead field="request" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("audits.request")}</SortableTableHead>
                <SortableTableHead field="model" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("audits.model")}</SortableTableHead>
                <TableHead>{t("audits.egress")}</TableHead>
                <SortableTableHead field="billing" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("audits.billing")}</SortableTableHead>
                <SortableTableHead field="tokens" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" className="px-3" onSort={changeSort}>{t("audits.tokens")}</SortableTableHead>
                <SortableTableHead field="status" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort}>{t("audits.status")}</SortableTableHead>
                <SortableTableHead field="duration" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("audits.duration")}</SortableTableHead>
                <SortableTableHead field="createdAt" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("audits.createdAt")}</SortableTableHead>
              </TableRow>
            </TableHeader>
            {auditsQuery.isPending ? (
              <TableBody><TableLoadingRow colSpan={8} /></TableBody>
            ) : (
              <VirtualTableBody items={result?.items ?? []} colSpan={8} rowHeight={72} overscan={6} renderRow={renderAuditRow} />
            )}
          </Table>
        ) : null}
      </DataTableShell>
      <RequestAuditDetailDialog key={selectedAudit?.id ?? "closed"} audit={selectedAudit} open={selectedAudit !== null} onOpenChange={(open) => !open && setSelectedAudit(null)} />
    </div>
  );
}

const AuditRow = memo(function AuditRow({ audit, locale, onOpen }: { audit: AuditDTO; locale: string; onOpen: (audit: AuditDTO) => void }) {
  return (
    <TableRow className="h-[72px]">
      <TableCell><RequestValue audit={audit} /></TableCell>
      <TableCell>
        <ModelRouteValue
          model={audit.modelPublicId || `#${audit.modelRouteId}`}
          upstreamModel={audit.modelUpstreamModel || "-"}
          account={audit.accountName || (audit.accountId ? `#${audit.accountId}` : "-")}
          clientKey={audit.clientKeyName || `#${audit.clientKeyId}`}
        />
      </TableCell>
      <TableCell><EgressValue audit={audit} /></TableCell>
      <TableCell><BillingValue audit={audit} /></TableCell>
      <TableCell className="px-3"><UsageDetails audit={audit} locale={locale} /></TableCell>
      <TableCell className="text-center"><AuditStatus audit={audit} onOpen={() => onOpen(audit)} /></TableCell>
      <TableCell className="whitespace-nowrap text-xs tabular-nums">{formatDuration(audit.durationMs)}</TableCell>
      <TableCell className="whitespace-nowrap text-xs text-muted-foreground">{formatDateTime(audit.createdAt, locale)}</TableCell>
    </TableRow>
  );
});

function RequestValue({ audit }: { audit: AuditDTO }) {
  const { t } = useTranslation();
  return (
    <div className="min-w-0">
      <span className="block truncate text-xs font-medium">{providerLabel(audit.provider)} · {t(`audits.operations.${audit.operation}`)}</span>
      <span className="mt-0.5 block truncate font-mono text-[10px] text-muted-foreground" title={audit.requestId}>{audit.requestId}</span>
    </div>
  );
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
        <button type="button" className="block min-w-0 max-w-full cursor-help text-left" aria-label={`${proxied ? t("audits.egressProxy") : t("audits.egressDirect")}: ${node}`}>
          <span className={cn("inline-flex items-center gap-1.5 text-xs", proxied ? "text-emerald-700 dark:text-emerald-300" : "text-muted-foreground")}>
            <span className={cn("size-1.5 rounded-full", proxied ? "bg-emerald-500" : "bg-muted-foreground/50")} />
            {proxied ? t("audits.egressProxy") : t("audits.egressDirect")}
          </span>
        </button>
      </TooltipTrigger>
      <TooltipContent className="max-w-72" side="top" align="start">
        <div>{node}</div>
        {details ? <div className="mt-1 text-primary-foreground/65">{details}</div> : null}
      </TooltipContent>
    </Tooltip>
  );
}

function BillingValue({ audit }: { audit: AuditDTO }) {
  const { t } = useTranslation();
  const upstreamReported = audit.costInUsdTicks > 0;
  const priced = upstreamReported || Boolean(audit.pricingModel);
  const ticks = upstreamReported ? audit.costInUsdTicks : audit.estimatedCostInUsdTicks;
  const amount = priced ? formatUSDCost(ticks, 2) : "-";
  const fullAmount = priced ? formatUSDCost(ticks, 10) : "";
  return (
    <div className="max-w-full text-left">
      {priced ? (
        <Tooltip>
          <TooltipTrigger asChild><span className="block cursor-help whitespace-nowrap text-xs tabular-nums" tabIndex={0}>{amount}</span></TooltipTrigger>
          <TooltipContent side="top"><span className="text-primary-foreground/65">{t("audits.exactBilling")}</span> <span className="font-mono">{fullAmount}</span></TooltipContent>
        </Tooltip>
      ) : <span className="block text-xs text-muted-foreground">-</span>}
      {audit.numServerSideToolsUsed > 0 ? (
        <span className="mt-0.5 block whitespace-nowrap text-[10px] text-muted-foreground">
          {t("audits.serverTools", { count: audit.numServerSideToolsUsed })}
        </span>
      ) : null}
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

function ModelRouteValue({ model, upstreamModel, account, clientKey }: { model: string; upstreamModel: string; account: string; clientKey: string }) {
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
        </button>
      </TooltipTrigger>
      <TooltipContent className="w-64 space-y-1.5 py-2" side="top" align="start">
        <div className="grid grid-cols-[auto_1fr] gap-x-3">
          <span className="text-primary-foreground/65">{t("audits.owningAccount")}</span>
          <span className="truncate text-right" title={account}>{account}</span>
        </div>
        <div className="grid grid-cols-[auto_1fr] gap-x-3">
          <span className="text-primary-foreground/65">{t("audits.owningKey")}</span>
          <span className="truncate text-right" title={clientKey}>{clientKey}</span>
        </div>
      </TooltipContent>
    </Tooltip>
  );
}

function UsageDetails({ audit, locale }: { audit: AuditDTO; locale: string }) {
  const { t } = useTranslation();
  if (audit.operation === "compaction" && audit.totalTokens === 0) {
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
  if (audit.operation === "video") {
    return <MediaUsage input={t("audits.imageCount", { count: audit.mediaInputImages })} output={t("audits.secondsCount", { count: audit.mediaOutputSeconds })} />;
  }
  if (audit.operation === "image" || audit.operation === "image_edit" || audit.mediaInputImages > 0 || audit.mediaOutputImages > 0) {
    return <MediaUsage input={t("audits.imageCount", { count: audit.mediaInputImages })} output={t("audits.imageCount", { count: audit.mediaOutputImages })} />;
  }
  const items = [
    { label: t("audits.input"), value: audit.inputTokens },
    { label: t("audits.output"), value: audit.outputTokens },
    { label: t("audits.cached"), value: audit.cachedInputTokens },
    { label: t("audits.reasoning"), value: audit.reasoningTokens },
  ];
  return (
    <div className="w-full">
      <div className="grid grid-cols-2 gap-1">
        {items.map((item) => (
          <div key={item.label} className="flex h-6 min-w-0 items-center justify-between gap-2 rounded-md bg-muted/45 px-2 text-[11px]">
            <span className="text-muted-foreground">{item.label}</span>
            <span className="font-medium tabular-nums">{formatNumber(item.value, locale)}</span>
          </div>
        ))}
      </div>
      {audit.numSourcesUsed > 0 ? (
        <div className="mt-1 flex flex-wrap gap-x-3 text-[10px] text-muted-foreground">
          <span>{t("audits.sources", { count: audit.numSourcesUsed })}</span>
        </div>
      ) : null}
    </div>
  );
}

function MediaUsage({ input, output }: { input: string; output: string }) {
  const { t } = useTranslation();
  return (
    <div className="grid w-full gap-1">
      <div className="flex h-6 items-center justify-between gap-3 rounded-md bg-muted/45 px-2 text-[11px]">
        <span className="text-muted-foreground">{t("audits.mediaInput")}</span>
        <span className="font-medium tabular-nums">{input}</span>
      </div>
      <div className="flex h-6 items-center justify-between gap-3 rounded-md bg-muted/45 px-2 text-[11px]">
        <span className="text-muted-foreground">{t("audits.output")}</span>
        <span className="font-medium tabular-nums">{output}</span>
      </div>
    </div>
  );
}

function StatusCode({ statusCode, hasError = false }: { statusCode: number; hasError?: boolean }) {
  const tone = statusTone(statusCode, hasError);
  return (
    <span className={cn("inline-flex items-center gap-1.5 text-xs tabular-nums", tone.text)}>
      <span className={cn("size-1.5 rounded-full", tone.dot)} />
      {statusCode || "-"}
    </span>
  );
}

function AuditStatus({ audit, onOpen }: { audit: AuditDTO; onOpen: () => void }) {
  const { t } = useTranslation();
  const mode = audit.operation === "compaction" ? t("audits.operations.compaction") : audit.streaming ? t("audits.stream") : t("audits.nonStream");
  const content = (
    <>
      <StatusCode statusCode={audit.statusCode} hasError={Boolean(audit.errorCode)} />
      <span className="block whitespace-nowrap text-[10px] text-muted-foreground">{mode}</span>
    </>
  );
  if (!audit.errorCode && audit.attemptCount === 0) return <div className="space-y-0.5 text-center">{content}</div>;
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button type="button" className="group space-y-0.5 rounded-md text-center outline-none focus-visible:ring-2 focus-visible:ring-ring/50 [&>span:last-child]:underline-offset-2 hover:[&>span:last-child]:text-foreground hover:[&>span:last-child]:underline" aria-label={t("audits.openDiagnostics")} onClick={onOpen}>{content}</button>
      </TooltipTrigger>
      <TooltipContent className="max-w-80 whitespace-normal break-words text-left leading-5" side="top">
        {audit.errorCode || t("audits.openDiagnostics")}
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

function formatUSDCost(ticks: number, fractionDigits: number): string {
  return `$${(ticks / 10_000_000_000).toFixed(fractionDigits)}`;
}
