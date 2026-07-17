import { useQuery } from "@tanstack/react-query";
import { RefreshCw } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { DashboardActivity } from "@/features/dashboard/dashboard-activity";
import { getDashboard, type DashboardPeriod } from "@/features/dashboard/dashboard-api";
import { DashboardOverview, DashboardResources } from "@/features/dashboard/dashboard-overview";
import { DashboardProviderDistribution } from "@/features/dashboard/dashboard-provider-distribution";
import { DashboardTopModels } from "@/features/dashboard/dashboard-top-models";
import { DashboardTrend } from "@/features/dashboard/dashboard-trend";
import { VersionUpdateBanner } from "@/features/system/version-update";
import { ErrorState } from "@/shared/components/data-state";
import { PeriodSelector } from "@/shared/components/period-selector";
import { formatDateTime } from "@/shared/lib/format";
import { PERIOD_DAYS, toPeriodValue, type PeriodDays } from "@/shared/lib/period";

type DashboardPreferences = { periodDays: PeriodDays };

const DASHBOARD_PREFERENCES_KEY = "grok2api:dashboard-preferences";
const DEFAULT_DASHBOARD_PREFERENCES: DashboardPreferences = { periodDays: 30 };

export function DashboardPage() {
  const { t, i18n } = useTranslation();
  const [preferences, setPreferences] = useState<DashboardPreferences>(readDashboardPreferences);
  const [manualRefreshing, setManualRefreshing] = useState(false);
  const forceRefresh = useRef(false);
  const { periodDays } = preferences;
  const period: DashboardPeriod = toPeriodValue(periodDays);
  const timezone = Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";

  useEffect(() => {
    saveDashboardPreferences(preferences);
  }, [preferences]);

  const dashboardQuery = useQuery({
    queryKey: ["dashboard", period, timezone],
    queryFn: () => getDashboard(period, timezone, forceRefresh.current),
    placeholderData: (previous) => previous,
    staleTime: 15_000,
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

  if (dashboardQuery.isError && !dashboardQuery.data) {
    return <ErrorState message={dashboardQuery.error.message} onRetry={refreshAll} />;
  }

  const dashboard = dashboardQuery.data;
  const loading = dashboardQuery.isPending || dashboardQuery.isPlaceholderData;
  const refreshing = dashboardQuery.isFetching || manualRefreshing;

  return (
    <div className="space-y-5">
      <div className="space-y-5">
        <header className="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
          <h1 className="text-xl font-medium">{t("dashboard.title")}</h1>
          <div className="flex min-w-0 shrink-0 items-center gap-2">
            {dashboard?.generatedAt ? <span className="mr-1 hidden text-[11px] text-muted-foreground lg:inline">{t("dashboard.lastUpdated", { time: formatDateTime(dashboard.generatedAt, i18n.language) })}</span> : null}
            <Button
              type="button"
              variant="ghost"
              size="icon"
              className="size-8 text-muted-foreground"
              onClick={refreshAll}
              disabled={refreshing}
              aria-label={t("common.refresh")}
            >
              <RefreshCw className={refreshing ? "animate-spin" : undefined} />
            </Button>
            <PeriodSelector
              value={periodDays}
              onChange={(value) => setPreferences((current) => ({ ...current, periodDays: value }))}
              ariaLabel={t("dashboard.usage")}
            />
          </div>
        </header>

        <VersionUpdateBanner />
      </div>

      <DashboardOverview dashboard={dashboard} locale={i18n.language} loading={loading} />

      <div className="grid items-stretch gap-2 xl:grid-cols-[minmax(0,3fr)_minmax(360px,2fr)]">
        <DashboardTrend
          dashboard={dashboard}
          locale={i18n.language}
          loading={loading}
        />
        <DashboardProviderDistribution dashboard={dashboard} locale={i18n.language} loading={loading} />
      </div>

      <div className="grid items-stretch gap-2 xl:grid-cols-[minmax(0,3fr)_minmax(360px,2fr)]">
        <DashboardTopModels dashboard={dashboard} locale={i18n.language} loading={loading} />
        <div className="grid min-h-0 grid-rows-[auto_minmax(0,1fr)] gap-2 xl:h-full">
          <DashboardActivity dashboard={dashboard} locale={i18n.language} loading={loading} />
          <DashboardResources dashboard={dashboard} locale={i18n.language} loading={loading} />
        </div>
      </div>
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
    if (periodDays === undefined) return DEFAULT_DASHBOARD_PREFERENCES;
    return { periodDays };
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
