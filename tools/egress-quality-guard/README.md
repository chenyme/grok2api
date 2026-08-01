# Egress Quality Guard

Egress Quality Guard combines passive production-audit monitoring with active
Grok Build probes through each fixed egress node. A passive hard-threshold
anomaly quarantines the node immediately; a soft anomaly is confirmed by a
fixed active probe.

This is a heuristic circuit breaker, not a model-quality oracle. Very high
reported throughput can be caused by upstream or intermediary buffering. Start
in an observation environment, inspect the JSON logs, and tune thresholds from
your own traffic before allowing automatic quarantine.

## Scope and prerequisites

- Supports Grok Build streaming requests after egress nodes and request audits are configured in grok2api.
- Every managed node should have accounts capable of serving the probe model explicitly assigned to it.
- Requires a dedicated probe client key and internal access to the administrator API.
- Classification is heuristic evidence. It cannot prove that upstream model capability changed and does not replace application-level regression tests.

## How it works

1. Passive mode polls recent successful streaming audits and computes the same
   speed shown by the grok2api panel: `output / (duration - first token)`.
   `output` intentionally includes reasoning tokens.
2. Active mode calls the administrator-only
   `POST /api/admin/v1/egress-nodes/{id}/quality-test` endpoint.
3. grok2api prefers an account bound to that node. If none is schedulable, it
   borrows any healthy account while still forcing the physical request through
   the node under test, then sends a fixed streaming prompt.
4. The fixed probe checks output tokens, chunk cadence, first-token time,
   instruction-marker compliance, and panel-equivalent output-token throughput.
5. A high-TPS production request at the hard threshold quarantines the node
   immediately. A soft result triggers a fixed-prompt probe; active hard results
   quarantine immediately, while active soft results must reach the configured
   strike count.
6. Quarantined nodes remain available only to administrator probes. Recovery
   records a generic connectivity probe for diagnosis, then uses the real
   model-quality probe as the authority before re-enabling the node.

The public inference API cannot request a specific egress node or bypass a
disabled node. This capability is confined to the authenticated admin route.

## Operating modes

`QUALITY_GUARD_MODE` accepts:

- `passive`: inspect ordinary request audits every few seconds. Routine polling
  adds no model requests. Hard anomalies quarantine immediately; soft anomalies
  trigger one active confirmation probe. Recovery probes still run for nodes
  quarantined by the guard.
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

With `QUALITY_GUARD_FAIL_CLOSED=true`, soft, hard, and indeterminate samples
leave scheduling before confirmation. The minimum healthy-node floor no longer
suppresses quarantine. A short buffered burst is first retested on the same IP
and is restored immediately when that one real-model probe is healthy, avoiding
an unnecessary rotation for a measurement artifact.

`QUALITY_GUARD_ROTATION_URL` enables a trusted internal rotation webhook scoped
by `QUALITY_GUARD_ROTATABLE_NODE_IDS`. Confirmed suspect nodes are rotated, the
exit-IP change is verified by the webhook, and one real-model quality probe must
pass before restoration. The optional `session_rotator.py` implements this
contract for 1024Proxy-style usernames containing `sid-...-t-...`.

Probe failures require `QUALITY_GUARD_CONSECUTIVE_ERRORS` consecutive attempts
before quarantine. Account-selection failures are reported separately: if the
entire Grok Build pool has no schedulable account, the guard backs off for
`QUALITY_GUARD_NO_ACCOUNT_BACKOFF_SECONDS` and suppresses duplicate logs without
counting a proxy failure or rotating the IP. The node remains isolated until a
real model-quality probe can pass.

## Admin UI

The admin sidebar includes a Quality guard page showing service freshness, mode, per-node panel-equivalent output Token/s, time to first token, strike counts, quarantine state, and recent events. Operators can also run one real model quality test against a selected node.

The page also reports cumulative automatic checks, active probes, passive audits, anomaly hits, quarantine and recovery actions, and output tokens produced by active probes since statistics were enabled. Those output counts include reasoning tokens. Manual tests are excluded. Actual proxy transfer bytes cannot be recovered reliably from HTTPS/SSE request audits, so Token counts are not presented as network traffic.

The main service exposes only sanitized state and writes editable policy fields only to a separate runtime config file. In Docker, mount the same state volume into the main service and set:

```yaml
services:
  grok2api:
    environment:
      QUALITY_GUARD_STATE_FILE: /var/lib/grok2api-quality-guard/state.json
      QUALITY_GUARD_RUNTIME_CONFIG_FILE: /var/lib/grok2api-quality-guard/runtime-config.json
    volumes:
      - quality_guard_state:/var/lib/grok2api-quality-guard

  egress-quality-guard:
    volumes:
      - quality_guard_state:/var/lib/grok2api-quality-guard
```

If the state path is not configured, the page reports that the guard is not connected. If the runtime config path is omitted, policy editing stays read-only. Saved policy changes are hot-reloaded in about one second without restarting containers. The administrator-authenticated endpoints never return the admin password, client key secret, proxy URL, probe prompt, or model response body.

## Safety properties

- Never deletes a node or changes account bindings.
- Never restores a node disabled by an operator.
- Refuses to quarantine below `QUALITY_GUARD_MIN_HEALTHY_NODES`.
- Strict mode overrides that floor rather than scheduling an unverified exit.
- Uses an exclusive process lock to prevent duplicate guards.
- Writes state atomically with mode `0600`.
- Logs metrics and node metadata, never credentials, proxy URLs, or response text.
- Keeps the administrator access token in memory only.

## Configuration

Copy `egress-quality-guard.env.example` to a private deployment location and
set its mode to `0600`. Create a dedicated client key with access to the probe
model, unlimited local billing, and enough RPM/concurrency for the configured
interval.

The required settings are:

| Variable | Purpose |
| --- | --- |
| `GROK2API_BASE_URL` | grok2api URL reachable by the guard |
| `GROK2API_ADMIN_USERNAME` | administrator login |
| `GROK2API_ADMIN_PASSWORD` | administrator password |
| `QUALITY_GUARD_CLIENT_KEY_ID` | numeric ID of a dedicated probe client key |
| `QUALITY_GUARD_MODEL` | public Build model ID |

`QUALITY_GUARD_NODE_IDS` is optional. When empty, the guard monitors every
enabled proxied Build node plus nodes previously quarantined by this guard.
`GROK2API_ADMIN_PASSWORD_FILE` can replace the password environment variable
when the runtime supports mounted secrets.

Default hybrid policy:

- inspect ordinary request audits every 5 seconds;
- run active per-node probes every 1,800 seconds, with up to 30 seconds of jitter;
- quarantine immediately at 1000 visible tokens/second;
- require two consecutive observations at 500 tokens/second;
- require two consecutive probe errors;
- quarantine for 300 seconds;
- retain at least three enabled proxied Build nodes.

Five nodes probed every 30 minutes produce 240 model requests per day. Passive
monitoring adds database reads but no model tokens or residential inference
traffic. Choose a longer active interval when upstream quota is limited.

## Docker Compose quick start

Run from the repository root:

```sh
sudo install -m 0600 \
  tools/egress-quality-guard/egress-quality-guard.env.example \
  /etc/grok2api-egress-quality-guard.env
sudo editor /etc/grok2api-egress-quality-guard.env

docker compose \
  -f docker-compose.yml \
  -f tools/egress-quality-guard/compose.override.example.yml \
  config --quiet
docker compose \
  -f docker-compose.yml \
  -f tools/egress-quality-guard/compose.override.example.yml \
  up -d --build grok2api egress-quality-guard
```

Verify the managed nodes, dedicated client key, model, and minimum healthy-node count before leaving the sidecar running. Never commit the private environment file, state volume, or production logs.

## Known limitations

- HTTPS/SSE audits cannot provide reliable proxy transfer-byte counts. The UI reports active-probe output Tokens and does not label them as network traffic.
- Intermediary buffering can produce unusually high instantaneous Token/s, so thresholds require calibration for each route.
- Passive monitoring processes only complete successful streaming requests with enough output to calculate speed. Short and failed requests are ignored.
- A real request may legitimately return cached content, an existing file, or a long constant. Immediate quarantine at the passive hard threshold is therefore intentionally aggressive; raise `hard_tps` when false positives are more costly. Soft anomalies still require a fixed-prompt confirmation.
- The first run establishes an audit baseline. Cumulative statistics also begin when this version first writes state.
- Manual quality tests are diagnostic. They are excluded from automatic statistics and do not directly change quarantine state.

## Run

Validate configuration and execute one cycle:

```sh
set -a
. /etc/grok2api-egress-quality-guard.env
set +a
python3 quality_guard.py --check-config
python3 quality_guard.py --once
```

For systemd, install the script under
`/opt/grok2api-egress-quality-guard/`, create the unprivileged
`grok-quality-guard` user, create `/var/lib/grok2api-quality-guard` owned by
that user, and install `grok2api-egress-quality-guard.service`.

Container example:

```yaml
services:
  egress-quality-guard:
    build:
      context: .
      dockerfile: tools/egress-quality-guard/Dockerfile
    env_file:
      - /etc/grok2api-egress-quality-guard.env
    volumes:
      - quality_guard_state:/var/lib/grok2api-quality-guard
    restart: unless-stopped

volumes:
  quality_guard_state:
```

See [`SECURITY.md`](./SECURITY.md) before deploying the guard outside a development environment.

## Tests

```sh
python3 -m unittest -v tools/egress-quality-guard/quality_guard_test.py
```

The tests cover active and passive threshold classification, audit baselining
and self-exclusion, minimum healthy-node protection, quarantine and recovery,
configuration validation, and private atomic state.
