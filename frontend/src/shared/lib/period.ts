export const PERIOD_DAYS = [1, 7, 30, 90] as const;

export type PeriodDays = (typeof PERIOD_DAYS)[number];
export type PeriodValue = "24h" | "7d" | "30d" | "90d";

export function toPeriodValue(days: PeriodDays): PeriodValue {
  return days === 1 ? "24h" : `${days}d`;
}
