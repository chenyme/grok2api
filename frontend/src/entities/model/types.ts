export type ModelRouteDTO = {
  id: string;
  publicId: string;
  provider: "grok_build" | "grok_web";
  upstreamModel: string;
  capability: "responses" | "chat" | "image" | "image_edit" | "video";
  enabled: boolean;
  supportedAccounts: number;
  syncedAccounts: number;
  totalAccounts: number;
  capabilityKnown: boolean;
  available: boolean;
  lastSyncedAt?: string;
};
