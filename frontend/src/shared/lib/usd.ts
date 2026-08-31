export const USD_TICKS_PER_DOLLAR = 10_000_000_000;

export function usdTicksToValue(ticks: number): number {
  return ticks / USD_TICKS_PER_DOLLAR;
}

export function formatUSDTicks(ticks: number, fractionDigits: number): string {
  return `$${usdTicksToValue(ticks).toFixed(fractionDigits)}`;
}

export function formatUSDTicksWithEstimate(
  actualTicks: number,
  estimatedTicks: number,
  estimatedLabel: string,
  fractionDigits = 6,
): string {
  const actual = Number.isFinite(actualTicks) && actualTicks > 0 ? actualTicks : 0;
  const estimated = Number.isFinite(estimatedTicks) && estimatedTicks > 0 ? estimatedTicks : 0;
  const ticks = actual || estimated;
  if (ticks === 0) return "$0";

  const estimateSuffix = actual === 0 ? ` (${estimatedLabel})` : "";
  return `${formatUSDTicks(ticks, fractionDigits)}${estimateSuffix}`;
}
