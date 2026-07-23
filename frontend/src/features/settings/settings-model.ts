import { z } from "zod";

import type { SettingsConfigDTO } from "@/features/settings/settings-api";

export type DurationUnit = "s" | "m" | "h" | "d";
export type DurationValue = { value: number; unit: DurationUnit };
export type ByteSizeUnit = "MiB" | "GiB";
export type ByteSizeValue = { value: number; unit: ByteSizeUnit };

const durationSchema = z.object({ value: z.number().positive(), unit: z.enum(["s", "m", "h", "d"]) });
const positiveInteger = z.number().int().positive();
const byteSizeSchema = z.object({ value: z.number().positive(), unit: z.enum(["MiB", "GiB"]) });
const routingTTLDuration = durationSchema.refine((value) => durationSeconds(value) <= 30 * 86_400);
const routingCooldownDuration = durationSchema.refine((value) => durationSeconds(value) <= 86_400);
const routingCapacityWaitDuration = durationSchema.refine((value) => durationSeconds(value) <= 5);
const auditFlushDuration = durationSchema.refine((value) => {
  const seconds = durationSeconds(value);
  return seconds >= 0.01 && seconds <= 60;
});
const consoleChatDuration = durationSchema.refine((value) => {
  const seconds = durationSeconds(value);
  return seconds >= 5 && seconds <= 30 * 60;
});
const buildResponseHeaderDuration = durationSchema.refine((value) => {
  const seconds = durationSeconds(value);
  return seconds >= 30 && seconds <= 30 * 60;
});
const forbiddenCodePattern = /^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/;

function parseForbiddenCodes(value: string): string[] {
  const seen = new Set<string>();
  const result: string[] = [];
  for (const item of value.split(/[\n,]/)) {
    const code = item.trim().toLowerCase();
    if (code === "" || seen.has(code)) continue;
    seen.add(code);
    result.push(code);
  }
  return result;
}

function validPublicAPIBaseURL(value: string): boolean {
  const trimmed = value.trim();
  if (trimmed.length === 0) return true;
  try {
    const parsed = new URL(trimmed);
    if (parsed.username !== "" || parsed.password !== "" || parsed.search !== "" || parsed.hash !== "") return false;
    return parsed.protocol === "http:" || parsed.protocol === "https:";
  } catch {
    return false;
  }
}

export const settingsSchema = z.object({
  server: z.object({
    maxConcurrentRequests: positiveInteger.max(100_000),
  }),
  providerBuild: z.object({
    baseURL: z.url(),
    fallbackBaseURL: z.url().refine((value) => value.startsWith("https://")),
    clientVersion: z.string().trim().min(1),
    clientIdentifier: z.string().trim().min(1),
    tokenAuth: z.string().trim().min(1),
    tokenAuthConfigured: z.boolean(),
    userAgent: z.string().trim().min(1),
    responseHeaderTimeout: buildResponseHeaderDuration,
  }),
  providerWeb: z.object({
    baseURL: z.url().refine((value) => value.startsWith("https://")),
    statsigMode: z.enum(["manual", "url"]),
    statsigManualValue: z.string().trim().max(4096),
    statsigManualConfigured: z.boolean(),
    statsigSignerURL: z.string().trim().max(2048),
    clearanceMode: z.enum(["manual", "flaresolverr"]),
    flareSolverrURL: z.string().trim().max(2048),
    clearanceTimeout: durationSchema.refine((value) => durationSeconds(value) >= 10 && durationSeconds(value) <= 300),
    clearanceRefresh: durationSchema.refine((value) => durationSeconds(value) >= 60 && durationSeconds(value) <= 86_400),
    quotaTimeout: durationSchema, chatTimeout: durationSchema, imageTimeout: durationSchema, videoTimeout: durationSchema,
    mediaConcurrency: positiveInteger.max(64), allowNSFW: z.boolean(),
    recoveryBackoffBase: durationSchema, recoveryBackoffMax: durationSchema,
  }).superRefine((value, context) => {
    if (durationSeconds(value.recoveryBackoffMax) < durationSeconds(value.recoveryBackoffBase)) {
      context.addIssue({ code: "custom", path: ["recoveryBackoffMax"], message: "invalid" });
    }
    if (value.statsigMode === "manual" && !value.statsigManualConfigured && value.statsigManualValue.length === 0) {
      context.addIssue({ code: "custom", path: ["statsigManualValue"], message: "required" });
    }
    if (value.statsigManualValue.length > 0 && !validStatsigID(value.statsigManualValue)) {
      context.addIssue({ code: "custom", path: ["statsigManualValue"], message: "invalid" });
    }
    if (value.statsigMode === "url") {
      if (!validStatsigSignerURL(value.statsigSignerURL)) {
        context.addIssue({ code: "custom", path: ["statsigSignerURL"], message: "invalid" });
      }
    }
    if (value.clearanceMode === "flaresolverr" && !validHTTPURL(value.flareSolverrURL)) {
      context.addIssue({ code: "custom", path: ["flareSolverrURL"], message: "invalid" });
    }
  }),
  providerConsole: z.object({
    baseURL: z.url().refine((value) => value.startsWith("https://")),
    chatTimeout: consoleChatDuration,
  }),
  batch: z.object({
    importConcurrency: positiveInteger.max(50),
    conversionConcurrency: positiveInteger.max(50),
    syncConcurrency: positiveInteger.max(50),
    refreshConcurrency: positiveInteger.max(50),
    randomDelay: z.number().int().min(0).max(5_000),
  }),
  media: z.object({
    maxImageSize: byteSizeSchema.refine((value) => byteSizeBytes(value) >= 1 << 20 && byteSizeBytes(value) <= 32 << 20),
    maxTotalSize: byteSizeSchema.refine((value) => byteSizeBytes(value) <= 2 ** 40),
    cleanupThresholdPercent: z.number().int().min(50).max(95),
    cleanupInterval: durationSchema.refine((value) => durationSeconds(value) >= 60 && durationSeconds(value) <= 86_400),
  }).refine((value) => byteSizeBytes(value.maxTotalSize) >= byteSizeBytes(value.maxImageSize), { path: ["maxTotalSize"] }),
  frontend: z.object({
    publicApiBaseURL: z.string().trim().max(2048).refine((value) => validPublicAPIBaseURL(value), { message: "invalid" }),
  }),
  routing: z.object({
    stickyTTL: routingTTLDuration,
    cooldownBase: routingCooldownDuration,
    cooldownMax: routingCooldownDuration,
    capacityWait: routingCapacityWaitDuration,
    maxAttempts: positiveInteger.max(10),
    preferFreeBuild: z.boolean(),
    segmentedSelector: z.object({
      enabled: z.boolean(),
      minCandidates: z.number().int().min(100).max(1_000_000),
      windowSize: z.number().int().min(8).max(256),
    }),
  }).refine((value) => durationSeconds(value.cooldownMax) >= durationSeconds(value.cooldownBase), { path: ["cooldownMax"] })
    .refine((value) => value.segmentedSelector.windowSize <= value.segmentedSelector.minCandidates, { path: ["segmentedSelector", "windowSize"] }),
  audit: z.object({ bufferSize: positiveInteger.max(262_144), batchSize: positiveInteger.max(4_096), flushInterval: auditFlushDuration, commitDelayMS: positiveInteger.max(50) })
    .refine((value) => value.batchSize <= value.bufferSize, { path: ["batchSize"] }),
  clientKeyDefaults: z.object({ rpmLimit: positiveInteger.max(100_000), maxConcurrent: positiveInteger.max(1_024) }),
  accounts: z.object({
    markBuildForbiddenReauth: z.boolean(),
    buildForbiddenReauthCodes: z.string().superRefine((value, context) => {
      const codes = parseForbiddenCodes(value);
      if (codes.length === 0 || codes.length > 32 || codes.some((code) => !forbiddenCodePattern.test(code))) {
        context.addIssue({ code: "custom", message: "invalid" });
      }
    }),
    autoCleanReauthEnabled: z.boolean(),
    autoCleanReauthInterval: durationSchema.refine((value) => {
      const seconds = durationSeconds(value);
      return seconds >= 60 && seconds <= 3_600;
    }),
    autoCleanReauthMinAge: durationSchema.refine((value) => {
      const seconds = durationSeconds(value);
      return seconds >= 60 && seconds <= 30 * 86_400;
    }),
    autoCleanIncludeDisabled: z.boolean(),
  }),
});

export type SettingsForm = z.infer<typeof settingsSchema>;

export function toSettingsForm(config: SettingsConfigDTO): SettingsForm {
  return {
    server: config.server,
    providerBuild: { ...config.providerBuild, responseHeaderTimeout: parseDuration(config.providerBuild.responseHeaderTimeout) },
    providerWeb: {
      ...config.providerWeb,
      statsigManualValue: "",
      clearanceTimeout: parseDuration(config.providerWeb.clearanceTimeout), clearanceRefresh: parseDuration(config.providerWeb.clearanceRefresh),
      quotaTimeout: parseDuration(config.providerWeb.quotaTimeout), chatTimeout: parseDuration(config.providerWeb.chatTimeout),
      imageTimeout: parseDuration(config.providerWeb.imageTimeout), videoTimeout: parseDuration(config.providerWeb.videoTimeout),
      recoveryBackoffBase: parseDuration(config.providerWeb.recoveryBackoffBase), recoveryBackoffMax: parseDuration(config.providerWeb.recoveryBackoffMax),
    },
    providerConsole: { ...config.providerConsole, chatTimeout: parseDuration(config.providerConsole.chatTimeout) },
    batch: { ...config.batch, randomDelay: parseDurationMilliseconds(config.batch.randomDelay) },
    media: {
      maxImageSize: parseByteSize(config.media.maxImageBytes), maxTotalSize: parseByteSize(config.media.maxTotalBytes),
      cleanupThresholdPercent: config.media.cleanupThresholdPercent,
      cleanupInterval: parseDuration(config.media.cleanupInterval),
    },
    frontend: {
      publicApiBaseURL: config.frontend.publicApiBaseURL,
    },
    routing: {
      stickyTTL: parseDuration(config.routing.stickyTTL), cooldownBase: parseDuration(config.routing.cooldownBase),
      cooldownMax: parseDuration(config.routing.cooldownMax), capacityWait: parseDuration(config.routing.capacityWait), maxAttempts: config.routing.maxAttempts,
      preferFreeBuild: config.routing.preferFreeBuild,
      segmentedSelector: config.routing.segmentedSelector,
    },
    audit: { bufferSize: config.audit.bufferSize, batchSize: config.audit.batchSize, flushInterval: parseDuration(config.audit.flushInterval), commitDelayMS: config.audit.commitDelayMS },
    clientKeyDefaults: config.clientKeyDefaults,
    accounts: {
      markBuildForbiddenReauth: config.accounts.markBuildForbiddenReauth,
      buildForbiddenReauthCodes: config.accounts.buildForbiddenReauthCodes.join("\n"),
      autoCleanReauthEnabled: config.accounts.autoCleanReauthEnabled,
      autoCleanReauthInterval: parseDuration(config.accounts.autoCleanReauthInterval),
      autoCleanReauthMinAge: parseDuration(config.accounts.autoCleanReauthMinAge),
      autoCleanIncludeDisabled: config.accounts.autoCleanIncludeDisabled,
    },
  };
}

export function toSettingsDTO(config: SettingsForm): SettingsConfigDTO {
  return {
    server: config.server,
    providerBuild: { ...config.providerBuild, responseHeaderTimeout: formatDuration(config.providerBuild.responseHeaderTimeout) },
    providerWeb: {
      ...config.providerWeb,
      quotaTimeout: formatDuration(config.providerWeb.quotaTimeout), chatTimeout: formatDuration(config.providerWeb.chatTimeout),
      imageTimeout: formatDuration(config.providerWeb.imageTimeout), videoTimeout: formatDuration(config.providerWeb.videoTimeout),
      clearanceTimeout: formatDuration(config.providerWeb.clearanceTimeout), clearanceRefresh: formatDuration(config.providerWeb.clearanceRefresh),
      recoveryBackoffBase: formatDuration(config.providerWeb.recoveryBackoffBase), recoveryBackoffMax: formatDuration(config.providerWeb.recoveryBackoffMax),
    },
    providerConsole: { ...config.providerConsole, chatTimeout: formatDuration(config.providerConsole.chatTimeout) },
    batch: { ...config.batch, randomDelay: `${config.batch.randomDelay}ms` },
    media: {
      maxImageBytes: byteSizeBytes(config.media.maxImageSize), maxTotalBytes: byteSizeBytes(config.media.maxTotalSize),
      cleanupThresholdPercent: config.media.cleanupThresholdPercent,
      cleanupInterval: formatDuration(config.media.cleanupInterval),
    },
    frontend: {
      publicApiBaseURL: config.frontend.publicApiBaseURL.trim(),
    },
    routing: {
      stickyTTL: formatDuration(config.routing.stickyTTL), cooldownBase: formatDuration(config.routing.cooldownBase),
      cooldownMax: formatDuration(config.routing.cooldownMax), capacityWait: formatDuration(config.routing.capacityWait), maxAttempts: config.routing.maxAttempts,
      preferFreeBuild: config.routing.preferFreeBuild,
      segmentedSelector: config.routing.segmentedSelector,
    },
    audit: { bufferSize: config.audit.bufferSize, batchSize: config.audit.batchSize, flushInterval: formatDuration(config.audit.flushInterval), commitDelayMS: config.audit.commitDelayMS },
    clientKeyDefaults: config.clientKeyDefaults,
    accounts: {
      markBuildForbiddenReauth: config.accounts.markBuildForbiddenReauth,
      buildForbiddenReauthCodes: parseForbiddenCodes(config.accounts.buildForbiddenReauthCodes),
      autoCleanReauthEnabled: config.accounts.autoCleanReauthEnabled,
      autoCleanReauthInterval: formatDuration(config.accounts.autoCleanReauthInterval),
      autoCleanReauthMinAge: formatDuration(config.accounts.autoCleanReauthMinAge),
      autoCleanIncludeDisabled: config.accounts.autoCleanIncludeDisabled,
    },
  };
}

export function isDurationUnit(value: string): value is DurationUnit {
  return value === "s" || value === "m" || value === "h" || value === "d";
}

export function isByteSizeUnit(value: string): value is ByteSizeUnit {
  return value === "MiB" || value === "GiB";
}

function byteSizeBytes(value: ByteSizeValue): number {
  return Math.round(value.value * (value.unit === "GiB" ? 2 ** 30 : 2 ** 20));
}

function parseByteSize(bytes: number): ByteSizeValue {
  if (bytes >= 2 ** 30 && bytes % 2 ** 30 === 0) return { value: bytes / 2 ** 30, unit: "GiB" };
  return { value: bytes / 2 ** 20, unit: "MiB" };
}

function durationSeconds(value: DurationValue): number {
  const factors: Record<DurationUnit, number> = { s: 1, m: 60, h: 3_600, d: 86_400 };
  return value.value * factors[value.unit];
}

function formatDuration(value: DurationValue): string {
  if (value.unit === "d") return `${value.value * 24}h`;
  return `${value.value}${value.unit}`;
}

function parseDuration(value: string): DurationValue {
  const simple = value.match(/^(\d+(?:\.\d+)?)(ms|s|m|h)$/);
  if (simple) {
    const amount = Number(simple[1]);
    if (simple[2] === "ms") return { value: amount / 1000, unit: "s" };
    if (simple[2] === "h" && amount >= 24 && amount % 24 === 0) return { value: amount / 24, unit: "d" };
    if (isDurationUnit(simple[2])) return { value: amount, unit: simple[2] };
  }

  const factors: Record<string, number> = { ns: 0.000001, us: 0.001, "µs": 0.001, ms: 1, s: 1000, m: 60_000, h: 3_600_000 };
  const parts = [...value.matchAll(/(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/g)];
  if (parts.map((part) => part[0]).join("") !== value || parts.length === 0) return { value: 1, unit: "s" };
  const milliseconds = parts.reduce((total, part) => total + Number(part[1]) * factors[part[2]], 0);
  const units: Array<[DurationUnit, number]> = [["d", 86_400_000], ["h", 3_600_000], ["m", 60_000], ["s", 1000]];
  for (const [unit, factor] of units) {
    const amount = milliseconds / factor;
    if (amount >= 1 && Number.isInteger(amount)) return { value: amount, unit };
  }
  return { value: milliseconds / 1000, unit: "s" };
}

function parseDurationMilliseconds(value: string): number {
  return Math.round(durationSeconds(parseDuration(value)) * 1000);
}

function validStatsigID(value: string): boolean {
  try {
    const normalized = value.trim().replace(/-/g, "+").replace(/_/g, "/");
    const padded = normalized.padEnd(Math.ceil(normalized.length / 4) * 4, "=");
    return atob(padded).length === 70;
  } catch {
    return false;
  }
}

function validStatsigSignerURL(value: string): boolean {
  try {
    const parsed = new URL(value);
    if (parsed.username !== "" || parsed.password !== "" || parsed.search !== "" || parsed.hash !== "") return false;
    const internal = internalSignerHostname(parsed.hostname);
    if (internal) return parsed.protocol === "http:" || parsed.protocol === "https:";
    return parsed.protocol === "https:" && (parsed.port === "" || parsed.port === "443");
  } catch {
    return false;
  }
}

function validHTTPURL(value: string): boolean {
  try {
    const parsed = new URL(value);
    if (parsed.username !== "" || parsed.password !== "" || parsed.search !== "" || parsed.hash !== "") return false;
    const internal = internalSignerHostname(parsed.hostname);
    if (internal) return parsed.protocol === "http:" || parsed.protocol === "https:";
    return parsed.protocol === "https:" && (parsed.port === "" || parsed.port === "443");
  } catch {
    return false;
  }
}

function internalSignerHostname(value: string): boolean {
  const host = value.toLowerCase().replace(/^\[|\]$/g, "").replace(/\.$/, "");
  if (host === "localhost" || host.endsWith(".localhost") || host.endsWith(".local") || host.endsWith(".internal")) return true;
  if (!host.includes(".")) {
    if (host.includes(":")) return host === "::1" || /^(?:fc|fd|fe[89ab])/i.test(host);
    return /^[a-z0-9](?:[a-z0-9_-]{0,61}[a-z0-9])?$/i.test(host);
  }
  const octets = host.split(".").map(Number);
  if (octets.length !== 4 || octets.some((part) => !Number.isInteger(part) || part < 0 || part > 255)) return false;
  return octets[0] === 10 || octets[0] === 127 || octets[0] === 169 && octets[1] === 254 || octets[0] === 172 && octets[1] >= 16 && octets[1] <= 31 || octets[0] === 192 && octets[1] === 168;
}
