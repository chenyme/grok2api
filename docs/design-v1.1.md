# Grok2API-Ex v1.1 Backend Design

## Scope

v1.1 adds the P0 image-generation backend surface for free/basic accounts:

- `POST /v1/images/generations` routes `grok-imagine-image-lite` requests through the Grok web chat image path.
- `POST /v1/responses` supports the same image generation path when the requested model is an image model or the request includes an `image_generation` tool.
- OpenAI-style `prompt`, `model`, `size`, `n`, and `response_format` are accepted, with `aspect_ratio` accepted as an x.ai-style extension.

## Parameter Mapping

| OpenAI-compatible field | Internal mapping |
| --- | --- |
| `prompt` | Sent as the Grok image prompt. |
| `model` | Resolved through the model registry; free/basic accounts should use `grok-imagine-image-lite`. |
| `size` | Validated and mapped to aspect ratio: `1280x720 -> 16:9`, `720x1280 -> 9:16`, `1792x1024 -> 3:2`, `1024x1792 -> 2:3`, `1024x1024 -> 1:1`. |
| `aspect_ratio` | Optional direct override; accepts `16:9`, `9:16`, `3:2`, `2:3`, `1:1`. |
| `n` | `grok-imagine-image-lite` allows `1-4`; higher-tier image models keep the existing `1-10` validation. |
| `response_format` | `url` returns a local `/v1/files/image?id=...` proxy/cache URL; `b64_json` returns raw base64. |

## Local Cache URLs

Image generation now forces generated `url` responses through local media storage before returning them. This avoids returning temporary x.ai/Grok asset URLs that can expire. If `app.app_url` is configured, the returned URL is absolute; otherwise it is a relative `/v1/files/image?id=...` URL served by this gateway.

## Errors

The router checks account-tier capability before dispatch:

- Requesting super/heavy-only image, edit, or video models with only free/basic accounts returns `model_not_available`.
- Exhausted or cooling free/basic accounts return a 429 `rate_limit_exceeded` message that explicitly calls out `grok-imagine-image-lite` quota/account availability.
- Upstream 401/403/429 failures from the lite image path are normalized into clear permission/session/quota messages.

## Responses Compatibility

`/v1/responses` image generation returns a standard Responses object with `image_generation_call` output items. For `response_format=url`, each item includes `result` and `url`. For `response_format=b64_json`, each item includes `result` containing the base64 payload.

## Admin Image Entry

`/admin/images` serves the existing static image-generation page inside the Admin shell and uses `/admin/api/images/generations` with the Admin key. Single-user LAN deployments should use `/admin` as the unified entry.

Legacy WebUI page entrypoints are compatibility redirects: `/webui` redirects to `/admin`, `/webui/login` redirects to `/admin/login`, and `/webui/images` redirects to `/admin/images`. The `/webui/api/images/generations` wrapper remains available for older scripted callers that still use the WebUI API key. The prompt textarea spans the form width, supports multi-line text, and remains vertically resizable.
