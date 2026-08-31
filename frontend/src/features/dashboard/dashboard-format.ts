import { usdTicksToValue } from "@/shared/lib/usd";

export function formatUSD(ticks: number, locale: string): string {
  return formatUSDValue(usdTicksToValue(ticks), locale);
}

export function formatUSDValue(value: number, locale: string): string {
  return `$${new Intl.NumberFormat(locale, { minimumFractionDigits: 2, maximumFractionDigits: 2 }).format(value)}`;
}

export function formatCompactUSD(value: number, locale: string): string {
  return `$${new Intl.NumberFormat(locale, { notation: "compact", maximumFractionDigits: 1 }).format(value)}`;
}

export function formatCompactNumber(value: number, locale: string): string {
  return new Intl.NumberFormat(locale, { notation: "compact", maximumFractionDigits: 1 }).format(value);
}
