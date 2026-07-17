import { useMemo, useState } from "react";
import type { TFunction } from "i18next";
import { createPortal } from "react-dom";
import { useTranslation } from "react-i18next";

import { Spinner } from "@/components/ui/spinner";
import type { DashboardDTO } from "@/features/dashboard/dashboard-api";
import { DashboardPanel } from "@/features/dashboard/dashboard-panel";
import { cn } from "@/shared/lib/cn";
import { formatNumber } from "@/shared/lib/format";

type DashboardProviderDistributionProps = {
  dashboard?: DashboardDTO;
  locale: string;
  loading: boolean;
};

type ProviderKey = "grok_build" | "grok_web" | "grok_console";

const STRIPE_COUNT = 40;
const PROVIDERS: Array<{ key: ProviderKey; color: string; dot: string }> = [
  { key: "grok_build", color: "bg-quota-product-1", dot: "bg-quota-product-1" },
  { key: "grok_web", color: "bg-quota-product-2", dot: "bg-quota-product-2" },
  { key: "grok_console", color: "bg-quota-product-4", dot: "bg-quota-product-4" },
];

export function DashboardProviderDistribution({ dashboard, locale, loading }: DashboardProviderDistributionProps) {
  const { t } = useTranslation();
  const [stripeHover, setStripeHover] = useState<{ index: number; x: number; y: number } | null>(null);
  const providers = useMemo(() => PROVIDERS.map((provider) => {
    const usage = dashboard?.providers.find((item) => item.provider === provider.key);
    return { ...provider, requests: usage?.requests ?? 0, successfulRequests: usage?.successfulRequests ?? 0, tokens: usage?.tokens ?? 0 };
  }), [dashboard?.providers]);
  const totalRequests = providers.reduce((total, item) => total + item.requests, 0);
  const totalSuccessfulRequests = providers.reduce((total, item) => total + item.successfulRequests, 0);
  const averageSuccessRate = totalRequests > 0 ? totalSuccessfulRequests / totalRequests * 100 : 0;
  const stripes = useMemo(() => buildProviderStripes(providers, totalRequests), [providers, totalRequests]);
  const hoveredProvider = stripeHover === null ? null : stripes[stripeHover.index] ?? null;
  const hoveredShare = hoveredProvider && totalRequests > 0 ? hoveredProvider.requests / totalRequests * 100 : 0;
  const stripeTooltip = hoveredProvider
    ? t("dashboard.providerStripeDetail", { provider: providerLabel(hoveredProvider.key, t), requests: formatNumber(hoveredProvider.requests, locale), share: formatNumber(hoveredShare, locale, 1) })
    : t("dashboard.providerNoRequests");
  const successRateSummary = loading ? <Spinner className="size-3.5" /> : <span className="text-base font-medium tabular-nums">{formatNumber(averageSuccessRate, locale, 1)}%</span>;

  return (
    <DashboardPanel
      id="dashboard-provider-distribution-title"
      title={t("dashboard.providerDistribution")}
      actions={<span className="flex min-h-5 items-center gap-1.5">{successRateSummary}<span className="text-[11px] text-muted-foreground">{t("dashboard.successRate")}</span></span>}
      className="flex h-full min-h-[360px] flex-col"
      contentClassName="flex flex-1 flex-col"
    >
      {loading ? (
        <div className="flex min-h-[260px] items-center justify-center"><Spinner className="size-5" /></div>
      ) : (
        <div className="flex flex-1 flex-col">
          <div className="relative">
            <div
              className="flex h-12 cursor-default items-stretch gap-1"
              aria-label={t("dashboard.providerDistribution")}
              onPointerMove={(event) => {
                const bounds = event.currentTarget.getBoundingClientRect();
                const position = Math.min(0.999, Math.max(0, (event.clientX - bounds.left) / Math.max(1, bounds.width)));
                setStripeHover({
                  index: Math.floor(position * STRIPE_COUNT),
                  x: clampTooltipX(event.clientX),
                  y: Math.max(48, event.clientY - 12),
                });
              }}
              onPointerLeave={() => setStripeHover(null)}
            >
              {stripes.map((provider, index) => (
                <span
                  key={index}
                  className={cn(
                    "pointer-events-none min-w-0 flex-1 rounded-[2px] transition-[transform,opacity] duration-150",
                    stripeHover?.index === index && "-translate-y-1 opacity-75",
                    provider ? provider.color : "bg-muted",
                  )}
                />
              ))}
            </div>
          </div>

          <div className="mt-3 grid flex-1 grid-rows-3 divide-y">
            {providers.map((provider) => {
              const share = totalRequests > 0 ? provider.requests / totalRequests * 100 : 0;
              const successRate = provider.requests > 0 ? provider.successfulRequests / provider.requests * 100 : 0;
              return (
                <div key={provider.key} className="flex min-h-16 items-center justify-between gap-4 py-3 first:pt-0 last:pb-0">
                  <div className="flex min-w-0 items-center gap-2.5">
                    <span className={cn("size-2 shrink-0 rounded-full", provider.dot)} />
                    <div className="min-w-0">
                      <p className="truncate text-xs">{providerLabel(provider.key, t)}</p>
                      <p className="mt-0.5 truncate text-[11px] text-muted-foreground">{t("dashboard.providerDetail", { rate: formatNumber(successRate, locale, 1), tokens: formatNumber(provider.tokens, locale) })}</p>
                    </div>
                  </div>
                  <div className="shrink-0 text-right">
                    <p className="text-xs font-medium tabular-nums">{formatNumber(provider.requests, locale)}</p>
                    <p className="mt-0.5 text-[11px] tabular-nums text-muted-foreground">{formatNumber(share, locale, 1)}%</p>
                  </div>
                </div>
              );
            })}
          </div>
        </div>
      )}
      {stripeHover && typeof document !== "undefined" ? createPortal(
        <div
          className="pointer-events-none fixed z-[100] w-max max-w-64 -translate-x-1/2 -translate-y-full truncate rounded-md bg-primary px-3 py-1.5 text-xs text-primary-foreground shadow-lg"
          style={{ left: stripeHover.x, top: stripeHover.y }}
        >
          {stripeTooltip}
        </div>,
        document.body,
      ) : null}
    </DashboardPanel>
  );
}

function buildProviderStripes<T extends { requests: number }>(providers: T[], totalRequests: number): Array<T | null> {
  if (totalRequests <= 0) return Array.from({ length: STRIPE_COUNT }, () => null);
  const boundaries: number[] = [];
  let cumulative = 0;
  for (const provider of providers) {
    cumulative += provider.requests;
    boundaries.push(cumulative / totalRequests);
  }
  return Array.from({ length: STRIPE_COUNT }, (_, index) => {
    const position = (index + 0.5) / STRIPE_COUNT;
    const providerIndex = boundaries.findIndex((boundary) => position <= boundary);
    return providers[providerIndex >= 0 ? providerIndex : providers.length - 1] ?? null;
  });
}

function providerLabel(provider: ProviderKey, t: TFunction): string {
  if (provider === "grok_build") return t("models.providerGrokBuild");
  if (provider === "grok_web") return t("models.providerGrokWeb");
  return t("console.name");
}

function clampTooltipX(value: number): number {
  const inset = Math.min(136, Math.max(8, window.innerWidth / 2 - 8));
  return Math.min(window.innerWidth - inset, Math.max(inset, value));
}
