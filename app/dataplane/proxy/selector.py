"""Proxy dataplane selector — pick the best egress node from a ProxyRuntimeTable.

Extracted from ProxyDirectory.acquire() to formalize the dataplane separation.
"""

from app.control.proxy.models import (
    EgressMode, ProxyScope, RequestKind,
)
from .table import ProxyRuntimeTable


def select_proxy(
    table: ProxyRuntimeTable,
    scope: ProxyScope = ProxyScope.APP,
    kind: RequestKind = RequestKind.HTTP,
) -> str | None:
    """Select a proxy URL from the table.

    Returns ``None`` for DIRECT mode or if no healthy nodes are available.
    """
    if table.egress_mode == EgressMode.DIRECT:
        return None

    if table.egress_mode == EgressMode.SINGLE_PROXY:
        if table.nodes:
            return table.nodes[0].proxy_url
        return None

    # PROXY_POOL: pick the node with lowest inflight count.
    healthy = table.healthy_nodes()
    if not healthy:
        return None

    best = min(healthy, key=lambda n: n.inflight)
    return best.proxy_url


__all__ = ["select_proxy"]
