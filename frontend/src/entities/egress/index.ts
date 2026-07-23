import { apiRequest } from "@/shared/api/client";
import { createObjectDecoder, decodeBooleanResult, hasShape, isArrayOf, isBoolean, isNumber, isOneOf, isOptional, isString } from "@/shared/api/decoder";
import type { SortOrder } from "@/shared/lib/table-sort";

export type EgressScope = "grok_build" | "grok_web" | "grok_console" | "grok_web_asset";

export type EgressNodeDTO = {
	id: string; name: string; scope: EgressScope; enabled: boolean;
	proxyConfigured: boolean; userAgent: string; cookieConfigured: boolean;
	accountBoundProxy: boolean; proxyPool: boolean;
	sourceId?: string; accountCapacity: number; assignedAccountCount: number;
	health: number; failureCount: number; cooldownUntil?: string; lastError?: string;
	probeStatus: "unknown" | "healthy" | "unhealthy"; lastProbedAt?: string; probeLatencyMs: number; exitIp?: string; probeError?: string;
};

export type EgressNodeInput = {
	name: string; scope: EgressScope; enabled: boolean; proxyPool: boolean; proxyURL?: string;
	accountCapacity: number; clearProxyURL?: boolean; userAgent: string; cloudflareCookies?: string; clearCookies?: boolean;
};

export type EgressNodeListDTO = { items: EgressNodeDTO[]; defaultUserAgents: Record<EgressScope, string> };

const egressNodeShape = {
	id: isString, name: isString, scope: isOneOf("grok_build", "grok_web", "grok_console", "grok_web_asset"), enabled: isBoolean,
	proxyConfigured: isBoolean, userAgent: isString, cookieConfigured: isBoolean, accountBoundProxy: isBoolean, proxyPool: isBoolean, health: isNumber, failureCount: isNumber,
	sourceId: isOptional(isString), accountCapacity: isNumber, assignedAccountCount: isNumber,
	probeStatus: isOneOf("unknown", "healthy", "unhealthy"), lastProbedAt: isOptional(isString), probeLatencyMs: isNumber, exitIp: isOptional(isString), probeError: isOptional(isString),
	cooldownUntil: isOptional(isString), lastError: isOptional(isString),
};
const egressNodeValidator = hasShape(egressNodeShape);
const decodeEgressNode = createObjectDecoder<EgressNodeDTO>("egress node", egressNodeShape);
const decodeEgressNodeList = createObjectDecoder<EgressNodeListDTO>("egress node list", {
  items: isArrayOf(egressNodeValidator),
  defaultUserAgents: hasShape({ grok_build: isString, grok_web: isString, grok_console: isString, grok_web_asset: isString }),
});

export function listEgressNodes(input?: { sortBy?: string; sortOrder?: SortOrder }): Promise<EgressNodeListDTO> {
  const query = new URLSearchParams();
  if (input?.sortBy && input.sortOrder) {
    query.set("sortBy", input.sortBy);
    query.set("sortOrder", input.sortOrder);
  }
  const suffix = query.size > 0 ? `?${query}` : "";
  return apiRequest(`/api/admin/v1/egress-nodes${suffix}`, {}, decodeEgressNodeList);
}

export function createEgressNode(input: EgressNodeInput): Promise<EgressNodeDTO> {
  return apiRequest("/api/admin/v1/egress-nodes", { method: "POST", body: input }, decodeEgressNode);
}

export function updateEgressNode(id: string, input: EgressNodeInput): Promise<EgressNodeDTO> {
  return apiRequest(`/api/admin/v1/egress-nodes/${id}`, { method: "PUT", body: input }, decodeEgressNode);
}

export function deleteEgressNode(id: string): Promise<{ deleted: boolean }> {
  return apiRequest(`/api/admin/v1/egress-nodes/${id}`, { method: "DELETE" }, decodeBooleanResult<{ deleted: boolean }>("deleted"));
}

export function deleteEgressNodes(ids: string[]): Promise<{ deleted: number }> {
  return apiRequest("/api/admin/v1/egress-nodes", { method: "DELETE", body: { ids } }, createObjectDecoder<{ deleted: number }>("egress node batch delete", { deleted: isNumber }));
}

export function refreshEgressClearance(id: string): Promise<{ refreshed: boolean }> {
	return apiRequest(`/api/admin/v1/egress-nodes/${id}/refresh-clearance`, { method: "POST" }, decodeBooleanResult<{ refreshed: boolean }>("refreshed"));
}

export function assignEgressAccounts(nodeID: string, provider: "grok_build" | "grok_web" | "grok_console", ids: string[], mode: "manual" | "auto" = "manual"): Promise<{ assigned: number }> {
  return apiRequest(`/api/admin/v1/egress-nodes/${nodeID}/accounts`, { method: "POST", body: { provider, ids, mode } }, createObjectDecoder<{ assigned: number }>("egress account assignment", { assigned: isNumber }));
}

export function unassignEgressAccounts(provider: "grok_build" | "grok_web" | "grok_console", ids: string[]): Promise<{ assigned: number }> {
  return apiRequest("/api/admin/v1/egress-nodes/accounts", { method: "DELETE", body: { provider, ids } }, createObjectDecoder<{ assigned: number }>("egress account assignment", { assigned: isNumber }));
}
