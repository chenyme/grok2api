export type ModelRouteDTO = {
  id: string;
  publicId: string;
  provider: "grok_build" | "grok_web" | "grok_console";
  upstreamModel: string;
  capability: "responses" | "chat" | "image" | "image_edit" | "video";
  origin: "catalog" | "discovered" | "manual";
  enabled: boolean;
  accountIds: string[];
  bindingMode: boolean;
  supportedAccounts: number;
  syncedAccounts: number;
  totalAccounts: number;
  capabilityKnown: boolean;
  available: boolean;
  lastSyncedAt?: string;
};
