import { apiRequest } from "@/shared/api/client";
import { createObjectDecoder, hasShape, isArrayOf, isBoolean, isNumber, isOneOf, isOptional, isString } from "@/shared/api/decoder";
import type { PeriodValue } from "@/shared/lib/period";
import type { SortOrder } from "@/shared/lib/table-sort";

export type AuditPeriod = PeriodValue;

export type AuditDTO = {
  id: string;
  requestId: string;
  clientKeyId: string;
  clientKeyName?: string;
  modelRouteId: string;
  modelPublicId?: string;
  modelUpstreamModel?: string;
  provider: "grok_build" | "grok_web" | "grok_console";
  operation: "responses" | "chat" | "messages" | "image" | "image_edit" | "video";
  usageSource: "upstream" | "estimated" | "none";
  accountId?: string;
  accountName?: string;
  statusCode: number;
  streaming: boolean;
  mediaInputImages: number;
  mediaOutputImages: number;
  mediaOutputSeconds: number;
  inputTokens: number;
  cachedInputTokens: number;
  outputTokens: number;
  reasoningTokens: number;
  totalTokens: number;
  costInUsdTicks: number;
  estimatedCostInUsdTicks: number;
  pricingModel?: string;
  pricingVersion?: string;
  numSourcesUsed: number;
  numServerSideToolsUsed: number;
  contextInputTokens: number;
  contextOutputTokens: number;
  durationMs: number;
  errorCode?: string;
  createdAt: string;
};

export type AuditCursorPageDTO = {
  items: AuditDTO[];
  pageSize: number;
  nextCursor: string;
  hasMore: boolean;
};

export type AuditSummaryDTO = {
  period: AuditPeriod;
  generatedAt: string;
  range: { start: string; end: string };
  usage: {
    requests: number;
    successfulRequests: number;
    failedRequests: number;
    inputTokens: number;
    cachedInputTokens: number;
    outputTokens: number;
    reasoningTokens: number;
    totalTokens: number;
    averageDurationMs: number;
    successRate: number;
    estimatedCostInUsdTicks: number;
  };
  pricing: {
    source: string;
    asOf: string;
    pricedRequests: number;
    unpricedRequests: number;
    pricedTokens: number;
    unpricedTokens: number;
  };
};

const auditValidator = hasShape({
  id: isString, requestId: isString, clientKeyId: isString, clientKeyName: isOptional(isString), modelRouteId: isString,
  modelPublicId: isOptional(isString), modelUpstreamModel: isOptional(isString), provider: isOneOf("grok_build", "grok_web", "grok_console"),
  operation: isOneOf("responses", "chat", "messages", "image", "image_edit", "video"), usageSource: isOneOf("upstream", "estimated", "none"),
  accountId: isOptional(isString), accountName: isOptional(isString), statusCode: isNumber, streaming: isBoolean,
  mediaInputImages: isNumber, mediaOutputImages: isNumber, mediaOutputSeconds: isNumber, inputTokens: isNumber,
  cachedInputTokens: isNumber, outputTokens: isNumber, reasoningTokens: isNumber, totalTokens: isNumber,
  costInUsdTicks: isNumber, estimatedCostInUsdTicks: isNumber, pricingModel: isOptional(isString), pricingVersion: isOptional(isString),
  numSourcesUsed: isNumber, numServerSideToolsUsed: isNumber, contextInputTokens: isNumber, contextOutputTokens: isNumber,
  durationMs: isNumber, errorCode: isOptional(isString), createdAt: isString,
});
const decodeAuditPage = createObjectDecoder<AuditCursorPageDTO>("audit page", {
  items: isArrayOf(auditValidator), pageSize: isNumber, nextCursor: isString, hasMore: isBoolean,
});
const decodeAuditSummary = createObjectDecoder<AuditSummaryDTO>("audit summary", {
  period: isOneOf("24h", "7d", "30d", "90d"), generatedAt: isString, range: hasShape({ start: isString, end: isString }),
  usage: hasShape({
    requests: isNumber, successfulRequests: isNumber, failedRequests: isNumber, inputTokens: isNumber,
    cachedInputTokens: isNumber, outputTokens: isNumber, reasoningTokens: isNumber, totalTokens: isNumber,
    averageDurationMs: isNumber, successRate: isNumber, estimatedCostInUsdTicks: isNumber,
  }),
  pricing: hasShape({
    source: isString, asOf: isString, pricedRequests: isNumber, unpricedRequests: isNumber, pricedTokens: isNumber, unpricedTokens: isNumber,
  }),
});

type AuditQuery = {
  cursor?: string;
  pageSize?: number;
  search?: string;
  model?: string;
  status?: string;
  mode?: string;
  key?: string;
  account?: string;
  period: AuditPeriod;
  sortBy?: string;
  sortOrder?: SortOrder;
};

export function getRequestAudits(input: AuditQuery): Promise<AuditCursorPageDTO> {
  const query = new URLSearchParams({ pagination: "cursor", pageSize: String(input.pageSize ?? 50), period: input.period });
  if (input.cursor) query.set("cursor", input.cursor);
  if (input.search) query.set("search", input.search);
  if (input.model) query.set("model", input.model);
  if (input.status) query.set("status", input.status);
  if (input.mode) query.set("mode", input.mode);
  if (input.key) query.set("key", input.key);
  if (input.account) query.set("account", input.account);
  if (input.sortBy && input.sortOrder) {
    query.set("sortBy", input.sortBy);
    query.set("sortOrder", input.sortOrder);
  }
  return apiRequest(`/api/admin/v1/request-audits?${query}`, {}, decodeAuditPage);
}

export function getRequestAuditSummary(input: Omit<AuditQuery, "cursor" | "pageSize">, refresh = false): Promise<AuditSummaryDTO> {
  const query = new URLSearchParams({ period: input.period });
  if (input.search) query.set("search", input.search);
  if (input.model) query.set("model", input.model);
  if (input.status) query.set("status", input.status);
  if (input.mode) query.set("mode", input.mode);
  if (input.key) query.set("key", input.key);
  if (input.account) query.set("account", input.account);
  if (refresh) query.set("refresh", "1");
  return apiRequest(`/api/admin/v1/request-audits/summary?${query}`, {}, decodeAuditSummary);
}
