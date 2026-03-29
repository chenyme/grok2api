# Repository Guidelines

## Project Structure & Module Organization
`main.py` boots the FastAPI app and lifecycle hooks. `app/api/v1` contains API endpoints, while `app/api/pages` serves page routes. Core infrastructure lives in `app/core` (config, auth, logging, storage, middleware). Business logic is grouped under `app/services`, mainly `grok`, `reverse`, `token`, and `cf_refresh`. Static admin and feature-page assets live in `_public/static`. Focused regression tests live in `tests/`. Deployment files are kept at the root: `Dockerfile`, `docker-compose.yml`, `render.yaml`, and `vercel.json`.

## Build, Test, and Development Commands
- `uv sync`: install locked dependencies for local development.
- `uv run granian --interface asgi --host 0.0.0.0 --port 8000 --workers 1 main:app`: run the service locally. Do not use `python main.py`; it exits by design.
- `uv run ruff check .`: lint Python code and catch import/style issues.
- `docker compose up -d`: start the containerized stack with local `data/` and `logs/` mounts.
- `uv run pytest tests/test_openai_usage.py`: run the current regression test file after installing `pytest` in your local dev environment.

## Coding Style & Naming Conventions
Target Python 3.13 and use 4-space indentation. Prefer small, explicit async functions and keep service boundaries clear. Use `snake_case` for modules, functions, and config keys, `PascalCase` for classes, and align route/service names when possible, for example `app/api/v1/chat.py` with `app/services/grok/services/chat.py`. Keep frontend assets grouped by feature under `_public/static/function` or `_public/static/admin`; use kebab-case for asset filenames.

## Testing Guidelines
Add tests in `tests/` using `test_*.py` names. Favor focused regression tests around SSE streaming, usage accounting, protocol adaptation, and storage/config edge cases. Mock external Grok or reverse-service calls instead of hitting live endpoints. For behavior changes, verify at least one local request flow such as `POST /v1/chat/completions`.

## Commit & Pull Request Guidelines
Use Conventional Commits. Recent history includes `fix: add estimated openai usage stats` and `refactor: update request overrides handling in ImageEditService`. PR titles are validated with the same format, optionally scoped like `feat(api): ...`. Use `.github/pull_request_template.md` and fill `## Summary`, `## Changes`, and `## Verification`; at least one checkbox in `Changes` and `Verification` must be checked. Link related issues when relevant and include screenshots for `_public` UI changes.

## Security & Configuration Tips
Keep secrets in `.env`; never commit tokens, cookies, or `cf_clearance` values. When adding config, update `config.defaults.toml` first and document any new environment variables in `readme.md`. CI runs gitleaks, pip-audit, and CodeQL, so secret handling and dependency hygiene must stay clean.
