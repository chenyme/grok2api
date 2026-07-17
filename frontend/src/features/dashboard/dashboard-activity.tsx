import { useMemo } from "react";
import { useTranslation } from "react-i18next";

import { Spinner } from "@/components/ui/spinner";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import type { DashboardDTO } from "@/features/dashboard/dashboard-api";
import { DashboardPanel } from "@/features/dashboard/dashboard-panel";
import { cn } from "@/shared/lib/cn";
import { formatNumber } from "@/shared/lib/format";

type DashboardActivityProps = {
  dashboard?: DashboardDTO;
  locale: string;
  loading: boolean;
};

const INTENSITY_CLASSES = [
  "bg-emerald-500/10",
  "bg-emerald-500/25",
  "bg-emerald-500/45",
  "bg-emerald-500/70",
  "bg-emerald-500",
] as const;

export function DashboardActivity({ dashboard, locale, loading }: DashboardActivityProps) {
  const { t } = useTranslation();
  const activity = useMemo(() => dashboard?.activity ?? [], [dashboard?.activity]);
  const activityWeeks = useMemo(() => Array.from({ length: Math.ceil(activity.length / 7) }, (_, index) => activity.slice(index * 7, index * 7 + 7)), [activity]);
  const maxRequests = Math.max(0, ...activity.map((point) => point.requests));
  const totalRequests = activity.reduce((total, point) => total + point.requests, 0);
  const generatedAt = dashboard?.generatedAt ? new Date(dashboard.generatedAt).getTime() : Number.POSITIVE_INFINITY;
  const rangeLabel = useMemo(() => formatActivityRange(activity, locale, generatedAt), [activity, generatedAt, locale]);

  return (
    <DashboardPanel
      id="dashboard-activity-title"
      title={t("dashboard.activityTitle")}
      actions={<span className="text-[11px] text-muted-foreground">{t("dashboard.lastDays", { count: 180 })}</span>}
      className="min-h-[210px]"
    >
      {loading ? (
        <div className="flex min-h-32 items-center justify-center"><Spinner className="size-5" /></div>
      ) : (
        <div>
          <div className="flex items-baseline justify-between gap-3">
            <p className="text-xl font-medium tabular-nums">{formatNumber(totalRequests, locale)}</p>
            <p className="text-[11px] text-muted-foreground">{rangeLabel}</p>
          </div>

          <div className="mt-4 w-full pb-1">
            <div className="flex w-full gap-1" aria-label={t("dashboard.activityTitle")}>
              {activityWeeks.map((week, weekIndex) => (
                <div key={weekIndex} className="grid min-w-0 flex-1 grid-rows-7 gap-1">
                  {week.map((point) => {
                    const future = new Date(point.start).getTime() > generatedAt;
                    return (
                      <Tooltip key={point.start}>
                        <TooltipTrigger asChild>
                          <span
                            className={cn(
                              "aspect-square w-full rounded-[3px]",
                              future ? "bg-muted/40" : INTENSITY_CLASSES[activityLevel(point.requests, maxRequests)],
                            )}
                            aria-hidden="true"
                          />
                        </TooltipTrigger>
                        <TooltipContent>{t("dashboard.activityDay", { date: formatActivityDate(point.start, locale), requests: formatNumber(point.requests, locale) })}</TooltipContent>
                      </Tooltip>
                    );
                  })}
                </div>
              ))}
            </div>
          </div>

          <div className="mt-3 flex items-center justify-end gap-1.5 text-[10px] text-muted-foreground">
            <span>{t("dashboard.activityLess")}</span>
            {INTENSITY_CLASSES.map((className) => <span key={className} className={cn("size-2.5 rounded-[2px]", className)} />)}
            <span>{t("dashboard.activityMore")}</span>
          </div>
        </div>
      )}
    </DashboardPanel>
  );
}

function activityLevel(value: number, maximum: number): number {
  if (value <= 0 || maximum <= 0) return 0;
  const ratio = Math.log1p(value) / Math.log1p(maximum);
  if (ratio <= 0.25) return 1;
  if (ratio <= 0.5) return 2;
  if (ratio <= 0.75) return 3;
  return 4;
}

function formatActivityDate(value: string, locale: string): string {
  return new Intl.DateTimeFormat(locale, { month: "short", day: "numeric" }).format(new Date(value));
}

function formatActivityRange(activity: DashboardDTO["activity"], locale: string, generatedAt: number): string {
  if (activity.length === 0) return "-";
  let lastVisible = activity[0];
  for (let index = activity.length - 1; index >= 0; index -= 1) {
    if (new Date(activity[index].start).getTime() <= generatedAt) {
      lastVisible = activity[index];
      break;
    }
  }
  return `${formatActivityDate(activity[0].start, locale)} – ${formatActivityDate(lastVisible.start, locale)}`;
}
