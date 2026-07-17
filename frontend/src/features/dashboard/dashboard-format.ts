const USD_TICKS = 10_000_000_000;

export function formatUSD(ticks: number, locale: string): string {
  return formatUSDValue(ticks / USD_TICKS, locale);
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

export function usdTicksToValue(ticks: number): number {
  return ticks / USD_TICKS;
}
