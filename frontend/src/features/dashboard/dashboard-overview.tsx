import { Activity, CircleDollarSign, UsersRound, WholeWord, type LucideIcon } from "lucide-react";
import { useTranslation } from "react-i18next";
import { Pie, PieChart } from "recharts";

import { ChartContainer, type ChartConfig } from "@/components/ui/chart";
import { Spinner } from "@/components/ui/spinner";
import type { DashboardDTO } from "@/features/dashboard/dashboard-api";
import { formatUSD, formatUSDValue, usdTicksToValue } from "@/features/dashboard/dashboard-format";
import { DashboardPanel } from "@/features/dashboard/dashboard-panel";
import { cn } from "@/shared/lib/cn";
import { formatNumber } from "@/shared/lib/format";

type DashboardDataProps = {
  dashboard?: DashboardDTO;
  locale: string;
  loading: boolean;
};

export function DashboardOverview({ dashboard, locale, loading }: DashboardDataProps) {
  const { t } = useTranslation();
  const resources = dashboard?.resources;
  const usage = dashboard?.usage;
  const cacheHitRate = (usage?.inputTokens ?? 0) > 0 ? (usage?.cachedInputTokens ?? 0) / (usage?.inputTokens ?? 1) * 100 : 0;
  const averageRequestCost = (usage?.requests ?? 0) > 0 ? usdTicksToValue(usage?.billedCostUsdTicks ?? 0) / (usage?.requests ?? 1) : 0;

  return (
    <section aria-label={t("dashboard.usage")}>
      <div className="grid gap-2 sm:grid-cols-2 xl:grid-cols-4">
        <DashboardMetric
          icon={UsersRound}
          label={t("dashboard.accountCount")}
          value={formatNumber(resources?.totalAccounts ?? 0, locale)}
          detail={t("dashboard.accountDistribution", { build: formatNumber(resources?.buildAccounts ?? 0, locale), web: formatNumber(resources?.webAccounts ?? 0, locale), console: formatNumber(resources?.consoleAccounts ?? 0, locale) })}
          loading={loading}
        />
        <DashboardMetric
          icon={Activity}
          label={t("dashboard.requests")}
          value={formatNumber(usage?.requests ?? 0, locale)}
          detail={t("dashboard.requestSuccessRate", { rate: formatNumber(usage?.successRate ?? 0, locale, 1) })}
          loading={loading}
        />
        <DashboardMetric
          icon={WholeWord}
          label={t("dashboard.tokens")}
          value={formatNumber(usage?.tokens ?? 0, locale)}
          detail={t("dashboard.tokenEfficiency", { rate: formatNumber(cacheHitRate, locale, 1) })}
          loading={loading}
        />
        <DashboardMetric
          icon={CircleDollarSign}
          label={t("dashboard.billing")}
          value={formatUSD(usage?.billedCostUsdTicks ?? 0, locale)}
          detail={t("dashboard.averageRequestCost", { cost: formatUSDValue(averageRequestCost, locale) })}
          loading={loading}
        />
      </div>
    </section>
  );
}

export function DashboardResources({ dashboard, locale, loading }: DashboardDataProps) {
  const { t } = useTranslation();
  const resources = dashboard?.resources;
  const activeAccounts = resources?.activeAccounts ?? 0;
  const totalAccounts = resources?.totalAccounts ?? 0;
  const unavailableAccounts = Math.max(0, totalAccounts - activeAccounts);
  const availability = totalAccounts > 0 ? activeAccounts / totalAccounts * 100 : 0;
  const chartConfig = {
    active: {
      label: t("dashboard.activeAccounts"),
      theme: { light: "oklch(0.68 0.14 160)", dark: "oklch(0.74 0.12 160)" },
    },
    unavailable: {
      label: t("dashboard.unavailableAccounts"),
      theme: { light: "oklch(0.88 0.01 250)", dark: "oklch(0.36 0.01 250)" },
    },
  } satisfies ChartConfig;
  const chartData = totalAccounts > 0 ? [
    { status: "active", value: activeAccounts, fill: "var(--color-active)" },
    { status: "unavailable", value: unavailableAccounts, fill: "var(--color-unavailable)" },
  ] : [
    { status: "unavailable", value: 1, fill: "var(--color-unavailable)" },
  ];

  return (
    <DashboardPanel
      id="dashboard-resources-title"
      title={t("dashboard.resourcesTitle")}
      className="flex h-full min-h-[360px] flex-col"
      contentClassName="flex flex-1"
    >
      {loading ? (
        <div className="flex min-h-[260px] items-center justify-center"><Spinner className="size-5" /></div>
      ) : (
        <div className="grid min-h-[260px] w-full flex-1 grid-cols-[minmax(0,1fr)_minmax(128px,0.8fr)] items-center gap-4">
          <div className="relative mx-auto size-44 max-w-full">
            <ChartContainer config={chartConfig} className="size-full aspect-square" aria-label={t("dashboard.resourcesTitle")}>
              <PieChart>
                <Pie
                  data={chartData}
                  dataKey="value"
                  nameKey="status"
                  innerRadius={56}
                  outerRadius={78}
                  paddingAngle={activeAccounts > 0 && unavailableAccounts > 0 ? 3 : 0}
                  strokeWidth={0}
                  animationDuration={700}
                  animationEasing="ease-out"
                />
              </PieChart>
            </ChartContainer>
            <div className="pointer-events-none absolute inset-0 flex flex-col items-center justify-center">
              <span className="text-2xl font-medium tabular-nums">{formatNumber(availability, locale, 0)}%</span>
              <span className="mt-1 text-[10px] text-muted-foreground">{t("dashboard.availability")}</span>
            </div>
          </div>

          <div className="min-w-0 divide-y divide-border/60">
            <ResourceSummary
              color="bg-emerald-500"
              label={t("dashboard.activeAccounts")}
              value={formatNumber(activeAccounts, locale)}
              detail={t("dashboard.availableSummary", { active: formatNumber(activeAccounts, locale), total: formatNumber(totalAccounts, locale) })}
            />
            <ResourceSummary
              color="bg-muted-foreground/35"
              label={t("dashboard.unavailableAccounts")}
              value={formatNumber(unavailableAccounts, locale)}
              detail={t("dashboard.unavailableSummary", { unavailable: formatNumber(unavailableAccounts, locale), total: formatNumber(totalAccounts, locale) })}
            />
            <ResourceSummary
              color="bg-violet-500"
              label={t("dashboard.enabledModels")}
              value={formatNumber(resources?.enabledModels ?? 0, locale)}
              detail={t("dashboard.modelsAvailableSummary", { enabled: formatNumber(resources?.enabledModels ?? 0, locale), total: formatNumber(resources?.totalModels ?? 0, locale) })}
            />
            <ResourceSummary
              color="bg-sky-500"
              label={t("dashboard.activeClientKeys")}
              value={formatNumber(resources?.activeClientKeys ?? 0, locale)}
              detail={t("dashboard.keysAvailableSummary", { active: formatNumber(resources?.activeClientKeys ?? 0, locale), total: formatNumber(resources?.totalClientKeys ?? 0, locale) })}
            />
          </div>
        </div>
      )}
    </DashboardPanel>
  );
}

function DashboardMetric({ icon: Icon, label, value, detail, loading }: { icon: LucideIcon; label: string; value: string; detail: string; loading: boolean }) {
  return (
    <article className="min-h-28 rounded-lg bg-card p-4" aria-busy={loading}>
      <header className="flex min-h-5 items-center justify-between gap-3">
        <span className="text-xs text-muted-foreground">{label}</span>
        <Icon className="size-4 shrink-0 text-muted-foreground" />
      </header>
      <div className="mt-3 flex min-h-8 items-center text-2xl font-medium tracking-tight tabular-nums">{loading ? <Spinner /> : value}</div>
      <p className={cn("mt-1.5 min-h-4 truncate text-[11px] text-muted-foreground", loading && "invisible")} title={detail}>{detail}</p>
    </article>
  );
}

function ResourceSummary({ color, label, value, detail }: { color: string; label: string; value: string; detail: string }) {
  return (
    <div className="flex items-center justify-between gap-3 py-3 first:pt-0 last:pb-0">
      <div className="flex min-w-0 items-center gap-2">
        <span className={cn("size-2 shrink-0 rounded-full", color)} />
        <div className="min-w-0">
          <p className="truncate text-xs">{label}</p>
          <p className="mt-0.5 truncate text-[10px] text-muted-foreground" title={detail}>{detail}</p>
        </div>
      </div>
      <span className="shrink-0 text-sm font-medium tabular-nums">{value}</span>
    </div>
  );
}
