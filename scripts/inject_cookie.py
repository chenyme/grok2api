#!/usr/bin/env python3
"""Grok cookie injector — set the SSO cookie in a browser and open Grok.

Usage:
    python inject_cookie.py <cookie_value>
    python inject_cookie.py --url https://grok.com <cookie_value>
    python inject_cookie.py --browser firefox <cookie_value>

The cookie value can be:
  - A raw JWT:  eyJ0eXAiOiJKV1Qi...
  - With sso= prefix:  sso=eyJ0eXAiOiJKV1Qi...
  - A full cookie header:  sso=eyJ...; Domain=.grok.com; Path=/

Requirements:
  pip install browser-cookie3  (optional, for reading existing cookies)

The script uses webbrowser to open the URL and prints instructions for
manual cookie injection into Chrome DevTools.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
import webbrowser

# ── JWT decode (no signature verification) ──────────────────────────────


def decode_jwt_payload(raw: str) -> dict:
    import base64

    parts = raw.strip().split(".")
    if len(parts) != 3:
        raise ValueError("Not a valid JWT (expected 3 segments)")

    payload_b64 = parts[1]
    payload_b64 += "=" * (4 - len(payload_b64) % 4)
    try:
        payload = json.loads(base64.urlsafe_b64decode(payload_b64))
    except Exception as exc:
        raise ValueError(f"Cannot decode JWT payload: {exc}") from exc
    return payload


def parse_cookie(raw: str) -> tuple[str, str, dict]:
    """Parse a Grok SSO cookie value.

    Returns (cookie_name, cookie_value, jwt_payload).
    """
    text = raw.strip()

    # Strip browser export format: "Cookie: sso=..."
    if text.lower().startswith("cookie:"):
        text = text[len("cookie:"):].strip()

    # Find the JWT by looking for sso= prefix
    cookie_value = text
    if text.startswith("sso="):
        cookie_value = text[4:]
        if ";" in cookie_value:
            cookie_value = cookie_value.split(";")[0].strip()

    # If no sso= prefix, assume raw JWT
    cookie_value = cookie_value.strip()
    payload = decode_jwt_payload(cookie_value)
    return ("sso", cookie_value, payload)


# ── Browser injection helpers ───────────────────────────────────────────


def inject_via_playwright(cookie_value: str, url: str) -> int:
    """Use Playwright to inject the cookie and open the browser."""
    try:
        from playwright.sync_api import sync_playwright
    except ImportError:
        print("[!] playwright not installed. Install with: pip install playwright && playwright install chromium")
        return 1

    with sync_playwright() as p:
        browser = p.chromium.launch(headless=False)
        context = browser.new_context()
        context.add_cookies([
            {
                "name": "sso",
                "value": cookie_value,
                "domain": ".grok.com",
                "path": "/",
                "httpOnly": False,
                "secure": True,
                "sameSite": "Lax",
            }
        ])
        page = context.new_page()
        page.goto(url)
        print(f"[+] Cookie injected — browser opened at {url}")
        print("[+] Press Ctrl+C to close the browser...")
        try:
            page.wait_for_timeout(60_000)  # 60s before auto-close
        except KeyboardInterrupt:
            pass
        browser.close()
    return 0


def print_manual_instructions(cookie_value: str, url: str) -> None:
    """Print manual DevTools injection instructions."""
    js = (
        "document.cookie = 'sso=" + cookie_value + "; "
        "domain=.grok.com; path=/; SameSite=Lax; Secure';"
    )

    print(f"""
╔══════════════════════════════════════════════════════════════╗
║  Grok Cookie Injection — Manual Instructions                ║
╠══════════════════════════════════════════════════════════════╣
║                                                              ║
║  1. Open {url} in your browser                      ║
║  2. Press F12 to open DevTools                              ║
║  3. Go to the Console tab                                   ║
║  4. Paste this command:                                     ║
║                                                              ║
║  {js}
║                                                              ║
║  5. Press Enter (the page will reload)                      ║
║  6. You should be logged in                                 ║
║                                                              ║
╠══════════════════════════════════════════════════════════════╣
║  Alternative — Application tab:                             ║
║  1. F12 → Application → Cookies → grok.com                  ║
║  2. Add cookie: name=sso, value=<jwt>, path=/               ║
║  3. Refresh the page                                        ║
║                                                              ║
╠══════════════════════════════════════════════════════════════╣
║  For grok2api import:                                       ║
║  Use the token value directly:                              ║
║    curl -X POST .../admin/api/tokens/add                    ║
║      -d '{{"tokens": ["{cookie_value}"], "pool": "auto"}}'  ║
╚══════════════════════════════════════════════════════════════╝
""")


# ── Main ─────────────────────────────────────────────────────────────────


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Inject a Grok SSO cookie into a browser and open Grok",
    )
    parser.add_argument(
        "cookie",
        help="Grok SSO cookie value (JWT, optionally with 'sso=' prefix)",
    )
    parser.add_argument(
        "--url", default="https://grok.com",
        help="Target URL (default: https://grok.com)",
    )
    parser.add_argument(
        "--manual", action="store_true",
        help="Print manual DevTools injection instructions (no automation)",
    )
    parser.add_argument(
        "--playwright", action="store_true",
        help="Use Playwright to automate cookie injection",
    )
    args = parser.parse_args()

    try:
        name, value, payload = parse_cookie(args.cookie)
    except ValueError as exc:
        print(f"[!] Cookie parse error: {exc}", file=sys.stderr)
        return 1

    session_id = payload.get("session_id", "unknown")
    print(f"[+] Parsed cookie: name={name}, session_id={session_id}")
    print(f"[+] Full token: {value[:40]}...{value[-20:]}")

    if args.playwright:
        return inject_via_playwright(value, args.url)

    # Default: print instructions and offer to open browser.
    print_manual_instructions(value, args.url)

    try:
        choice = input("\n[?] Open browser now? (y/N): ").strip().lower()
        if choice in ("y", "yes"):
            webbrowser.open(args.url)
    except (KeyboardInterrupt, EOFError):
        pass

    return 0


if __name__ == "__main__":
    sys.exit(main())
