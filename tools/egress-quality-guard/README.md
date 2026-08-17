# Egress Quality Guard

Egress Quality Guard combines passive production-audit monitoring with active
Grok Build probes through each fixed egress node. A complete production request
at either passive threshold quarantines the node immediately and holds it until
the fixed recovery probe is healthy.

This is a heuristic circuit breaker, not a model-quality oracle. Very high
reported throughput can be caused by upstream or intermediary buffering. Start
in an observation environment, inspect the JSON logs, and tune thresholds from
your own traffic before allowing automatic quarantine.

## Scope and prerequisites

- Supports Grok Build streaming requests after egress nodes and request audits are configured in grok2api.
- At least one schedulable Grok Build account must be able to serve the probe model. The account does not have to be bound to every managed node.
- The built-in thinking guard is enforced only when the backend recognizes the configured Build model as reasoning-capable. Keep the default `grok-4.5` or another verified reasoning model when missing-thinking detection is required; unknown and non-reasoning models retain marker/TPS checks without this signal.
- The main service automatically provisions a non-exportable system probe identity. The sidecar reaches only a scoped internal API over the Compose network.
- Classification is heuristic evidence. It cannot prove that upstream model capability changed and does not replace application-level regression tests.

## How it works

1. Passive mode polls recent successful streaming audits and computes the same
   speed shown by the grok2api panel: `output / generation window`.
   `output` includes reasoning tokens. The window is `duration - first token`,
   except when that tail is both shorter than the first-token wait and under
   1s: then the full duration is used so buffered thinking is not assigned to
   a few milliseconds.
2. Active mode calls a quality-guard-only internal probe endpoint. The scoped
   credential cannot access account exports, administrator management, or the
   rest of the administrator API.
3. grok2api prefers an account bound to that node. If none is schedulable, it
   borrows any healthy account while still forcing the physical request through
   the node under test, then sends a fixed streaming prompt. The backend pins
   the route to Grok Build even when another provider exposes the same public
   model name.
4. The fixed probe checks output tokens, chunk cadence, first-token time,
   instruction-marker compliance, and panel-equivalent output-token throughput.
5. A production request at either passive TPS threshold quarantines the node
   immediately and starts the hold. Active hard results quarantine immediately;
   active soft results must reach the configured strike count.
6. Quarantined nodes remain available only to administrator probes. Recovery
   records a generic connectivity probe for diagnosis, then uses the real
   model-quality probe as the authority before re-enabling the node.

The public inference API cannot request a specific egress node or bypass a
disabled node. This capability is confined to the authenticated internal route.
Ambiguous probe-only 403 responses do not cool borrowed accounts; definitive
credential, account-block, and quota signals retain their normal transitions.

## Operating modes

`qualityGuard.mode` accepts:

- `passive`: inspect ordinary request audits every few seconds. Routine polling
  adds no model requests. Soft and hard anomalies quarantine immediately;
  recovery probes run only after the hold for nodes quarantined by the guard.
- `active`: run only fixed per-node probes at the configured interval.
- `hybrid`: enable both detectors. This is the recommended default.

Passive monitoring ignores non-streaming requests, failed requests, responses
with fewer than 32 output tokens, and audits created by the guard's own client
key. On first startup it records a baseline without replaying historical
anomalies. Cursor pagination and a persistent bounded ID set prevent duplicate
processing across polls and restarts.

Generic IP/Cloudflare probes are intentionally not recovery gates: some
residential exits can reach Grok normally while a probe endpoint is blocked.
The model-quality request is the authoritative recovery signal.

## Strict quarantine and IP rotation

With `qualityGuard.failClosed: true`, soft, hard, and indeterminate samples
leave scheduling before confirmation. The minimum healthy-node floor no longer
suppresses quarantine. A short buffered burst is first retested on the same IP
and is restored immediately when that one real-model probe is healthy, avoiding
an unnecessary rotation for a measurement artifact.

`qualityGuard.rotationURL` enables a trusted internal rotation webhook scoped
by `qualityGuard.rotatableNodeIDs`. Confirmed suspect nodes are rotated, the
exit-IP change is verified by the webhook, and one real-model quality probe must
pass before restoration. The optional `session_rotator.py` implements this
contract for 1024Proxy-style usernames containing `sid-...-t-...`.

Probe failures require `qualityGuard.consecutiveErrors` consecutive attempts
before quarantine. Account-selection failures are reported separately: if the
entire Grok Build pool has no schedulable account, the guard backs off for
`qualityGuard.noAccountBackoff` and suppresses duplicate logs without
counting a proxy failure or rotating the IP. The node remains isolated until a
real model-quality probe can pass.

## Admin UI

The admin sidebar includes a Quality guard page showing service freshness, mode, per-node panel-equivalent output Token/s, time to first token, strike counts, quarantine state, and recent events. Operators can also run one real model quality test against a selected node.

Nodes referenced by a fixed egress fallback policy cannot be disabled without
making that policy invalid. The guard discovers and excludes them, and the UI
marks them as protected. Remove or change the fixed fallback policy before
placing such a node under automatic quarantine management.

The page also reports cumulative automatic checks, active probes, passive audits, anomaly hits, quarantine and recovery actions, and output tokens produced by active probes since statistics were enabled. Those output counts include reasoning tokens. Manual tests are excluded. Actual proxy transfer bytes cannot be recovered reliably from HTTPS/SSE request audits, so Token counts are not presented as network traffic.

The main Compose file owns the private shared state volume. grok2api writes a
versioned bootstrap file containing normalized `config.yaml` settings and a
derived, quality-guard-only credential; the sidecar never reads or stores the
administrator password. Saved policy changes from the admin UI are hot-reloaded
in about one second without restarting containers. Public and administrator
responses never return the internal credential, client-key secret, proxy URL,
probe prompt, or model response body.

## Safety properties

- Never deletes a node or changes account bindings.
- Never restores a node disabled by an operator.
- Refuses to quarantine below `qualityGuard.minimumHealthyNodes`.
- Strict mode overrides that floor rather than scheduling an unverified exit.
- Uses an exclusive process lock to prevent duplicate guards.
- Writes state atomically with mode `0600`.
- Logs metrics and node metadata, never credentials, proxy URLs, or response text.
- Uses a constant-time-checked internal credential scoped to six egress/audit routes.

## Configuration

All operator-owned settings live in the main `config.yaml`. The main service
automatically creates and reuses a hidden, Build-only system identity for probe
authorization, accounting, and audit attribution; operators never create,
copy, select, or configure a Client Key for the guard:

```yaml
qualityGuard:
  enabled: true
  model: "grok-4.5"
  mode: hybrid
  activeInterval: 30m
  passivePollInterval: 5s
  softTPS: 500
  hardTPS: 1000
  consecutiveSoft: 2
  consecutiveErrors: 2
  quarantineDuration: 5m
  noAccountBackoff: 5m
  minimumHealthyNodes: 3
  failClosed: false
  nodeIDs: []
```

The legacy preview `clientKeyID` field is accepted but ignored and can be
removed. Upgrades intentionally do not delete an operator-created key because
it may still serve another workload.

An empty `nodeIDs` list discovers every enabled proxied Build node plus nodes
previously quarantined by this guard. Discovery follows all API pages; fixed
fallback nodes are reported as protected and excluded from automatic quarantine.
Advanced rotation fields are documented in `config.example.yaml`.

Default hybrid policy:

- inspect ordinary request audits every 5 seconds;
- run active per-node probes every 1,800 seconds, with up to 30 seconds of jitter;
- quarantine immediately at 1000 visible tokens/second;
- require two consecutive active-probe observations at 500 tokens/second;
- require two consecutive probe errors;
- quarantine for 300 seconds;
- retain at least three enabled proxied Build nodes.

Five nodes probed every 30 minutes produce 240 model requests per day. Passive
monitoring adds database reads but no model tokens or residential inference
traffic. Choose a longer active interval when upstream quota is limited.

## Docker Compose quick start

The repository's main `docker-compose.yml` includes the sidecar behind the
optional `quality-guard` profile. A normal `docker compose up -d` does not start
it or generate probe traffic.

Run from the repository root:

```sh
docker compose --profile quality-guard config --quiet
docker compose --profile quality-guard up -d --build
```

After changing the base `qualityGuard` settings in `config.yaml`, run
`docker compose --profile quality-guard restart grok2api egress-quality-guard`
so the main service regenerates the bootstrap. Policy changes saved in the
admin page still hot-reload without a restart.

Verify the managed nodes, model, and minimum healthy-node count before leaving
the sidecar running. Never commit the state volume or
production logs.
Stop only the guard with
`docker compose --profile quality-guard stop egress-quality-guard`; the main
API remains available.

## Known limitations

- HTTPS/SSE audits cannot provide reliable proxy transfer-byte counts. The UI reports active-probe output Tokens and does not label them as network traffic.
- Intermediary buffering can produce unusually high instantaneous Token/s, so thresholds require calibration for each route.
- Passive monitoring processes only complete successful streaming requests with enough output to calculate speed. Short and failed requests are ignored.
- A real request may legitimately return cached content, an existing file, or a long constant. Passive soft and hard anomalies therefore isolate immediately and hold the node until a controlled recovery probe succeeds; raise the thresholds when false positives are more costly.
- Missing-thinking detection applies only to controlled probe profiles that explicitly require thinking. Arbitrary user traffic may legitimately use a non-reasoning model or disable reasoning and is never isolated solely because `reasoningTokens` is zero.
- The first run establishes an audit baseline. Cumulative statistics also begin when this version first writes state.
- Manual quality tests are diagnostic. They are excluded from automatic statistics and do not directly change quarantine state.

See [`SECURITY.md`](./SECURITY.md) before deploying the guard outside a development environment.

## Tests

```sh
python3 -m unittest -v tools/egress-quality-guard/quality_guard_test.py tools/egress-quality-guard/session_rotator_test.py
```

The tests cover active and passive threshold classification, complete node
pagination, audit baselining and self-exclusion, fixed-fallback protection,
crash-safe quarantine ownership, quarantine and recovery, live exit-IP rotation
verification, configuration validation, and private atomic state.
