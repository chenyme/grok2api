export const MISSING_AUDIT_USAGE_PLACEHOLDER = "—";

export type AuditUsageOperation =
  | "responses"
  | "compaction"
  | "chat"
  | "messages"
  | "image"
  | "image_edit"
  | "video"
  | "tts"
  | "stt"
  | "realtime"
  | "voice";

const DURATION_OPERATIONS = new Set<AuditUsageOperation>(["tts", "stt", "realtime", "voice"]);

export type AuditUsageLabels = {
  input: string;
  output: string;
  cached: string;
  reasoning: string;
  mediaInput: string;
  mediaOutput: string;
  imageCount: (count: number) => string;
  secondsCount: (count: number) => string;
};

export type AuditUsageItem = {
  key: "mediaInput" | "mediaOutput" | "input" | "output" | "cached" | "reasoning";
  label: string;
  value: string;
};

export type AuditUsageView = {
  mode: "compaction" | "duration" | "metrics";
  mediaItems?: AuditUsageItem[];
  tokenItems?: AuditUsageItem[];
  durationSeconds?: string;
};

export type AuditUsageInput = {
  operation: AuditUsageOperation;
  usageSource: "upstream" | "estimated" | "none";
  mediaInputImages: number;
  mediaOutputImages: number;
  mediaOutputSeconds: number;
  inputTokens: number;
  cachedInputTokens: number;
  outputTokens: number;
  reasoningTokens: number;
  totalTokens: number;
  durationMs: number;
};

export function auditTokenUsageAvailable(audit: Pick<AuditUsageInput, "usageSource">): boolean {
  return audit.usageSource !== "none";
}

export function formatAuditTokenValue(value: number, available: boolean, formatNumber: (value: number) => string): string {
  if (!available) {
    return MISSING_AUDIT_USAGE_PLACEHOLDER;
  }
  return formatNumber(value);
}

function mediaItems(audit: AuditUsageInput, labels: AuditUsageLabels): AuditUsageItem[] | undefined {
  if (audit.operation === "video") {
    return [
      { key: "mediaInput", label: labels.mediaInput, value: labels.imageCount(audit.mediaInputImages) },
      { key: "mediaOutput", label: labels.mediaOutput, value: labels.secondsCount(audit.mediaOutputSeconds) },
    ];
  }
  if (audit.operation === "image" || audit.operation === "image_edit" || audit.mediaInputImages > 0 || audit.mediaOutputImages > 0) {
    return [
      { key: "mediaInput", label: labels.mediaInput, value: labels.imageCount(audit.mediaInputImages) },
      { key: "mediaOutput", label: labels.mediaOutput, value: labels.imageCount(audit.mediaOutputImages) },
    ];
  }
  return undefined;
}

function tokenItems(audit: AuditUsageInput, formatNumber: (value: number) => string, labels: AuditUsageLabels): AuditUsageItem[] {
  const available = auditTokenUsageAvailable(audit);
  return [
    { key: "input", label: labels.input, value: formatAuditTokenValue(audit.inputTokens, available, formatNumber) },
    { key: "output", label: labels.output, value: formatAuditTokenValue(audit.outputTokens, available, formatNumber) },
    { key: "cached", label: labels.cached, value: formatAuditTokenValue(audit.cachedInputTokens, available, formatNumber) },
    { key: "reasoning", label: labels.reasoning, value: formatAuditTokenValue(audit.reasoningTokens, available, formatNumber) },
  ];
}

export function buildAuditUsageView(
  audit: AuditUsageInput,
  formatNumber: (value: number) => string,
  labels: AuditUsageLabels,
): AuditUsageView {
  if (audit.operation === "compaction" && audit.totalTokens === 0) {
    return { mode: "compaction" };
  }
  if (DURATION_OPERATIONS.has(audit.operation)) {
    return { mode: "duration", durationSeconds: (audit.durationMs / 1000).toFixed(2) };
  }
  return {
    mode: "metrics",
    mediaItems: mediaItems(audit, labels),
    tokenItems: tokenItems(audit, formatNumber, labels),
  };
}
