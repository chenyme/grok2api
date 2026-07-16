import { useQuery } from "@tanstack/react-query";
import { Activity, Box, CircleDollarSign, RefreshCw, Users } from "lucide-react";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Area, Bar, CartesianGrid, ComposedChart, Line, XAxis, YAxis } from "recharts";

import { Button } from "@/components/ui/button";
import { ChartContainer, ChartLegend, ChartLegendContent, ChartTooltip, ChartTooltipContent, type ChartConfig } from "@/components/ui/chart";
import { Spinner } from "@/components/ui/spinner";
import { getDashboard, type DashboardPeriod, type DashboardDTO } from "@/features/dashboard/dashboard-api";
import { VersionUpdateBanner } from "@/features/system/version-update";
import { useAuth } from "@/shared/auth/use-auth";
import { ErrorState } from "@/shared/components/data-state";
import { PeriodSelector } from "@/shared/components/period-selector";
import { cn } from "@/shared/lib/cn";
import { formatDateTime, formatNumber } from "@/shared/lib/format";
import { PERIOD_DAYS, toPeriodValue, type PeriodDays } from "@/shared/lib/period";

const USD_TICKS = 10_000_000_000;

type TrendMetric = "tokens" | "billing";

type DashboardPreferences = { periodDays: PeriodDays; trendMetric: TrendMetric };

const DASHBOARD_PREFERENCES_KEY = "grok2api:dashboard-preferences";
const DEFAULT_DASHBOARD_PREFERENCES: DashboardPreferences = { periodDays: 30, trendMetric: "tokens" };

const MODEL_CHART_COLORS = [
  { light: "oklch(0.76 0.1 205)", dark: "oklch(0.72 0.1 205)" },
  { light: "oklch(0.77 0.1 160)", dark: "oklch(0.73 0.1 160)" },
  { light: "oklch(0.8 0.11 85)", dark: "oklch(0.76 0.11 85)" },
  { light: "oklch(0.77 0.11 30)", dark: "oklch(0.73 0.11 30)" },
  { light: "oklch(0.77 0.1 300)", dark: "oklch(0.73 0.1 300)" },
  { light: "oklch(0.74 0.09 185)", dark: "oklch(0.7 0.09 185)" },
  { light: "oklch(0.8 0.1 125)", dark: "oklch(0.76 0.1 125)" },
  { light: "oklch(0.78 0.1 345)", dark: "oklch(0.74 0.1 345)" },
  { light: "oklch(0.8 0.09 55)", dark: "oklch(0.76 0.09 55)" },
  { light: "oklch(0.76 0.09 275)", dark: "oklch(0.72 0.09 275)" },
] as const;

export function DashboardPage() {
  const { t, i18n } = useTranslation();
  const { admin } = useAuth();
  const [preferences, setPreferences] = useState<DashboardPreferences>(readDashboardPreferences);
  const { periodDays, trendMetric } = preferences;
  const [manualRefreshing, setManualRefreshing] = useState(false);
  const forceRefresh = useRef(false);
  const period: DashboardPeriod = toPeriodValue(periodDays);
  const timezone = Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  useEffect(() => {
    saveDashboardPreferences(preferences);
  }, [preferences]);
  const dashboardQuery = useQuery({
    queryKey: ["dashboard", period, timezone],
    queryFn: () => getDashboard(period, timezone, forceRefresh.current),
    placeholderData: (previous) => previous,
  });

  function refreshAll(): void {
    setManualRefreshing(true);
    forceRefresh.current = true;
    void Promise.all([
      dashboardQuery.refetch(),
      new Promise<void>((resolve) => window.setTimeout(resolve, 400)),
    ]).finally(() => {
      forceRefresh.current = false;
      setManualRefreshing(false);
    });
  }

  if (dashboardQuery.isError) {
    return <ErrorState message={dashboardQuery.error.message} onRetry={refreshAll} />;
  }

  const dashboard = dashboardQuery.data;
  const resources = dashboard?.resources;
  const usage = dashboard?.usage;
  const activeAccounts = resources?.activeAccounts ?? 0;
  const cacheHitRate = (usage?.inputTokens ?? 0) > 0 ? (usage?.cachedInputTokens ?? 0) / (usage?.inputTokens ?? 1) * 100 : 0;
  const loading = dashboardQuery.isPending;
  const displayName = admin?.username ? admin.username.charAt(0).toUpperCase() + admin.username.slice(1) : "Admin";

  return (
    <div className="space-y-8">
      <div className="space-y-5">
        <header>
          <div>
            <h1 className="text-xl font-medium">{t("dashboard.title")}</h1>
            <p className="mt-1 text-xs text-muted-foreground">
              {t("dashboard.subtitle", { name: displayName })}
              {dashboard?.generatedAt ? <span> · {t("dashboard.lastUpdated", { time: formatDateTime(dashboard.generatedAt, i18n.language) })}</span> : null}
            </p>
          </div>
        </header>

        <VersionUpdateBanner />

        <section className="space-y-4">
          <div className="flex items-center justify-between gap-3">
            <h2 className="shrink-0 text-sm font-medium">{t("dashboard.usage")}</h2>
            <div className="flex min-w-0 shrink-0 items-center gap-2">
              <Button variant="ghost" size="icon" className="size-8 text-muted-foreground" onClick={refreshAll} disabled={dashboardQuery.isFetching || manualRefreshing} aria-label={t("common.refresh")}><RefreshCw className={manualRefreshing ? "animate-spin" : undefined} /></Button>
              <PeriodSelector value={periodDays} onChange={(value) => setPreferences((current) => ({ ...current, periodDays: value }))} ariaLabel={t("dashboard.usage")} />
            </div>
          </div>

          <div className="grid gap-2 sm:grid-cols-2 xl:grid-cols-4">
            <MetricCard icon={<Users />} label={t("dashboard.activeAccounts")} value={formatNumber(activeAccounts, i18n.language)} detail={t("dashboard.availableSummary", { active: activeAccounts, total: resources?.totalAccounts ?? 0 })} loading={loading} />
            <MetricCard icon={<Activity />} label={t("dashboard.requests")} value={formatNumber(usage?.requests ?? 0, i18n.language)} detail={t("dashboard.requestQualitySummary", { success: formatNumber(usage?.successRate ?? 0, i18n.language, 1), failed: usage?.failedRequests ?? 0 })} loading={loading} />
            <MetricCard icon={<Box />} label={t("dashboard.tokens")} value={formatNumber(usage?.tokens ?? 0, i18n.language)} detail={t("dashboard.tokenEfficiency", { rate: formatNumber(cacheHitRate, i18n.language, 1) })} loading={loading} />
            <MetricCard icon={<CircleDollarSign />} label={t("dashboard.billing")} value={formatUSD(usage?.billedCostUsdTicks ?? 0, i18n.language)} detail={t("dashboard.billingSummary", { period })} loading={loading} />
          </div>
        </section>
      </div>

      <TrendPanel dashboard={dashboard} metric={trendMetric} onMetricChange={(value) => setPreferences((current) => ({ ...current, trendMetric: value }))} locale={i18n.language} loading={loading} />

      <TopModels dashboard={dashboard} locale={i18n.language} loading={loading} />
    </div>
  );
}

function readDashboardPreferences(): DashboardPreferences {
  if (typeof window === "undefined") return DEFAULT_DASHBOARD_PREFERENCES;
  try {
    const value: unknown = JSON.parse(window.localStorage.getItem(DASHBOARD_PREFERENCES_KEY) ?? "null");
    if (!value || typeof value !== "object") return DEFAULT_DASHBOARD_PREFERENCES;
    const candidate = value as Record<string, unknown>;
    const periodDays = PERIOD_DAYS.find((days) => days === candidate.periodDays);
    const trendMetric = candidate.trendMetric;
    if (periodDays === undefined || (trendMetric !== "tokens" && trendMetric !== "billing")) return DEFAULT_DASHBOARD_PREFERENCES;
    return { periodDays, trendMetric };
  } catch {
    return DEFAULT_DASHBOARD_PREFERENCES;
  }
}

function saveDashboardPreferences(value: DashboardPreferences): void {
  try {
    window.localStorage.setItem(DASHBOARD_PREFERENCES_KEY, JSON.stringify(value));
  } catch {
    return;
  }
}

function MetricCard({ icon, label, value, detail, loading }: { icon: ReactNode; label: string; value: string; detail: string; loading: boolean }) {
  return (
    <div className="min-h-28 rounded-lg bg-card p-4">
      <div className="flex items-center justify-between gap-3">
        <span className="text-xs text-muted-foreground">{label}</span>
        <span className="flex size-5 items-center justify-center text-muted-foreground [&_svg]:size-4">{icon}</span>
      </div>
      <div className="mt-3 flex min-h-7 items-center text-xl font-medium tabular-nums">{loading ? <Spinner /> : value}</div>
      <p className={cn("mt-1 text-xs text-muted-foreground", loading && "invisible")}>{detail}</p>
    </div>
  );
}

function TrendPanel({ dashboard, metric, onMetricChange, locale, loading }: { dashboard?: DashboardDTO; metric: TrendMetric; onMetricChange: (metric: TrendMetric) => void; locale: string; loading: boolean }) {
  const { t } = useTranslation();
  const modelSeries = (dashboard?.topModels ?? []).map((item, index) => ({ key: `model_${index}`, model: item.model, color: MODEL_CHART_COLORS[index % MODEL_CHART_COLORS.length] }));
  const chartData = dashboard?.series.map((bucket, index, series) => {
    const row: Record<string, string | number> = {
      requests: bucket.requests,
      tick: shouldShowTick(index, series.length, dashboard.period) ? formatBucketTick(bucket.start, dashboard.period, locale) : "",
      tooltipLabel: formatBucketRange(bucket.start, bucket.end, dashboard.period, locale),
    };
    let assigned = 0;
    for (const item of modelSeries) {
      const usage = bucket.models?.find((candidate) => candidate.model === item.model);
      const value = metric === "tokens" ? usage?.tokens ?? 0 : (usage?.billedCostUsdTicks ?? 0) / USD_TICKS;
      row[item.key] = value;
      assigned += value;
    }
    const total = metric === "tokens" ? bucket.tokens : (bucket.billedCostUsdTicks ?? 0) / USD_TICKS;
    row.other = Math.max(0, total - assigned);
    return row;
  }) ?? [];
  const chartConfig: ChartConfig = {
    requests: { label: t("dashboard.trendRequests"), theme: { light: "oklch(0.68 0.15 245)", dark: "oklch(0.74 0.13 245)" } },
    other: { label: t("dashboard.otherModels"), theme: { light: "oklch(0.82 0.04 245)", dark: "oklch(0.64 0.05 245)" } },
  };
  for (const item of modelSeries) {
    chartConfig[item.key] = { label: item.model, theme: item.color };
  }
  return (
    <section className="rounded-lg bg-card p-4 sm:p-5">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h2 className="text-sm font-medium">{t("dashboard.trend")}</h2>
        <div className="inline-flex h-8 items-center rounded-md bg-muted p-0.5">
          {(["tokens", "billing"] as const).map((value) => <Button key={value} type="button" variant="ghost" size="sm" className={cn("h-7 rounded-sm px-3 text-xs font-normal", metric === value && "bg-background shadow-sm hover:bg-background")} onClick={() => onMetricChange(value)}>{value === "tokens" ? t("dashboard.trendTokens") : t("dashboard.billing")}</Button>)}
        </div>
      </div>
      <ChartContainer config={chartConfig} className={cn("mt-4 h-[280px] w-full aspect-auto", loading && "opacity-40")}>
        <ComposedChart accessibilityLayer data={chartData} margin={{ left: 4, right: 8, top: 8, bottom: 0 }}>
          <defs>
            <linearGradient id="dashboard-requests-fill" x1="0" y1="0" x2="0" y2="1">
              <stop offset="5%" stopColor="var(--color-requests)" stopOpacity={0.24} />
              <stop offset="95%" stopColor="var(--color-requests)" stopOpacity={0.02} />
            </linearGradient>
          </defs>
          <CartesianGrid vertical={false} strokeDasharray="3 3" />
          <XAxis dataKey="tick" tickLine={false} axisLine={false} tickMargin={10} minTickGap={12} />
          <YAxis yAxisId="usage" tickLine={false} axisLine={false} tickMargin={8} width={48} allowDecimals={false} tickFormatter={(value) => metric === "billing" ? formatCompactUSD(Number(value), locale) : formatCompactNumber(Number(value), locale)} />
          <YAxis yAxisId="requests" orientation="right" tickLine={false} axisLine={false} tickMargin={8} width={40} allowDecimals={false} tickFormatter={(value) => formatCompactNumber(Number(value), locale)} />
          <ChartTooltip cursor={false} content={<ChartTooltipContent className="w-80 max-w-[calc(100vw-2rem)]" indicator="dot" labelFormatter={(_label, payload) => payload?.[0]?.payload?.tooltipLabel ?? ""} formatter={(value, name) => <div className="flex w-full items-center justify-between gap-4"><span className="min-w-0 truncate text-xs font-normal text-muted-foreground">{chartConfig[String(name)]?.label ?? name}</span><span className="shrink-0 font-mono text-xs font-normal tabular-nums text-muted-foreground">{metric === "billing" && name !== "requests" ? formatUSDValue(Number(value), locale) : formatNumber(Number(value), locale)}</span></div>} />} />
          <Area yAxisId="requests" dataKey="requests" type="monotone" stroke="none" fill="url(#dashboard-requests-fill)" dot={false} activeDot={false} legendType="none" tooltipType="none" />
          {modelSeries.map((item) => <Bar key={item.key} yAxisId="usage" dataKey={item.key} stackId="models" fill={`var(--color-${item.key})`} maxBarSize={36} />)}
          <Bar yAxisId="usage" dataKey="other" stackId="models" fill="var(--color-other)" maxBarSize={36} radius={[3, 3, 0, 0]} />
          <Line
            key={`requests-${dashboard?.period ?? "loading"}-${dashboard?.generatedAt ?? "loading"}`}
            yAxisId="requests"
            dataKey="requests"
            type="monotone"
            stroke="var(--color-requests)"
            strokeWidth={2}
            dot={false}
            activeDot={{ r: 3, fill: "var(--color-requests)", stroke: "var(--color-background)", strokeWidth: 2 }}
            animateNewValues={false}
            animationDuration={700}
            animationEasing="ease-out"
          />
          <ChartLegend content={<ChartLegendContent className="text-xs text-muted-foreground" />} />
        </ComposedChart>
      </ChartContainer>
    </section>
  );
}

function TopModels({ dashboard, locale, loading }: { dashboard?: DashboardDTO; locale: string; loading: boolean }) {
  const { t } = useTranslation();
  const models = dashboard?.topModels ?? [];
  return (
    <section className="rounded-lg bg-card p-4 sm:p-5">
      <h2 className="text-sm font-medium">{t("dashboard.topModels")}</h2>
      <div className="mt-4 overflow-x-auto">
        <div className="min-w-[1080px]">
          <div className="grid grid-cols-[minmax(220px,1fr)_80px_100px_100px_100px_100px_110px_100px] gap-4 border-b pb-2 text-[11px] text-muted-foreground">
            <span>{t("dashboard.model")}</span><span className="text-right">{t("dashboard.requests")}</span><span className="text-right">{t("dashboard.inputTokens")}</span><span className="text-right">{t("dashboard.cachedTokens")}</span><span className="text-right">{t("dashboard.outputTokens")}</span><span className="text-right">{t("dashboard.reasoningTokens")}</span><span className="text-right">{t("dashboard.tokens")}</span><span className="text-right">{t("dashboard.billing")}</span>
          </div>
          {loading ? <div className="flex h-28 items-center justify-center"><Spinner /></div> : models.length === 0 ? <div className="flex h-28 items-center justify-center text-xs text-muted-foreground">{t("dashboard.noTopModels")}</div> : (
            <div className="divide-y">
              {models.map((item, index) => {
                return (
                  <div key={item.model} className="grid grid-cols-[minmax(220px,1fr)_80px_100px_100px_100px_100px_110px_100px] items-center gap-4 py-3 text-xs">
                    <div className="flex min-w-0 items-center gap-3"><span className="w-5 shrink-0 text-right font-mono text-[11px] text-muted-foreground">{index + 1}</span><span className="truncate" title={item.model}>{item.model}</span></div>
                    <span className="text-right tabular-nums">{formatNumber(item.requests, locale)}</span>
                    <span className="text-right tabular-nums text-muted-foreground">{formatNumber(item.inputTokens, locale)}</span>
                    <span className="text-right tabular-nums text-muted-foreground">{formatNumber(item.cachedInputTokens, locale)}</span>
                    <span className="text-right tabular-nums text-muted-foreground">{formatNumber(item.outputTokens, locale)}</span>
                    <span className="text-right tabular-nums text-muted-foreground">{formatNumber(item.reasoningTokens, locale)}</span>
                    <span className="text-right tabular-nums">{formatNumber(item.tokens, locale)}</span>
                    <span className="text-right tabular-nums">{formatUSD(item.billedCostUsdTicks, locale)}</span>
                  </div>
                );
              })}
            </div>
          )}
        </div>
      </div>
    </section>
  );
}

function formatBucketRange(startValue: string | undefined, endValue: string | undefined, period: DashboardPeriod, locale: string): string {
  if (!startValue || !endValue) return "-";
  const start = new Date(startValue);
  const end = new Date(endValue);
  if (period === "24h") {
    const formatter = new Intl.DateTimeFormat(locale, { hour: "2-digit", minute: "2-digit", hourCycle: "h23" });
    return `${formatter.format(start)}–${formatter.format(end)}`;
  }
  const formatter = new Intl.DateTimeFormat(locale, { month: "short", day: "numeric" });
  if (period !== "90d") return formatter.format(start);
  const inclusiveEnd = new Date(end.getTime() - 1);
  return `${formatter.format(start)}–${formatter.format(inclusiveEnd)}`;
}

function shouldShowTick(index: number, count: number, period: DashboardPeriod): boolean {
  const step = period === "24h" ? 3 : period === "30d" ? 5 : 1;
  return index % step === 0 || index === count - 1;
}

function formatBucketTick(value: string, period: DashboardPeriod, locale: string): string {
  const options: Intl.DateTimeFormatOptions = period === "24h"
    ? { hour: "2-digit", minute: "2-digit" }
    : { month: "numeric", day: "numeric" };
  return new Intl.DateTimeFormat(locale, options).format(new Date(value));
}

function formatCompactNumber(value: number, locale: string): string {
  return new Intl.NumberFormat(locale, { notation: "compact", maximumFractionDigits: 1 }).format(value);
}

function formatUSD(ticks: number, locale: string): string {
  return formatUSDValue(ticks / USD_TICKS, locale);
}

function formatUSDValue(value: number, locale: string): string {
  return `$${new Intl.NumberFormat(locale, { minimumFractionDigits: 2, maximumFractionDigits: 2 }).format(value)}`;
}

function formatCompactUSD(value: number, locale: string): string {
  return `$${new Intl.NumberFormat(locale, { notation: "compact", maximumFractionDigits: 1 }).format(value)}`;
}
