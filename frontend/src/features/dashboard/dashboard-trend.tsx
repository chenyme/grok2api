import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { Area, Bar, CartesianGrid, ComposedChart, Line, XAxis, YAxis } from "recharts";

import { ChartContainer, ChartLegend, ChartTooltip, ChartTooltipContent, type ChartConfig } from "@/components/ui/chart";
import { Spinner } from "@/components/ui/spinner";
import type { DashboardDTO, DashboardPeriod } from "@/features/dashboard/dashboard-api";
import { formatCompactNumber, formatCompactUSD, formatUSDValue, usdTicksToValue } from "@/features/dashboard/dashboard-format";
import { DashboardPanel } from "@/features/dashboard/dashboard-panel";
import { EmptyState } from "@/shared/components/data-state";
import { cn } from "@/shared/lib/cn";
import { formatNumber } from "@/shared/lib/format";

type DashboardTrendProps = {
  dashboard?: DashboardDTO;
  locale: string;
  loading: boolean;
};

type TrendSeries = "billing" | "tokens" | "requests";
type AxisSide = "left" | "right";

const TREND_SERIES: TrendSeries[] = ["billing", "tokens", "requests"];

export function DashboardTrend({ dashboard, locale, loading }: DashboardTrendProps) {
  const { t } = useTranslation();
  const [hiddenSeries, setHiddenSeries] = useState<Set<TrendSeries>>(() => new Set());
  const chartData = useMemo(() => dashboard?.series.map((bucket) => ({
    requests: bucket.requests,
    tokens: bucket.tokens,
    billing: usdTicksToValue(bucket.billedCostUsdTicks),
    start: bucket.start,
    tooltipLabel: formatBucketRange(bucket.start, bucket.end, dashboard.period, locale),
  })) ?? [], [dashboard, locale]);
  const xTicks = useMemo(() => chartData
    .filter((_point, index) => shouldShowTick(index, chartData.length, dashboard?.period ?? "24h"))
    .map((point) => point.start), [chartData, dashboard?.period]);
  const chartConfig = useMemo<ChartConfig>(() => ({
    tokens: {
      label: t("dashboard.trendTokens"),
      theme: { light: "oklch(0.68 0.15 245)", dark: "oklch(0.74 0.13 245)" },
    },
    billing: {
      label: t("dashboard.billing"),
      theme: { light: "oklch(0.7 0.11 160)", dark: "oklch(0.73 0.1 160)" },
    },
    requests: {
      label: t("dashboard.trendRequests"),
      theme: { light: "oklch(0.76 0.06 245)", dark: "oklch(0.56 0.07 245)" },
    },
  }), [t]);
  const hasData = dashboard?.series.some((bucket) => bucket.requests > 0 || bucket.tokens > 0 || bucket.billedCostUsdTicks > 0) ?? false;
  const axisSides = resolveTrendAxes(hiddenSeries);

  function toggleSeries(series: TrendSeries): void {
    setHiddenSeries((current) => {
      const next = new Set(current);
      if (next.has(series)) next.delete(series);
      else next.add(series);
      return next;
    });
  }

  return (
    <DashboardPanel id="dashboard-trend-title" title={t("dashboard.trend")} className="h-full min-h-[360px]">
      {!loading && !hasData ? (
        <div className="flex h-[280px] items-center justify-center">
          <EmptyState message={t("dashboard.noTrendData")} />
        </div>
      ) : (
        <div className="relative" aria-busy={loading}>
          <ChartContainer config={chartConfig} className={cn("h-[280px] w-full aspect-auto", loading && "opacity-40")}>
            <ComposedChart accessibilityLayer data={chartData} margin={{ left: 0, right: 4, top: 10, bottom: 0 }}>
              <defs>
                <linearGradient id="dashboard-tokens-fill" x1="0" y1="0" x2="0" y2="1">
                  <stop offset="5%" stopColor="var(--color-tokens)" stopOpacity={0.2} />
                  <stop offset="95%" stopColor="var(--color-tokens)" stopOpacity={0.01} />
                </linearGradient>
              </defs>
              <CartesianGrid vertical={false} strokeDasharray="3 3" />
              <XAxis
                dataKey="start"
                ticks={xTicks}
                interval={0}
                tickLine={false}
                axisLine={false}
                tickMargin={10}
                minTickGap={12}
                tickFormatter={(value) => formatBucketTick(String(value), dashboard?.period ?? "24h", locale)}
              />
              <YAxis
                yAxisId="tokens"
                hide={!axisSides.tokens}
                orientation={axisSides.tokens ?? "left"}
                tickLine={false}
                axisLine={false}
                tickMargin={8}
                width={axisSides.tokens ? 48 : 0}
                allowDecimals={false}
                tickFormatter={(value) => formatCompactNumber(Number(value), locale)}
              />
              <YAxis
                yAxisId="billing"
                hide={!axisSides.billing}
                orientation={axisSides.billing ?? "right"}
                tickLine={false}
                axisLine={false}
                tickMargin={8}
                width={axisSides.billing ? 48 : 0}
                allowDecimals
                tickFormatter={(value) => formatCompactUSD(Number(value), locale)}
              />
              <YAxis
                yAxisId="requests"
                hide={!axisSides.requests}
                orientation={axisSides.requests ?? "right"}
                tickLine={false}
                axisLine={false}
                tickMargin={8}
                width={axisSides.requests ? 48 : 0}
                allowDecimals={false}
                tickFormatter={(value) => formatCompactNumber(Number(value), locale)}
              />
              <ChartTooltip
                cursor={false}
                content={(
                  <ChartTooltipContent
                    className="w-64 max-w-[calc(100vw-2rem)]"
                    indicator="dot"
                    labelFormatter={(_label, payload) => payload?.[0]?.payload?.tooltipLabel ?? ""}
                    formatter={(value, name, item) => (
                      <div className="flex w-full items-center justify-between gap-4">
                        <span className="flex min-w-0 items-center gap-2 text-xs font-normal text-muted-foreground">
                          <span className="size-2 shrink-0 rounded-full" style={{ backgroundColor: item.color || `var(--color-${String(name)})` }} />
                          <span className="truncate">{chartConfig[String(name)]?.label ?? name}</span>
                        </span>
                        <span className="shrink-0 font-mono text-xs font-normal tabular-nums text-muted-foreground">
                          {name === "billing" ? formatUSDValue(Number(value), locale) : formatNumber(Number(value), locale)}
                        </span>
                      </div>
                    )}
                  />
                )}
              />
              <Bar
                yAxisId="billing"
                dataKey="billing"
                fill="var(--color-billing)"
                fillOpacity={0.42}
                hide={hiddenSeries.has("billing")}
                maxBarSize={32}
                radius={[3, 3, 0, 0]}
                animationDuration={700}
                animationEasing="ease-out"
              />
              <Area
                key={`tokens-${dashboard?.period ?? "loading"}`}
                yAxisId="tokens"
                dataKey="tokens"
                type="monotone"
                stroke="var(--color-tokens)"
                strokeWidth={1.5}
                fill="url(#dashboard-tokens-fill)"
                hide={hiddenSeries.has("tokens")}
                dot={false}
                activeDot={{ r: 3, fill: "var(--color-tokens)", stroke: "var(--color-background)", strokeWidth: 2 }}
                animationDuration={700}
                animationEasing="ease-out"
              />
              <Line
                key={`requests-${dashboard?.period ?? "loading"}`}
                yAxisId="requests"
                dataKey="requests"
                type="monotone"
                stroke="var(--color-requests)"
                strokeWidth={1.25}
                strokeDasharray="5 4"
                hide={hiddenSeries.has("requests")}
                dot={false}
                activeDot={{ r: 3, fill: "var(--color-requests)", stroke: "var(--color-background)", strokeWidth: 2 }}
                animationDuration={700}
                animationEasing="ease-out"
              />
              <ChartLegend content={<DashboardTrendLegend config={chartConfig} hiddenSeries={hiddenSeries} onToggle={toggleSeries} />} />
            </ComposedChart>
          </ChartContainer>
          {loading ? <div className="absolute inset-0 flex items-center justify-center"><Spinner className="size-5" /></div> : null}
        </div>
      )}
    </DashboardPanel>
  );
}

function DashboardTrendLegend({ config, hiddenSeries, onToggle }: { config: ChartConfig; hiddenSeries: Set<TrendSeries>; onToggle: (series: TrendSeries) => void }) {
  const { t } = useTranslation();
  return (
    <div className="flex flex-wrap items-center justify-center gap-x-4 gap-y-2 pt-3 text-xs text-muted-foreground">
      {TREND_SERIES.map((series) => {
        const hidden = hiddenSeries.has(series);
        const label = config[series]?.label ?? series;
        return (
          <button
            key={series}
            type="button"
            className={cn("flex items-center gap-1.5 rounded-md px-2 py-1 transition-[background-color,color,opacity] hover:bg-accent hover:opacity-100", hidden && "opacity-35")}
            onClick={() => onToggle(series)}
            aria-pressed={!hidden}
            aria-label={`${t(hidden ? "common.enable" : "common.disable")} ${String(label)}`}
          >
            <SeriesLegendMark series={series} />
            <span>{label}</span>
          </button>
        );
      })}
    </div>
  );
}

function SeriesLegendMark({ series }: { series: TrendSeries }) {
  if (series === "billing") {
    return <span className="size-2 shrink-0 rounded-[2px]" style={{ backgroundColor: "var(--color-billing)" }} />;
  }
  return (
    <span
      className={cn("w-3 shrink-0 border-t", series === "requests" && "border-dashed")}
      style={{ borderColor: `var(--color-${series})` }}
    />
  );
}

function resolveTrendAxes(hiddenSeries: ReadonlySet<TrendSeries>): Partial<Record<TrendSeries, AxisSide>> {
  const visible = TREND_SERIES.filter((series) => !hiddenSeries.has(series));
  if (visible.length === 3) return { tokens: "left", billing: "right" };
  if (visible.length === 2) {
    if (visible.includes("tokens")) {
      return { tokens: "left", [visible.includes("billing") ? "billing" : "requests"]: "right" };
    }
    return { requests: "left", billing: "right" };
  }
  return visible.length === 1 ? { [visible[0]]: "left" } : {};
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
