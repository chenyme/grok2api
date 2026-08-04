FROM python:3.12-alpine

RUN addgroup -S -g 10001 quality-guard && \
    adduser -S -D -H -u 10001 -G quality-guard quality-guard && \
    mkdir -p /var/lib/grok2api-quality-guard && \
    chown -R quality-guard:quality-guard /var/lib/grok2api-quality-guard && \
    chmod 0700 /var/lib/grok2api-quality-guard

COPY tools/egress-quality-guard/quality_guard.py /usr/local/bin/grok2api-egress-quality-guard
RUN chmod 0755 /usr/local/bin/grok2api-egress-quality-guard

USER quality-guard
VOLUME ["/var/lib/grok2api-quality-guard"]
ENTRYPOINT ["/usr/local/bin/grok2api-egress-quality-guard"]
