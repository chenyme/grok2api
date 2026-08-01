# Immediate egress failure probe and bounded retry

This mechanism reduces long retry stalls after a fixed proxy has a transient transport failure. It is separate from model-quality monitoring and applies only to failures known to occur before a request was submitted upstream.

## Request flow

1. A fixed egress node reports a transport error such as connection refusal, reset, timeout, or unexpected EOF.
2. The node enters the existing exponential cooldown and its cached transport is invalidated.
3. After the cooldown state is persisted, the manager schedules an immediate connectivity probe for that node.
4. Concurrent failures for the same node join the running probe instead of starting duplicate probes.
5. A subsequent request explicitly bound to that cooling node waits up to five seconds for the probe.
6. A healthy probe atomically clears only transport-failure health, failure-count, and cooldown fields. The waiting request reloads the node and continues immediately.
7. An unhealthy or failed probe leaves the cooldown in place, so the waiting request returns the normal unavailable result instead of retrying a dead route.

The probe runs with an independent 20-second background timeout. Cancellation of the user request stops only that request's wait; it does not cancel the shared probe. A five-second completion grace closes the race where a retry arrives immediately after the probe finishes.

## Fixed nodes and proxy pools

Fixed nodes represent one stable proxy endpoint. A transport failure can therefore describe the node, and a successful independent probe is meaningful evidence that it recovered.

Proxy-pool nodes represent request-level or identity-level rotating exits. One failed tunnel does not prove that the whole pool is unhealthy. Pool leases request a fresh tunnel, ignore stale global cooldown state, and do not schedule a node-wide failure probe after one request-level transport error. Proxy URLs containing `{account}` use the same pool behavior automatically.

## Safety invariants

- Never replays a request after it may have been submitted upstream.
- Does not treat authentication failures, quota exhaustion, rate limits, or ordinary upstream HTTP failures as proxy transport failures.
- Persists cooldown before scheduling the recovery probe.
- Coalesces probes per node to prevent a failure burst from causing a probe burst.
- Re-reads node state after probe completion instead of trusting stale in-memory state.
- Uses an expected encrypted proxy value when persisting probe results, so a probe cannot overwrite an operator's concurrent proxy change.
- Clears only cooldowns whose `last_error` is exactly the transport-failure marker. Anti-bot and operator state remain intact.
- Honors request cancellation and bounds every wait.
- Logs node metadata and probe outcome without proxy URLs or credentials.

## Implementation map

- `manager.go`: failure classification, cooldown, probe scheduling, coalescing, bounded waiting, and retry acquisition.
- `application/egress/operations.go`: real connectivity probe and persistence.
- `relational/egress_repository.go`: compare-and-update persistence and transport-only cooldown recovery.
- `application/app/application.go`: wires the egress service probe callback into the manager.

## Test coverage

`manager_test.go` covers pool isolation, fixed-node cooldown, probe coalescing, healthy recovery, unhealthy recovery, and request cancellation. Repository and operations tests cover stale-probe rejection and transport-only state clearing.
