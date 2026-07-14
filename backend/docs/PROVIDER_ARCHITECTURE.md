# Provider Architecture

## Stable Boundary

The service has three fixed upstream Providers:

| Provider | Internal route prefix | Model catalog | Credential lifecycle | Quota authority |
| --- | --- | --- | --- | --- |
| `grok_build` | `Build/` | Remote discovery per account | OAuth import, Device OAuth, refresh | Billing |
| `grok_web` | `Web/` | Built-in catalog filtered by account tier | SSO import | Remote quota windows |
| `grok_console` | `Console/` | Built-in catalog | SSO import | Local quota window |

`infra/provider.Definition` is the authoritative declaration of each Provider's capabilities. `Registry.Validate` runs during application startup and fails fast when a declaration does not match the Adapter's implemented small interfaces. Application services should read the Definition or request a capability from the Registry instead of branching on Provider names.

The Definition also owns lifecycle and gateway policy: credential refresh support, quota authority, upstream-versus-estimated usage, forbidden-response egress retry, conversation state, compaction, and media surfaces. These policies are intentionally declarative so the shared account and gateway services do not encode Build/Web/Console behavior by name.

## Ownership

- `domain/account` owns stable Provider identities and model namespaces.
- `domain/model` owns client-facing model names, Provider-qualified internal route IDs, and their normalization boundary.
- `infra/provider/cli` owns Grok Build upstream HTTP, OAuth, model discovery, Billing, and Responses compatibility.
- `infra/provider/web` owns Grok Web SSO, quota, app-chat, image, video, and Statsig/egress behavior.
- `infra/provider/console` owns Console SSO, its static model catalog, local quota policy, and stateless Responses transport.
- `application/account` orchestrates credential and quota maintenance without knowing upstream wire formats.
- `application/model` orchestrates account capability synchronization and route persistence from Registry definitions.
- `application/gateway` resolves a route, selects only that Provider's account pool, acquires concurrency capacity, forwards through a capability Adapter, and finalizes quota/audit state.

Provider packages must not depend on HTTP handlers or management-page DTOs. Transport handlers must not construct Provider-specific upstream payloads.

## Capability Interfaces

Adapters implement only the surfaces they own:

- `ResponseAdapter`: Responses plus normalized Chat Completions and Messages forwarding.
- `ModelCatalogAdapter`: remote discovery or static account-aware model calculation.
- `CredentialRefreshAdapter`, `DeviceOAuthAdapter`, `CredentialCodecAdapter`: credential lifecycle.
- `BillingAdapter` or `QuotaAdapter`: quota authority.
- `ImageAdapter`, `VideoAdapter`: media generation.
- `RoutingMetadataAdapter`: quota mode and tier eligibility used by the selector.
- `PricingMetadataAdapter`: maps private upstream model IDs to stable pricing IDs.

Adding a method to the base `Adapter` interface is intentionally avoided. A new capability requires a focused interface, a Definition field, Registry validation, and application-level consumption.

## Request and Pool Flow

1. Expand the client-facing model name into currently available Provider-qualified route candidates, or honor an explicitly qualified compatibility name.
2. Filter candidates by client-key permission, protocol capability, and stored-response ownership, then choose one Provider and upstream mapping.
3. Verify the requested protocol/media surface against the Provider Definition.
4. Enforce client-key model permission, Redis/memory RPM, billing reservation, and global concurrency.
5. Select an eligible account from only the route's Provider pool using model capability, quota mode, priority, cooldown, sticky routing, and per-account concurrency.
6. Ensure the credential is usable; only OAuth Providers may refresh.
7. Forward through the requested capability Adapter and apply bounded failover for account-scoped failures.
8. Finalize audit, quota, response ownership, billing, cooldown, and concurrency exactly once.

An unqualified client model such as `grok-4.5` may resolve to Build, Web, or Console when multiple available routes expose that name. Once a route is selected, request retries stay inside that Provider pool. An explicitly qualified compatibility name such as `Web/grok-chat-fast` pins the request to that source and never enters another Provider pool.

## Batch and Multi-Instance Runtime

Bulk import, conversion, model sync, and credential refresh use child pools under one shared upstream concurrency budget. Random delay and bounded worker counts prevent synchronized bursts. Singleflight prevents duplicate in-process full model syncs.

With `runtime_store.driver=redis`, Redis is authoritative for client-key RPM, distributed concurrency, sticky sessions, Device OAuth sessions, refresh locks, settings notifications, and quota-recovery scheduling. The memory implementation preserves the same interfaces for a single-instance deployment but is not a multi-instance coordination mechanism.

## Model-ID Migration

Stored route IDs are canonical: `Build/<model>`, `Web/<model>`, or `Console/<model>`. These prefixes are internal routing identities; `/v1/models`, request bodies, responses, and management-page public names use the unqualified client-facing model name. Schema initialization migrates legacy rows in one transaction:

- route IDs remain unchanged;
- client-key permissions and other route-ID references remain intact;
- the previous public ID is saved in `model_route_aliases`;
- catalog reconciliation can rename a route again without losing either alias;
- an unqualified name can resolve to several source routes without a database uniqueness conflict.

New manual routes are normalized to their selected Provider internally. A public ID or upstream target carrying another known Provider prefix is rejected.

## Change Checklist

When an upstream changes, keep the change inside its Provider package whenever possible:

1. Update that Adapter's private protocol implementation and tests.
2. Update its `Definition` only when the externally supported capability boundary changes.
3. Update its catalog/routing metadata if model eligibility or quota modes change.
4. Run Registry contract tests, Provider protocol tests, gateway tests, race tests, and the full backend suite.
5. Regenerate Swagger and update compatibility documentation when the downstream API surface changes.

If a change requires Provider-name conditionals in a transport handler or an unrelated Provider package, the abstraction boundary should be reconsidered first.
