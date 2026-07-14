import { useQuery } from "@tanstack/react-query";
import { Activity, ArrowDown, ArrowUp, Box, BrainCircuit, CircleCheck, CircleDollarSign, CornerDownRight, Database, Info, RefreshCw, Search, type LucideIcon } from "lucide-react";
import { useRef, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { Table, TableBody, TableCell, TableHeader, TableRow } from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { listModels } from "@/entities/model/model-api";
import { getRequestAudits, getRequestAuditSummary, type AuditDTO, type AuditPeriod } from "@/features/audits/request-audits-api";
import { EmptyState, ErrorState, TableLoadingRow } from "@/shared/components/data-state";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { DataTableFilters } from "@/shared/components/data-table-filters";
import { CursorPagination } from "@/shared/components/pagination";
import { PeriodSelector } from "@/shared/components/period-selector";
import { SortableTableHead } from "@/shared/components/sortable-table-head";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { cn } from "@/shared/lib/cn";
import { formatDateTime, formatDuration, formatNumber } from "@/shared/lib/format";
import { toPeriodValue, type PeriodDays } from "@/shared/lib/period";
import { nextTableSort, type SortOrder, type TableSort } from "@/shared/lib/table-sort";

export function RequestAuditsPage() {
  const { t, i18n } = useTranslation();
  const [cursors, setCursors] = useState<string[]>([""]);
  const [pageSize, setPageSize] = useState(50);
  const [search, setSearch] = useState("");
  const [modelFilter, setModelFilter] = useState("");
  const [statusFilter, setStatusFilter] = useState("");
  const [modeFilter, setModeFilter] = useState("");
  const [keyFilter, setKeyFilter] = useState("");
  const [accountFilter, setAccountFilter] = useState("");
  const [periodDays, setPeriodDays] = useState<PeriodDays>(1);
  const [sort, setSort] = useState<TableSort>({ field: "createdAt", order: "desc" });
  const [manualRefreshing, setManualRefreshing] = useState(false);
  const forceSummaryRefresh = useRef(false);
  const debouncedSearch = useDebouncedValue(search);
  const debouncedKeyFilter = useDebouncedValue(keyFilter);
  const debouncedAccountFilter = useDebouncedValue(accountFilter);
  const cursor = cursors[cursors.length - 1];
  const period: AuditPeriod = toPeriodValue(periodDays);
  const auditsQuery = useQuery({
    queryKey: ["request-audits", "cursor", cursor, pageSize, debouncedSearch, modelFilter, statusFilter, modeFilter, debouncedKeyFilter, debouncedAccountFilter, period, sort.field, sort.order],
    queryFn: () => getRequestAudits({ cursor, pageSize, search: debouncedSearch, model: modelFilter, status: statusFilter, mode: modeFilter, key: debouncedKeyFilter, account: debouncedAccountFilter, period, sortBy: sort.field, sortOrder: sort.order }),
  });
  const summaryQuery = useQuery({
    queryKey: ["request-audits", "summary", debouncedSearch, modelFilter, statusFilter, modeFilter, debouncedKeyFilter, debouncedAccountFilter, period],
    queryFn: () => getRequestAuditSummary({ search: debouncedSearch, model: modelFilter, status: statusFilter, mode: modeFilter, key: debouncedKeyFilter, account: debouncedAccountFilter, period }, forceSummaryRefresh.current),
    placeholderData: (previous) => previous,
  });
  const modelOptionsQuery = useQuery({
    queryKey: ["models", "audit-filter"],
    queryFn: () => listModels({ page: 1, pageSize: 100 }),
    staleTime: 60_000,
  });
  const result = auditsQuery.data;
  const summary = summaryQuery.data;
  const summaryLoading = summaryQuery.isPending || summaryQuery.isPlaceholderData;
  const cacheRate = summary?.usage.inputTokens ? summary.usage.cachedInputTokens / summary.usage.inputTokens * 100 : 0;

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

  function changeSort(field: string, initialOrder: SortOrder): void {
    setSort((current) => nextTableSort(current, field, initialOrder));
    setCursors([""]);
  }

  return (
    <div className="space-y-8">
      <header>
        <h1 className="text-xl font-medium">{t("audits.title")}</h1>
        <p className="sr-only">{t("audits.description")}</p>
      </header>

      <section className="space-y-4">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <h2 className="text-sm font-medium">{t("audits.usageSummary")}</h2>
          <PeriodSelector value={periodDays} onChange={(days) => { setPeriodDays(days); setCursors([""]); }} ariaLabel={t("audits.usageSummary")} />
        </div>
        <div className="grid gap-2 sm:grid-cols-2 xl:grid-cols-4">
          <AuditMetric icon={Activity} loading={summaryLoading} label={t("audits.totalRequests")} value={formatNumber(summary?.usage.requests ?? 0, i18n.language, 0)} detail={t("audits.requestBreakdown", { success: formatNumber(summary?.usage.successfulRequests ?? 0, i18n.language, 0), failed: formatNumber(summary?.usage.failedRequests ?? 0, i18n.language, 0) })} />
          <AuditMetric icon={Box} loading={summaryLoading} label={t("audits.totalTokens")} value={formatNumber(summary?.usage.totalTokens ?? 0, i18n.language, 0)} detail={t("audits.tokenEfficiency", { cacheRate: formatNumber(cacheRate, i18n.language, 1) })} />
          <AuditMetric icon={CircleCheck} loading={summaryLoading} label={t("audits.successRate")} value={`${formatNumber(summary?.usage.successRate ?? 0, i18n.language, 1)}%`} detail={t("audits.averageDuration", { duration: formatDuration(summary?.usage.averageDurationMs ?? 0) })} />
          <AuditMetric
            icon={CircleDollarSign}
            loading={summaryLoading}
            label={t("audits.estimatedCost")}
            value={(summary?.pricing.pricedRequests ?? 0) > 0 ? `$${((summary?.usage.estimatedCostInUsdTicks ?? 0) / 10_000_000_000).toFixed(6)}` : "-"}
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
                <Input className="h-8 pl-9 text-xs" value={search} onChange={(event) => { setSearch(event.target.value); setCursors([""]); }} placeholder={t("audits.search")} aria-label={t("audits.search")} />
              </div>
              <DataTableFilters filters={[
                { id: "model", label: t("audits.model"), value: modelFilter, onChange: (value) => { setModelFilter(value); setCursors([""]); }, options: [...new Map((modelOptionsQuery.data?.items ?? []).map((model) => [model.publicId, { value: model.publicId, label: model.publicId }])).values()] },
                { id: "status", label: t("audits.status"), value: statusFilter, onChange: (value) => { setStatusFilter(value); setCursors([""]); }, options: [
                  { value: "2xx", label: `2xx · ${t("audits.statusSuccess")}` },
                  { value: "4xx", label: `4xx · ${t("audits.statusClientError")}` },
                  { value: "5xx", label: `5xx · ${t("audits.statusServerError")}` },
                ] },
                { id: "mode", label: t("audits.mode"), value: modeFilter, onChange: (value) => { setModeFilter(value); setCursors([""]); }, options: [
                  { value: "stream", label: "Stream" },
                  { value: "nonStream", label: "Non-Stream" },
                ] },
                { id: "key", type: "text", label: t("audits.key"), value: keyFilter, placeholder: t("audits.keyFilterPlaceholder"), onChange: (value) => { setKeyFilter(value); setCursors([""]); } },
                { id: "account", type: "text", label: t("audits.account"), value: accountFilter, placeholder: t("audits.accountFilterPlaceholder"), onChange: (value) => { setAccountFilter(value); setCursors([""]); } },
              ]} />
            </div>
            <Button variant="ghost" size="sm" className="text-muted-foreground" onClick={refreshAll} disabled={auditsQuery.isFetching || summaryQuery.isFetching || manualRefreshing}><RefreshCw className={manualRefreshing ? "animate-spin" : undefined} />{t("common.refresh")}</Button>
          </>
        )}
        footer={result && result.items.length > 0 ? (
          <CursorPagination
            page={cursors.length}
            pageSize={pageSize}
            hasMore={result.hasMore && Boolean(result.nextCursor)}
            onFirstPage={() => setCursors([""])}
            onPreviousPage={() => setCursors((values) => values.slice(0, -1))}
            onNextPage={() => setCursors((values) => [...values, result.nextCursor])}
            onPageSizeChange={(value) => { setPageSize(value); setCursors([""]); }}
          />
        ) : undefined}
      >
        {auditsQuery.isError ? <ErrorState message={auditsQuery.error.message} onRetry={() => void auditsQuery.refetch()} /> : null}
        {result && result.items.length === 0 ? <EmptyState /> : null}
        {auditsQuery.isPending || (result && result.items.length > 0) ? (
          <Table className="min-w-[1160px] table-fixed text-xs">
            <colgroup>
              <col className="w-[14%]" />
              <col className="w-[14%]" />
              <col className="w-[10%]" />
              <col className="w-[26%]" />
              <col className="w-[8%]" />
              <col className="w-[10%]" />
              <col className="w-[7%]" />
              <col className="w-[11%]" />
            </colgroup>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <SortableTableHead field="request" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("audits.request")}</SortableTableHead>
                <SortableTableHead field="model" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("audits.model")}</SortableTableHead>
                <SortableTableHead field="billing" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("audits.billing")}</SortableTableHead>
                <SortableTableHead field="tokens" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("audits.tokens")}</SortableTableHead>
                <SortableTableHead field="status" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort}>{t("audits.status")}</SortableTableHead>
                <SortableTableHead field="mode" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort} className="whitespace-nowrap">{t("audits.mode")}</SortableTableHead>
                <SortableTableHead field="duration" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("audits.duration")}</SortableTableHead>
                <SortableTableHead field="createdAt" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("audits.createdAt")}</SortableTableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {auditsQuery.isPending ? <TableLoadingRow colSpan={8} /> : result?.items.map((audit) => (
                <TableRow key={audit.id}>
                  <TableCell className="py-3">
                    <span className="block truncate text-xs" title={audit.requestId}>{audit.requestId}</span>
                  </TableCell>
                  <TableCell className="py-3">
                    <ModelRouteValue
                      model={audit.modelPublicId || `#${audit.modelRouteId}`}
                      upstreamModel={audit.modelUpstreamModel || "-"}
                      account={audit.accountName || (audit.accountId ? `#${audit.accountId}` : "-")}
                      clientKey={audit.clientKeyName || `#${audit.clientKeyId}`}
                    />
                  </TableCell>
                  <TableCell className="py-3"><BillingValue audit={audit} /></TableCell>
                  <TableCell className="py-3"><UsageDetails audit={audit} locale={i18n.language} /></TableCell>
                  <TableCell className="py-3 text-center"><AuditStatus statusCode={audit.statusCode} errorCode={audit.errorCode} /></TableCell>
                  <TableCell className="py-3 text-center"><Badge variant="outline" className="whitespace-nowrap font-normal">{audit.streaming ? "Stream" : "Non-Stream"}</Badge></TableCell>
                  <TableCell className="whitespace-nowrap py-3 text-xs tabular-nums">{formatNumber(audit.durationMs, i18n.language)} ms</TableCell>
                  <TableCell className="whitespace-nowrap py-3 text-xs text-muted-foreground">{formatDateTime(audit.createdAt, i18n.language)}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        ) : null}
      </DataTableShell>
    </div>
  );
}

function BillingValue({ audit }: { audit: AuditDTO }) {
  const { t } = useTranslation();
  const upstreamReported = audit.costInUsdTicks > 0;
  const priced = upstreamReported || Boolean(audit.pricingModel);
  const ticks = upstreamReported ? audit.costInUsdTicks : audit.estimatedCostInUsdTicks;
  const amount = priced ? `$${(ticks / 10_000_000_000).toFixed(6)}` : "-";
  return (
    <div className="max-w-full text-left">
      <span className="block whitespace-nowrap text-xs tabular-nums">{amount}</span>
      {audit.numServerSideToolsUsed > 0 ? (
        <span className="mt-0.5 block whitespace-nowrap text-[10px] text-muted-foreground">
          {t("audits.serverTools", { count: audit.numServerSideToolsUsed })}
        </span>
      ) : null}
    </div>
  );
}

function AuditMetric({ icon: Icon, label, value, detail, tooltip, loading }: { icon: LucideIcon; label: string; value: string; detail?: ReactNode; tooltip?: string; loading: boolean }) {
  return (
    <div className="min-h-28 rounded-lg bg-card p-4">
      <div className="flex items-center justify-between gap-3">
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
      </div>
      {loading ? (
        <div className="mt-3 flex min-h-7 items-center"><Spinner /></div>
      ) : (
        <>
          <div className="mt-3 flex min-h-7 items-center text-xl font-medium tabular-nums">{value}</div>
          {detail ? <div className="mt-1 truncate whitespace-nowrap text-xs text-muted-foreground" title={typeof detail === "string" ? detail : undefined}>{detail}</div> : null}
        </>
      )}
    </div>
  );
}

function AuditTokenMetric({ icon: Icon, label, value, loading }: { icon: LucideIcon; label: string; value: string; loading: boolean }) {
  return (
    <div className="flex min-h-12 min-w-0 items-center justify-between gap-3 rounded-lg bg-muted/55 px-4 py-2">
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
    <div className="w-full max-w-[260px]">
      <div className="grid grid-cols-2 gap-1">
        {items.map((item) => (
          <div key={item.label} className="flex h-7 items-center justify-between rounded-md bg-muted/55 px-2 text-[11px]">
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
    <div className="grid w-full max-w-[260px] gap-1">
      <div className="flex h-7 items-center justify-between rounded-md bg-muted/55 px-2 text-[11px]">
        <span className="text-muted-foreground">{t("audits.mediaInput")}</span>
        <span className="font-medium tabular-nums">{input}</span>
      </div>
      <div className="flex h-7 items-center justify-between rounded-md bg-muted/55 px-2 text-[11px]">
        <span className="text-muted-foreground">{t("audits.output")}</span>
        <span className="font-medium tabular-nums">{output}</span>
      </div>
    </div>
  );
}

function StatusBadge({ statusCode }: { statusCode: number }) {
  const compactClassName = "min-w-9";
  if (statusCode >= 200 && statusCode < 300) {
    return <Badge variant="secondary" className={cn(compactClassName, "bg-emerald-500/10 text-emerald-700 dark:text-emerald-300")}>{statusCode}</Badge>;
  }
  if (statusCode >= 400 && statusCode < 500) {
    return <Badge variant="secondary" className={cn(compactClassName, "bg-amber-500/10 text-amber-700 dark:text-amber-300")}>{statusCode}</Badge>;
  }
  if (statusCode >= 500) {
    return <Badge variant="secondary" className={cn(compactClassName, "bg-red-500/10 text-red-700 dark:text-red-300")}>{statusCode}</Badge>;
  }
  return <Badge variant="secondary" className={cn(compactClassName, "bg-muted text-muted-foreground")}>{statusCode || "-"}</Badge>;
}

function AuditStatus({ statusCode, errorCode }: { statusCode: number; errorCode?: string }) {
  if (!errorCode) return <StatusBadge statusCode={statusCode} />;
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button type="button" className="cursor-help" aria-label={errorCode}>
          <StatusBadge statusCode={statusCode} />
        </button>
      </TooltipTrigger>
      <TooltipContent className="max-w-80 whitespace-normal break-words text-left leading-5" side="top">
        {errorCode}
      </TooltipContent>
    </Tooltip>
  );
}
