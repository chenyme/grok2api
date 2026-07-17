import { useTranslation } from "react-i18next";

import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import type { DashboardDTO } from "@/features/dashboard/dashboard-api";
import { formatUSD } from "@/features/dashboard/dashboard-format";
import { DashboardPanel } from "@/features/dashboard/dashboard-panel";
import { EmptyState, TableLoadingRow } from "@/shared/components/data-state";
import { cn } from "@/shared/lib/cn";
import { formatNumber } from "@/shared/lib/format";

type DashboardTopModelsProps = {
  dashboard?: DashboardDTO;
  locale: string;
  loading: boolean;
};

const COLUMN_COUNT = 4;

export function DashboardTopModels({ dashboard, locale, loading }: DashboardTopModelsProps) {
  const { t } = useTranslation();
  const models = dashboard?.topModels ?? [];

  return (
    <DashboardPanel
      id="dashboard-top-models-title"
      title={t("dashboard.topModels")}
      className="h-full"
    >
      <Table className="min-w-[560px] table-fixed [&_tbody_tr]:border-border/60">
        <TableHeader className="[&_tr]:border-border/70">
          <TableRow className="hover:bg-transparent">
            <TableHead>{t("dashboard.model")}</TableHead>
            <TableHead className="w-28 whitespace-nowrap">
              <span className="flex items-center justify-end gap-1.5">
                <span className="size-1.5 rounded-full bg-emerald-500" />
                {t("dashboard.billing")}
              </span>
            </TableHead>
            <TableHead className="w-28">
              <span className="flex items-center justify-end gap-1.5">
                <span className="size-1.5 rounded-full bg-violet-500" />
                {t("dashboard.trendTokens")}
              </span>
            </TableHead>
            <TableHead className="w-28">
              <span className="flex items-center justify-end gap-1.5">
                <span className="size-1.5 rounded-full bg-sky-500" />
                {t("dashboard.trendRequests")}
              </span>
            </TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {loading ? <TableLoadingRow colSpan={COLUMN_COUNT} /> : models.length === 0 ? (
            <TableRow className="hover:bg-transparent">
              <TableCell colSpan={COLUMN_COUNT} className="p-0">
                <EmptyState message={t("dashboard.noTopModels")} />
              </TableCell>
            </TableRow>
          ) : models.map((item) => {
            const inactive = item.requests === 0;
            const tokenDetails = [
              [t("dashboard.inputTokens"), item.inputTokens],
              [t("dashboard.outputTokens"), item.outputTokens],
              ...(item.cachedInputTokens > 0 ? [[t("dashboard.cachedTokens"), item.cachedInputTokens]] : []),
              ...(item.reasoningTokens > 0 ? [[t("dashboard.reasoningTokens"), item.reasoningTokens]] : []),
            ];
            return (
              <TableRow key={item.model} className="h-14">
                <TableCell>
                  <div className="min-w-0">
                    <span className={cn("block truncate text-xs font-medium", inactive && "font-normal text-muted-foreground")} title={item.model}>{item.model}</span>
                    <p className="mt-1 truncate text-[10px] text-muted-foreground/80">
                      {tokenDetails.map(([label, value]) => `${label} ${formatNumber(Number(value), locale)}`).join(" · ")}
                    </p>
                  </div>
                </TableCell>
                <TableCell className={cn("whitespace-nowrap text-right text-xs font-medium tabular-nums text-emerald-600 dark:text-emerald-400", item.billedCostUsdTicks === 0 && "font-normal text-muted-foreground")}>{formatUSD(item.billedCostUsdTicks, locale)}</TableCell>
                <TableCell
                  className={cn("text-right text-xs font-medium tabular-nums text-violet-600 dark:text-violet-400", item.tokens === 0 && "font-normal text-muted-foreground")}
                  title={formatNumber(item.tokens, locale)}
                >
                  {formatCompactTokens(item.tokens, locale)}
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  <span className={cn("text-xs font-medium text-sky-600 dark:text-sky-400", inactive && "font-normal text-muted-foreground")}>{formatNumber(item.requests, locale)}</span>
                </TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </DashboardPanel>
  );
}

function formatCompactTokens(value: number, locale: string): string {
  const absolute = Math.abs(value);
  if (absolute < 10_000) return formatNumber(value, locale);
  const units = [
    { threshold: 1_000_000_000, suffix: "B" },
    { threshold: 1_000_000, suffix: "M" },
    { threshold: 1_000, suffix: "K" },
  ];
  const unit = units.find((candidate) => absolute >= candidate.threshold);
  if (!unit) return formatNumber(value, locale);
  const compact = value / unit.threshold;
  const precision = Math.abs(compact) < 10 && !Number.isInteger(compact) ? 1 : 0;
  return `${formatNumber(compact, locale, precision)}${unit.suffix}`;
}
