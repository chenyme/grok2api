import type { TFunction } from "i18next";
import { Info } from "lucide-react";
import { useTranslation } from "react-i18next";

import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import type { AccountDTO, BillingDTO, QuotaDTO } from "@/features/accounts/accounts-api";
import { cn } from "@/shared/lib/cn";
import { formatDateTime, formatNumber } from "@/shared/lib/format";

export function AccountQuota({ quota, billing, locale }: { quota: QuotaDTO; billing?: BillingDTO; locale: string }) {
  const { t } = useTranslation();
  if (quota.type === "unknown") {
    return <span className="text-xs text-muted-foreground">{t("accountType.pending")}</span>;
  }
  if (quota.type !== "free") {
    return <BuildQuota quota={quota} billing={billing} locale={locale} />;
  }

  const percent = Math.min(100, Math.max(0, quota.usagePercent));
  const used = formatNumber(quota.used, locale, 0);
  const limit = formatNumber(quota.limit, locale, 0);
  const isEstimated = !quota.limitKnown;
  const recoveryDescription = quota.nextProbeAt
    ? t("accounts.waitingResetUntil", { time: formatDateTime(quota.nextProbeAt, locale) })
    : quota.status === "probing"
      ? t("accounts.probingQuota")
      : t("accounts.quotaResetUnknown");
  const usage = quota.limit > 0
    ? isEstimated ? t("accounts.freeEstimatedUsage", { used, limit }) : `${used} / ${limit} tokens`
    : t("accounts.freeObservedUsage", { used });

  return (
    <div className="w-full min-w-0 space-y-1.5">
      <div className="flex items-start justify-between gap-3 text-[11px] font-normal">
        <div className="inline-flex min-w-0 items-center gap-1 text-muted-foreground">
          <span>{usage}</span>
          {isEstimated ? (
            <Tooltip>
              <TooltipTrigger asChild>
                <button type="button" className="inline-flex shrink-0 text-muted-foreground transition-colors hover:text-foreground" aria-label={t("accounts.freeEstimatedDescription")}>
                  <Info className="size-3.5" />
                </button>
              </TooltipTrigger>
              <TooltipContent className="max-w-80 space-y-1">
                <div>{t("accounts.freeEstimatedDescription")}</div>
                <div className="text-muted-foreground">{recoveryDescription}</div>
              </TooltipContent>
            </Tooltip>
          ) : null}
        </div>
        <span className="shrink-0 text-muted-foreground">{isEstimated ? "≈" : ""}{formatNumber(quota.usagePercent, locale, 1)}%</span>
      </div>
      <div className="h-1.5 overflow-hidden rounded-full bg-muted"><div className="h-full bg-primary" style={{ width: `${percent}%` }} /></div>
    </div>
  );
}

function BuildQuota({ quota, billing, locale }: { quota: QuotaDTO; billing?: BillingDTO; locale: string }) {
  const { t } = useTranslation();
  const percentageQuota = quota.unit === "percent";
  const hasWeekly = percentageQuota || billing?.usagePeriodType === "USAGE_PERIOD_TYPE_WEEKLY";
  const hasMonthly = !percentageQuota && quota.limit > 0;
  if (!hasWeekly && !hasMonthly) return <span className="text-xs text-muted-foreground">{t("accounts.paidQuotaUsage")}</span>;

  const weeklyPercent = Math.max(0, Math.min(100, percentageQuota ? quota.usagePercent : (billing?.creditUsagePercent ?? 0)));
  const monthlyPercent = Math.max(0, Math.min(100, quota.usagePercent));
  const weeklyPeriodEnd = quota.periodEnd ?? billing?.usagePeriodEnd;
  return (
    <div className="w-full min-w-0 space-y-1.5">
      <div className={cn("grid w-full min-w-0 divide-x divide-border/70", hasWeekly && hasMonthly ? "grid-cols-2" : "grid-cols-1")}>
        {hasWeekly ? (
          <Tooltip>
            <TooltipTrigger asChild>
              <button type="button" className="min-w-0 px-2 text-left font-normal first:pl-0 last:pr-0">
                <div className="flex items-center justify-between gap-1 text-[11px]"><span className="truncate text-muted-foreground">{t("accounts.weeklyQuota")}</span><span className="shrink-0 tabular-nums">{formatNumber(weeklyPercent, locale, 1)}%</span></div>
                <div className="mt-1.5 h-1.5 overflow-hidden rounded-full bg-muted"><div className="h-full bg-primary" style={{ width: `${weeklyPercent}%` }} /></div>
              </button>
            </TooltipTrigger>
            <TooltipContent>
              <div>{t("accounts.weeklyLimit", { percent: formatNumber(100 - weeklyPercent, locale, 1) })}</div>
              <div className="text-muted-foreground">{weeklyPeriodEnd ? t("accounts.quotaResetAt", { time: formatDateTime(weeklyPeriodEnd, locale) }) : t("accounts.quotaResetUnknown")}</div>
            </TooltipContent>
          </Tooltip>
        ) : null}
        {hasMonthly ? (
          <Tooltip>
            <TooltipTrigger asChild>
              <button type="button" className="min-w-0 px-2 text-left font-normal first:pl-0 last:pr-0">
                <div className="flex items-center justify-between gap-1 text-[11px]"><span className="truncate text-muted-foreground">{t("accounts.monthlyQuota")}</span><span className="shrink-0 tabular-nums">{formatNumber(quota.used, locale, 2)}/{formatNumber(quota.limit, locale, 2)}</span></div>
                <div className="mt-1.5 h-1.5 overflow-hidden rounded-full bg-muted"><div className="h-full bg-primary" style={{ width: `${monthlyPercent}%` }} /></div>
              </button>
            </TooltipTrigger>
            <TooltipContent>
              <div>{t("accounts.paidQuotaDetails", { remaining: formatNumber(quota.remaining, locale, 2) })}</div>
              <div className="text-muted-foreground">{billing?.billingPeriodEnd ? t("accounts.quotaResetAt", { time: formatDateTime(billing.billingPeriodEnd, locale) }) : t("accounts.quotaResetUnknown")}</div>
            </TooltipContent>
          </Tooltip>
        ) : null}
      </div>
    </div>
  );
}

const visibleWebQuotaModes = ["auto", "fast", "expert", "heavy"] as const;

export function ConsoleQuota({ windows, locale }: { windows: NonNullable<AccountDTO["quotaWindows"]>; locale: string }) {
  const { t } = useTranslation();
  const window = windows.find((value) => value.mode === "console") ?? windows[0];
  if (!window) return <span className="text-xs text-muted-foreground">{t("accounts.quotaNotSynced")}</span>;
  return <WebQuotaMode mode="Console" window={window} locale={locale} />;
}

export function WebQuota({ windows, locale, tier }: { windows: NonNullable<AccountDTO["quotaWindows"]>; locale: string; tier?: AccountDTO["webTier"] }) {
  const { t } = useTranslation();
  if (windows.length === 0) return <span className="text-xs text-muted-foreground">{t("accounts.quotaNotSynced")}</span>;
  const windowsByMode = new Map(windows.map((window) => [window.mode, window]));
  const weekly = windowsByMode.get("weekly");
  if (weekly) return <WeeklyWebQuota window={weekly} locale={locale} t={t} />;

  const fast = windowsByMode.get("fast");
  if (tier === "basic" && fast) return <WebQuotaMode mode="Fast" window={fast} locale={locale} />;
  return (
    <div className="grid w-full min-w-0 grid-cols-4 divide-x divide-border/70">
      {visibleWebQuotaModes.map((mode) => {
        const window = windowsByMode.get(mode);
        if (!window) {
          return <div key={mode} className="min-w-0 px-2 first:pl-0 last:pr-0"><div className="flex items-center justify-between gap-1 text-[11px]"><span className="truncate capitalize text-muted-foreground">{mode}</span><span className="text-muted-foreground">-</span></div><div className="mt-1.5 h-1.5 rounded-full bg-muted" /></div>;
        }
        return <WebQuotaMode key={mode} mode={formatWebQuotaMode(mode)} window={window} locale={locale} compact />;
      })}
    </div>
  );
}

type WebQuotaWindow = NonNullable<AccountDTO["quotaWindows"]>[number];

function WeeklyWebQuota({ window, locale, t }: { window: WebQuotaWindow; locale: string; t: TFunction }) {
  const usedPercent = Math.max(0, Math.min(100, window.usagePercent));
  const breakdown = (window.breakdown ?? []).filter((item) => item.usagePercent > 0);
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button type="button" className="block w-full min-w-0 text-left">
          <div className="flex items-center justify-between gap-2 text-[11px]">
            {breakdown.length > 0 ? <div className="flex min-w-0 items-center gap-2.5 overflow-hidden text-muted-foreground">{breakdown.slice(0, 3).map((item) => <span key={item.productCode} className="flex shrink-0 items-center gap-1"><span className={cn("size-1.5 rounded-full", quotaProductColor(item.productCode))} /><span>{quotaProductLabel(item.productCode, t)}</span><span className="tabular-nums text-foreground">{formatNumber(item.usagePercent, locale, 1)}%</span></span>)}{breakdown.length > 3 ? <span className="shrink-0">+{breakdown.length - 3}</span> : null}</div> : <span className="truncate text-muted-foreground">{t("accounts.weeklyQuota")}</span>}
            <span className="shrink-0 tabular-nums">{formatNumber(usedPercent, locale, 1)}%</span>
          </div>
          <div className="mt-1.5 flex h-1.5 overflow-hidden rounded-full bg-muted">{breakdown.length > 0 ? breakdown.map((item) => <div key={item.productCode} className={cn("h-full shrink-0", quotaProductColor(item.productCode))} style={{ width: `${Math.max(0, Math.min(100, item.usagePercent))}%` }} />) : <div className="h-full bg-primary" style={{ width: `${usedPercent}%` }} />}</div>
        </button>
      </TooltipTrigger>
      <TooltipContent>
        <div>{t("accounts.webWeeklyQuotaUsage", { remaining: formatNumber(100 - usedPercent, locale, 1) })}</div>
        <div className="text-muted-foreground">{window.resetAt ? t("accounts.quotaResetAt", { time: formatDateTime(window.resetAt, locale) }) : t("accounts.quotaResetUnknown")}</div>
        {breakdown.length > 0 ? <div className="mt-2 grid gap-1 border-t pt-2">{breakdown.map((item) => <div key={item.productCode} className="flex items-center justify-between gap-4"><span className="flex items-center gap-1.5"><span className={cn("size-2 rounded-full", quotaProductColor(item.productCode))} />{quotaProductLabel(item.productCode, t)}</span><span className="tabular-nums">{formatNumber(item.usagePercent, locale, 1)}%</span></div>)}</div> : null}
      </TooltipContent>
    </Tooltip>
  );
}

function WebQuotaMode({ mode, window, locale, compact = false }: { mode: string; window: WebQuotaWindow; locale: string; compact?: boolean }) {
  const { t } = useTranslation();
  const used = Math.max(0, window.total - window.remaining);
  const percent = window.total > 0 ? Math.max(0, Math.min(100, used / window.total * 100)) : 0;
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button type="button" className={cn("block w-full min-w-0 text-left", compact && "px-2 first:pl-0 last:pr-0")}>
          <div className="flex items-center justify-between gap-1 text-[11px]"><span className="truncate text-muted-foreground">{mode}</span><span className="shrink-0 tabular-nums">{formatNumber(used, locale, 0)}/{formatNumber(window.total, locale, 0)}</span></div>
          <div className="mt-1.5 h-1.5 overflow-hidden rounded-full bg-muted"><div className="h-full bg-primary" style={{ width: `${percent}%` }} /></div>
        </button>
      </TooltipTrigger>
      <TooltipContent><div>{t("accounts.webModeQuotaRemaining", { mode, remaining: formatNumber(window.remaining, locale, 0) })}</div><div className="text-muted-foreground">{window.resetAt ? t("accounts.quotaResetAt", { time: formatDateTime(window.resetAt, locale) }) : t("accounts.quotaResetUnknown")}</div></TooltipContent>
    </Tooltip>
  );
}

function formatWebQuotaMode(mode: string): string {
  return mode ? mode.charAt(0).toUpperCase() + mode.slice(1) : mode;
}

function quotaProductLabel(code: number, t: TFunction): string {
  const keys: Record<number, string> = { 0: "thirdParty", 1: "api", 2: "build", 3: "plugins", 4: "chat", 5: "imagine", 6: "voice" };
  const key = keys[code];
  return key ? t(`quotaProducts.${key}`) : t("quotaProducts.unknown", { code });
}

function quotaProductColor(code: number): string {
  const colors: Record<number, string> = { 0: "bg-quota-product-0", 1: "bg-quota-product-1", 2: "bg-quota-product-2", 3: "bg-quota-product-3", 4: "bg-quota-product-4", 5: "bg-quota-product-5", 6: "bg-quota-product-6" };
  return colors[code] ?? "bg-muted-foreground";
}
