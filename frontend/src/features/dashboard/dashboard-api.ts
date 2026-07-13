import { apiRequest } from "@/shared/api/client";
import { createObjectDecoder, hasShape, isArrayOf, isNumber, isOneOf, isString } from "@/shared/api/decoder";
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

const dashboardSeriesModel = hasShape({ model: isString, tokens: isNumber, billedCostUsdTicks: isNumber });
const dashboardSeriesItem = hasShape({
  start: isString, end: isString, requests: isNumber, inputTokens: isNumber, cachedInputTokens: isNumber,
  outputTokens: isNumber, reasoningTokens: isNumber, tokens: isNumber, billedCostUsdTicks: isNumber, models: isArrayOf(dashboardSeriesModel),
});
const dashboardModelItem = hasShape({
  model: isString, requests: isNumber, inputTokens: isNumber, cachedInputTokens: isNumber,
  outputTokens: isNumber, reasoningTokens: isNumber, tokens: isNumber, billedCostUsdTicks: isNumber,
});
const decodeDashboard = createObjectDecoder<DashboardDTO>("dashboard", {
  period: isOneOf("24h", "7d", "30d", "90d"),
  generatedAt: isString,
  range: hasShape({ start: isString, end: isString }),
  resources: hasShape({
    activeAccounts: isNumber, totalAccounts: isNumber, enabledModels: isNumber, totalModels: isNumber,
    activeClientKeys: isNumber, totalClientKeys: isNumber, allTimeRequests: isNumber,
  }),
  usage: hasShape({
    requests: isNumber, successfulRequests: isNumber, failedRequests: isNumber, inputTokens: isNumber,
    cachedInputTokens: isNumber, outputTokens: isNumber, reasoningTokens: isNumber, tokens: isNumber,
    billedCostUsdTicks: isNumber, successRate: isNumber,
  }),
  series: isArrayOf(dashboardSeriesItem),
  topModels: isArrayOf(dashboardModelItem),
});

export function getDashboard(period: DashboardPeriod, timezone: string, refresh = false): Promise<DashboardDTO> {
  const query = new URLSearchParams({ period, timezone });
  if (refresh) query.set("refresh", "1");
  return apiRequest(`/api/admin/v1/dashboard?${query.toString()}`, {}, decodeDashboard);
}
