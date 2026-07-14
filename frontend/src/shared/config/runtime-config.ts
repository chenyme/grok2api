const apiBaseUrl = window.__GROK2API_RUNTIME_CONFIG__?.apiBaseUrl?.replace(/\/$/, "") ?? "";
const configuredPublicApiBaseUrl = window.__GROK2API_RUNTIME_CONFIG__?.publicApiBaseUrl?.replace(/\/$/, "") ?? "";
const developmentApiBaseUrl = typeof __GROK2API_DEV_API_TARGET__ === "string" ? __GROK2API_DEV_API_TARGET__.replace(/\/$/, "") : "";

export const runtimeConfig = {
  apiBaseUrl,
  publicApiBaseUrl: configuredPublicApiBaseUrl || apiBaseUrl || developmentApiBaseUrl || window.location.origin,
};
