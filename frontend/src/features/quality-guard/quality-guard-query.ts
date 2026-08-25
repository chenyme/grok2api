const QUALITY_GUARD_STATUS_PATH = "/api/admin/v1/egress-quality-guard";

export function qualityGuardStatusPath(nodeIds?: string[]): string {
  if (nodeIds === undefined) return QUALITY_GUARD_STATUS_PATH;
  const query = new URLSearchParams();
  if (nodeIds.length === 0) query.append("nodeId", "");
  else nodeIds.forEach((nodeId) => query.append("nodeId", nodeId));
  return `${QUALITY_GUARD_STATUS_PATH}?${query}`;
}
