# PR: Inject native web_search / x_search for free Build prompt cache

## Summary

Free Grok Build OAuth traffic on `cli-chat-proxy` often reports
`usage.input_tokens_details.cached_tokens = 0` even when:

- the client uses OpenAI **Responses**
- `prompt_cache_key` / sticky account affinity is stable
- multi-turn prefixes are byte-stable

Empirically, declaring native tools unlocks a cache-capable free path:

```json
{"type": "web_search"}
{"type": "x_search"}
```

This matches CLIProxyAPI issue discussion (#4213) and the
`inject-build-search-tools` approach. This PR adds an equivalent opt-out switch
to grok2api Build provider (default **on**), with admin Web UI + settings API.

## Changes

| Area | Detail |
|------|--------|
| Config | `provider.build.injectBuildSearchTools` (default `true`), `hideInjectedSearchResults` (reserved, default `false`) |
| CLI adapter | Before `prompt_cache_key` injection, append missing native tools |
| Settings domain / admin API | Hot-reload fields on Provider Build |
| Web UI | Settings → Grok Build toggles (zh/en) |
| Tests | Unit tests for inject/dedupe |
| Repro | `scripts/repro_build_prompt_cache.py` |

`hideInjectedSearchResults` is wired end-to-end but **does not yet filter**
upstream search output (reserved for a follow-up if needed). In practice free
turns often set `num_server_side_tools_used: 0` even with tools declared.

## Test plan

### A. Unit

```bash
cd backend
go test ./internal/infra/provider/cli/ -count=1 -run 'TestInjectBuildSearchTools'
```

### B. Live free Build A/B (no client tools)

Requires at least one usable free Build account and a client key.

```bash
export GROK2API_BASE=http://127.0.0.1:8000
export GROK2API_KEY=g2a_xxx
export GROK2API_MODEL=grok-4.5

# With inject enabled (default): turn 2 should show large cached_tokens
python3 scripts/repro_build_prompt_cache.py --mode baseline --turns 2

# Client-provided native tools: also caches (inject may no-op due to dedupe)
python3 scripts/repro_build_prompt_cache.py --mode native --turns 2

# Only function tools + inject disabled in admin: expect cached_tokens stay ~0
# (toggle inject off in Web UI / settings, save, then:)
python3 scripts/repro_build_prompt_cache.py --mode function --turns 2 --allow-zero
```

### C. Recorded production results (2026-07-21, free pool)

Gateway: fork image `ghcr.io/sycghj/grok2api` with inject default on.

**Before (historical audit, no native tools):**

| provider    | requests | input_tokens | cached_input_tokens |
|-------------|----------|--------------|---------------------|
| grok_build  | 270      | ~15.9M       | **0**               |
| grok_console| 19       | ~0.28M       | ~112k (~40%)        |

Direct two-turn Responses, fixed `prompt_cache_key`, sticky same account,
**no tools**:

| turn | input | cached |
|------|-------|--------|
| 1    | 10608 | 0      |
| 2    | 10624 | 0      |

Direct two-turn with client `web_search` + `x_search`:

| turn | input | cached |
|------|-------|--------|
| 1    | 12552 | 128    |
| 2    | 12573 | 12544 (~99.8%) |

After deploy of inject default (client **baseline**, no tools in body):

| turn | model                 | input | cached |
|------|-----------------------|-------|--------|
| 1    | grok-4.5-build-free   | 4149  | 128    |
| 2    | grok-4.5-build-free   | 4149  | **4096** |

### D. Web UI

1. Open Admin → Settings → Providers → Grok Build.
2. Confirm **Inject native search tools (prompt cache)** is on by default.
3. Toggle off, Save, re-run baseline repro → expect near-zero cache on free.
4. Toggle on, Save, re-run baseline → expect turn-2 cache hit.

## Config surface

YAML (optional; defaults apply when omitted):

```yaml
provider:
  build:
    injectBuildSearchTools: true
    hideInjectedSearchResults: false
```

Admin JSON field names:

- `providerBuild.injectBuildSearchTools`
- `providerBuild.hideInjectedSearchResults`

## Notes / non-goals

- Does **not** implement Anthropic `cache_control` for Grok (xAI uses prefix +
  session affinity, not Claude-style explicit cache blocks).
- Does **not** change sticky routing semantics.
- Paid / official `api.x.ai` paths can disable the inject if native tools are
  undesirable.
- Console provider already showed non-zero cache in audits; this PR targets
  **Build free** path.

## Related

- CLIProxyAPI #4213 / #4214 (`inject-build-search-tools`, session affinity on `prompt_cache_key`)
- grok2api `resolveBuildSessionIdentity` / sticky session (already present; insufficient alone on free)
