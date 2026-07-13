import { apiRequest } from "@/shared/api/client";
import type { PeriodValue } from "@/shared/lib/period";

export type DashboardPeriod = PeriodValue;

export type DashboardDTO = {
  period: DashboardPeriod;
  generatedAt: string;
  range: { start: string; end: string };
  resources: {
    activeAccounts: number;
    totalAccounts: number;
    enabledModels: number;
    totalModels: number;
    activeClientKeys: number;
    totalClientKeys: number;
    allTimeRequests: number;
  };
  usage: {
    requests: number;
    successfulRequests: number;
    failedRequests: number;
    inputTokens: number;
    cachedInputTokens: number;
    outputTokens: number;
    reasoningTokens: number;
    tokens: number;
    billedCostUsdTicks: number;
    successRate: number;
  };
  series: Array<{ start: string; end: string; requests: number; inputTokens: number; cachedInputTokens: number; outputTokens: number; reasoningTokens: number; tokens: number; billedCostUsdTicks: number; models: Array<{ model: string; tokens: number; billedCostUsdTicks: number }> }>;
  topModels: Array<{ model: string; requests: number; inputTokens: number; cachedInputTokens: number; outputTokens: number; reasoningTokens: number; tokens: number; billedCostUsdTicks: number }>;
};

export function getDashboard(period: DashboardPeriod, timezone: string, refresh = false): Promise<DashboardDTO> {
  const query = new URLSearchParams({ period, timezone });
  if (refresh) query.set("refresh", "1");
  return apiRequest<DashboardDTO>(`/api/admin/v1/dashboard?${query.toString()}`);
}
