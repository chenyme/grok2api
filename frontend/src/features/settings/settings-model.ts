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
const auditFlushDuration = durationSchema.refine((value) => {
  const seconds = durationSeconds(value);
  return seconds >= 0.01 && seconds <= 60;
});

export const settingsSchema = z.object({
  providerBuild: z.object({
    baseURL: z.url(),
    clientVersion: z.string().trim().min(1),
    clientIdentifier: z.string().trim().min(1),
    tokenAuth: z.string().trim().min(1),
    userAgent: z.string().trim().min(1),
  }),
  providerWeb: z.object({
    baseURL: z.url().refine((value) => value.startsWith("https://")),
    statsigMode: z.enum(["manual", "url"]),
    statsigManualValue: z.string().trim().max(4096),
    statsigManualConfigured: z.boolean(),
    statsigSignerURL: z.string().trim().max(2048),
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
      const valid = (() => {
        try {
          const parsed = new URL(value.statsigSignerURL);
          return parsed.protocol === "https:" && parsed.username === "" && parsed.password === "" && parsed.search === "" && parsed.hash === "" && (parsed.port === "" || parsed.port === "443");
        } catch {
          return false;
        }
      })();
      if (!valid) {
        context.addIssue({ code: "custom", path: ["statsigSignerURL"], message: "invalid" });
      }
    }
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
  routing: z.object({
    stickyTTL: routingTTLDuration,
    cooldownBase: routingCooldownDuration,
    cooldownMax: routingCooldownDuration,
    maxAttempts: positiveInteger.max(10),
  }).refine((value) => durationSeconds(value.cooldownMax) >= durationSeconds(value.cooldownBase), { path: ["cooldownMax"] }),
  audit: z.object({ bufferSize: positiveInteger.max(262_144), batchSize: positiveInteger.max(4_096), flushInterval: auditFlushDuration })
    .refine((value) => value.batchSize <= value.bufferSize, { path: ["batchSize"] }),
  clientKeyDefaults: z.object({ rpmLimit: positiveInteger.max(100_000), maxConcurrent: positiveInteger.max(1_024) }),
});

export type SettingsForm = z.infer<typeof settingsSchema>;

export function toSettingsForm(config: SettingsConfigDTO): SettingsForm {
  return {
    providerBuild: config.providerBuild,
    providerWeb: {
      ...config.providerWeb,
      statsigManualValue: "",
      quotaTimeout: parseDuration(config.providerWeb.quotaTimeout), chatTimeout: parseDuration(config.providerWeb.chatTimeout),
      imageTimeout: parseDuration(config.providerWeb.imageTimeout), videoTimeout: parseDuration(config.providerWeb.videoTimeout),
      recoveryBackoffBase: parseDuration(config.providerWeb.recoveryBackoffBase), recoveryBackoffMax: parseDuration(config.providerWeb.recoveryBackoffMax),
    },
    batch: { ...config.batch, randomDelay: parseDurationMilliseconds(config.batch.randomDelay) },
    media: {
      maxImageSize: parseByteSize(config.media.maxImageBytes), maxTotalSize: parseByteSize(config.media.maxTotalBytes),
      cleanupThresholdPercent: config.media.cleanupThresholdPercent,
      cleanupInterval: parseDuration(config.media.cleanupInterval),
    },
    routing: {
      stickyTTL: parseDuration(config.routing.stickyTTL), cooldownBase: parseDuration(config.routing.cooldownBase),
      cooldownMax: parseDuration(config.routing.cooldownMax), maxAttempts: config.routing.maxAttempts,
    },
    audit: { bufferSize: config.audit.bufferSize, batchSize: config.audit.batchSize, flushInterval: parseDuration(config.audit.flushInterval) },
    clientKeyDefaults: config.clientKeyDefaults,
  };
}

export function toSettingsDTO(config: SettingsForm): SettingsConfigDTO {
  return {
    providerBuild: config.providerBuild,
    providerWeb: {
      ...config.providerWeb,
      quotaTimeout: formatDuration(config.providerWeb.quotaTimeout), chatTimeout: formatDuration(config.providerWeb.chatTimeout),
      imageTimeout: formatDuration(config.providerWeb.imageTimeout), videoTimeout: formatDuration(config.providerWeb.videoTimeout),
      recoveryBackoffBase: formatDuration(config.providerWeb.recoveryBackoffBase), recoveryBackoffMax: formatDuration(config.providerWeb.recoveryBackoffMax),
    },
    batch: { ...config.batch, randomDelay: `${config.batch.randomDelay}ms` },
    media: {
      maxImageBytes: byteSizeBytes(config.media.maxImageSize), maxTotalBytes: byteSizeBytes(config.media.maxTotalSize),
      cleanupThresholdPercent: config.media.cleanupThresholdPercent,
      cleanupInterval: formatDuration(config.media.cleanupInterval),
    },
    routing: {
      stickyTTL: formatDuration(config.routing.stickyTTL), cooldownBase: formatDuration(config.routing.cooldownBase),
      cooldownMax: formatDuration(config.routing.cooldownMax), maxAttempts: config.routing.maxAttempts,
    },
    audit: { bufferSize: config.audit.bufferSize, batchSize: config.audit.batchSize, flushInterval: formatDuration(config.audit.flushInterval) },
    clientKeyDefaults: config.clientKeyDefaults,
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
