export {};

declare global {
  const __GROK2API_DEV_API_TARGET__: string;

  interface Window {
    __GROK2API_RUNTIME_CONFIG__?: {
      apiBaseUrl?: string;
      publicApiBaseUrl?: string;
    };
  }
}
