# Security notes

Egress Quality Guard operates inside the grok2api administrator trust boundary. It can run real model requests and enable or disable configured egress nodes, so deploy it as an internal control-plane service.

## Required controls

- Never expose the sidecar directly to the public network. It does not listen on a port.
- Keep the administrator password in a root-readable environment file with mode `0600`, or use `GROK2API_ADMIN_PASSWORD_FILE` with a mounted secret.
- Use a dedicated client key for probes. Restrict it to the intended Build model and set explicit RPM and concurrency limits.
- Keep the state volume private. State does not contain credentials or response bodies, but it contains node names, IDs, timing data, and operational history.
- Keep both quality-guard endpoints behind the existing administrator authentication middleware.
- Run the sidecar as an unprivileged user. The provided container and systemd unit do this by default.

## Data handling

The status API deliberately omits the administrator password, client-key secret, proxy URLs, probe prompt, expected marker, and model response body. Logs contain classifications, timings, Token counts, node IDs, and node names only.

The runtime policy file accepts only the documented strategy fields. It cannot edit credentials, proxy configuration, node membership, the probe model, or the probe prompt.

## Reporting a vulnerability

Do not include credentials, proxy URLs, response bodies, database files, state files, or production logs in a public issue. Follow the repository's private security-reporting channel when one is available; otherwise contact the maintainer before publishing operational details.
