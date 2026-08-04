# Security notes

Egress Quality Guard is an internal control-plane sidecar. It can run real model requests and enable or disable configured egress nodes, but it does not receive administrator credentials or general administrator API access.

## Required controls

- Never expose the sidecar directly to the public network. It does not listen on a port.
- The main service provisions a hidden, Build-only system identity for probes. It cannot authenticate through public `/v1` routes and cannot be listed, exported, edited, disabled, or deleted through Client Key administration.
- Keep the shared volume private. Its bootstrap file contains a derived, quality-guard-only internal credential; state also contains node names, IDs, timing data, and operational history.
- The internal credential must remain limited to the dedicated quality-guard route group. Never register the general administrator API under this middleware.
- The passive-audit route exposes only performance metadata. It replaces Client Key identity with a server-computed `qualityProbe` marker, allowing the sidecar to ignore its own probes without learning other key IDs or names.
- Run both containers as the matching unprivileged UID. The provided images use UID 10001 and directory mode `0700`.

## Data handling

The internal credential is derived from `secrets.jwtSecret` with domain separation, written atomically with mode `0600`, and checked in constant time. It is never returned by an HTTP API. The status API deliberately omits the client-key secret, proxy URLs, probe prompt, expected marker, and model response body. Logs contain classifications, timings, Token counts, node IDs, and node names only.

The runtime policy file accepts only the documented strategy fields. It cannot edit credentials, proxy configuration, node membership, the probe model, or the probe prompt.

## Reporting a vulnerability

Do not include credentials, proxy URLs, response bodies, database files, state files, or production logs in a public issue. Follow the repository's private security-reporting channel when one is available; otherwise contact the maintainer before publishing operational details.
