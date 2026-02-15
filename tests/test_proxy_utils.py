from app.services.grok.utils.proxy import normalize_proxy_url, build_request_proxies


def test_normalize_socks5_to_socks5h():
    assert (
        normalize_proxy_url("socks5://127.0.0.1:1080")
        == "socks5h://127.0.0.1:1080"
    )


def test_normalize_keeps_http_proxy():
    assert normalize_proxy_url("http://127.0.0.1:8080") == "http://127.0.0.1:8080"


def test_build_request_proxies_none_for_empty():
    assert build_request_proxies("") is None
