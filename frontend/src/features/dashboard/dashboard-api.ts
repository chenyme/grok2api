import { apiRequest } from "@/shared/api/client";
import { createObjectDecoder, hasShape, isArrayOf, isNumber, isOneOf, isString } from "@/shared/api/decoder";
import type { PeriodValue } from "@/shared/lib/period";

export type DashboardPeriod = PeriodValue;

export type DashboardUsageDTO = {
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

export type DashboardDTO = {
  period: DashboardPeriod;
  generatedAt: string;
  range: { start: string; end: string };
  resources: {
    activeAccounts: number;
    totalAccounts: number;
    buildAccounts: number;
    webAccounts: number;
    consoleAccounts: number;
    enabledModels: number;
    totalModels: number;
	activeClientKeys: number;
	totalClientKeys: number;
  };
  usage: DashboardUsageDTO;
  series: Array<{ start: string; end: string; requests: number; inputTokens: number; cachedInputTokens: number; outputTokens: number; reasoningTokens: number; tokens: number; billedCostUsdTicks: number }>;
  activity: Array<{ start: string; requests: number }>;
  topModels: Array<{ model: string; requests: number; inputTokens: number; cachedInputTokens: number; outputTokens: number; reasoningTokens: number; tokens: number; billedCostUsdTicks: number }>;
  providers: Array<{ provider: string; requests: number; successfulRequests: number; tokens: number }>;
};

const dashboardSeriesItem = hasShape({
  start: isString, end: isString, requests: isNumber, inputTokens: isNumber, cachedInputTokens: isNumber,
  outputTokens: isNumber, reasoningTokens: isNumber, tokens: isNumber, billedCostUsdTicks: isNumber,
});
const dashboardUsage = hasShape({
  requests: isNumber, successfulRequests: isNumber, failedRequests: isNumber, inputTokens: isNumber,
  cachedInputTokens: isNumber, outputTokens: isNumber, reasoningTokens: isNumber, tokens: isNumber,
  billedCostUsdTicks: isNumber, successRate: isNumber,
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
    activeAccounts: isNumber, totalAccounts: isNumber, buildAccounts: isNumber, webAccounts: isNumber, consoleAccounts: isNumber, enabledModels: isNumber, totalModels: isNumber,
		activeClientKeys: isNumber, totalClientKeys: isNumber,
  }),
  usage: dashboardUsage,
  series: isArrayOf(dashboardSeriesItem),
  activity: isArrayOf(hasShape({ start: isString, requests: isNumber })),
  topModels: isArrayOf(dashboardModelItem),
  providers: isArrayOf(hasShape({ provider: isString, requests: isNumber, successfulRequests: isNumber, tokens: isNumber })),
});

export function getDashboard(period: DashboardPeriod, timezone: string, refresh = false): Promise<DashboardDTO> {
  const query = new URLSearchParams({ period, timezone });
  if (refresh) query.set("refresh", "1");
  return apiRequest(`/api/admin/v1/dashboard?${query.toString()}`, {}, decodeDashboard);
}
