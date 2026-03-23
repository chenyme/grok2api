# Repository Guidelines

## Project Structure & Architecture
`main.py` boots the FastAPI app, loads `.env`, mounts `/_public/static`, and wires lifecycle tasks. Core framework code lives in `app/core/` (`config.py`, `storage.py`, `auth.py`, logging, exceptions). HTTP entrypoints live in `app/api/`: `v1/` exposes OpenAI-compatible routes (`/chat/completions`, `/responses`, `/images/*`, `/videos`, `/models`, `/v1/files/*`), while `pages/` serves the admin and function UIs from `_public/static/`. Business logic lives in `app/services/`: `grok/services/` handles chat, image, video, and responses flows; `reverse/` contains low-level Grok HTTP/WebSocket/grpc-web adapters; `token/` manages pool selection and refresh; `cf_refresh/` updates Cloudflare session config. Deployment assets are in `Dockerfile`, `docker-compose.yml`, `render.yaml`, and `scripts/`.

## Build, Test, and Development Commands
Use `uv` for local setup and never start with `python main.py`.

- `uv sync`: install runtime and dev dependencies into `.venv`.
- `uv run granian --interface asgi --host 0.0.0.0 --port 8000 --workers 1 main:app`: run the API locally.
- `uv run ruff check .`: lint Python before opening a PR.
- `docker compose up -d`: run the packaged container stack.
- `docker build -t grok2api .`: build the production image locally.

## Coding Style & Naming Conventions
Target Python 3.13, 4-space indentation, `snake_case` for functions/modules, `PascalCase` for Pydantic models and service classes. Keep routers thin: validate request shape in `app/api/...`, then delegate behavior to `app/services/...`. Reuse `app/core/config.py` and `app/core/storage.py` instead of introducing ad-hoc env or file reads. Prefer extending existing reverse adapters and token-pool logic over duplicating request code.

## Testing & Verification
There is no first-party `tests/` suite in the repo today. For every change, run `uv run ruff check .` and manually verify the affected API or page with `curl`, the admin UI (`/admin`), or function pages (`/chat`, `/imagine`, `/video`, `/voice`). If you add non-trivial logic, create focused automated tests under `tests/`.

## Commit & Pull Request Guidelines
Recent history follows Conventional Commits: `feat:`, `fix:`, `refactor:`, `chore:`, plus optional scopes such as `fix(token): ...`. Keep commits narrow and descriptive. PR titles must also follow Conventional Commits. The PR body must include `## Summary`, `## Changes`, and `## Verification`, and both `## Changes` and `## Verification` need checked checklist items. Link issues when relevant and include screenshots for `_public/static/` UI work.

## Security & Configuration Tips
Treat `.env`, `data/config.toml`, `data/token.json`, proxy settings, and `cf_clearance` values as secrets; never commit real tokens or cookies. Prefer `SERVER_STORAGE_TYPE=redis|mysql|pgsql` plus `SERVER_STORAGE_URL` for persistent deployments. When changing auth, token refresh, or proxy behavior, verify both API routes and admin/function access paths.
