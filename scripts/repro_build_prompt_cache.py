#!/usr/bin/env python3
"""Reproduce Grok Build free prompt-cache behavior against a grok2api Responses endpoint.

Background
----------
Free Grok Build OAuth (cli-chat-proxy, model often reported as grok-*-build-free)
frequently returns usage.input_tokens_details.cached_tokens == 0 across multi-turn
traffic unless the request declares native tools:

  {"type": "web_search"}
  {"type": "x_search"}

This matches the analysis in CLIProxyAPI #4213. Stable prompt_cache_key + account
stickiness alone is NOT enough without those tools.

Modes
-----
  baseline   No tools in the client body (expects inject=on gateway to recover cache).
  native     Client sends web_search + x_search (should cache even with inject=off).
  function   Client only sends a function tool (no native search types).

Examples
--------
  export GROK2API_BASE=http://127.0.0.1:8000
  export GROK2API_KEY=g2a_xxx
  python3 scripts/repro_build_prompt_cache.py --mode baseline --turns 2
  python3 scripts/repro_build_prompt_cache.py --mode function --turns 2
  python3 scripts/repro_build_prompt_cache.py --mode native --turns 2

Exit code is 0 when the last turn reports cached_tokens > 0 (success path for
baseline/native under a healthy free Build pool). Use --allow-zero to always exit 0.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request
from typing import Any


def build_tools(mode: str) -> list[dict[str, Any]] | None:
    if mode == "baseline":
        return None
    if mode == "native":
        return [{"type": "web_search"}, {"type": "x_search"}]
    if mode == "function":
        return [
            {
                "type": "function",
                "name": "local_echo",
                "description": "Echo helper used only to prove function tools do not unlock free Build cache.",
                "parameters": {
                    "type": "object",
                    "properties": {"text": {"type": "string"}},
                    "required": ["text"],
                },
            }
        ]
    raise SystemExit(f"unknown mode: {mode}")


def call_responses(
    *,
    base: str,
    key: str,
    model: str,
    prompt_cache_key: str,
    system: str,
    user: str,
    tools: list[dict[str, Any]] | None,
    timeout: float,
) -> dict[str, Any]:
    body: dict[str, Any] = {
        "model": model,
        "store": False,
        "prompt_cache_key": prompt_cache_key,
        "input": [
            {"role": "system", "content": system},
            {"role": "user", "content": user},
        ],
    }
    if tools is not None:
        body["tools"] = tools
    req = urllib.request.Request(
        f"{base.rstrip('/')}/v1/responses",
        data=json.dumps(body).encode("utf-8"),
        headers={
            "Authorization": f"Bearer {key}",
            "Content-Type": "application/json",
        },
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read().decode("utf-8"))


def cached_tokens(usage: dict[str, Any] | None) -> int:
    if not usage:
        return 0
    details = usage.get("input_tokens_details") or {}
    value = details.get("cached_tokens")
    try:
        return int(value or 0)
    except (TypeError, ValueError):
        return 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--base", default=os.environ.get("GROK2API_BASE", "http://127.0.0.1:8000"))
    parser.add_argument("--key", default=os.environ.get("GROK2API_KEY", ""))
    parser.add_argument("--model", default=os.environ.get("GROK2API_MODEL", "grok-4.5"))
    parser.add_argument("--mode", choices=("baseline", "native", "function"), default="baseline")
    parser.add_argument("--turns", type=int, default=2)
    parser.add_argument("--pad-words", type=int, default=2000, help="Stable system-prefix size (words)")
    parser.add_argument("--prompt-cache-key", default="")
    parser.add_argument("--timeout", type=float, default=180.0)
    parser.add_argument("--sleep", type=float, default=1.0)
    parser.add_argument("--allow-zero", action="store_true", help="Do not fail when last turn has zero cache")
    args = parser.parse_args()

    if not args.key:
        print("GROK2API_KEY / --key is required", file=sys.stderr)
        return 2
    if args.turns < 1:
        print("--turns must be >= 1", file=sys.stderr)
        return 2

    pck = args.prompt_cache_key or f"cache-repro-{args.mode}-{int(time.time())}"
    system = "prefix-stable-cache-test " + ("word " * max(args.pad_words, 1))
    tools = build_tools(args.mode)

    print(json.dumps({
        "base": args.base,
        "model": args.model,
        "mode": args.mode,
        "turns": args.turns,
        "prompt_cache_key": pck,
        "tools": tools,
        "system_chars": len(system),
    }, ensure_ascii=False))

    last_cached = 0
    for turn in range(1, args.turns + 1):
        try:
            payload = call_responses(
                base=args.base,
                key=args.key,
                model=args.model,
                prompt_cache_key=pck,
                system=system,
                user=f"Reply with exactly: OK{turn}",
                tools=tools,
                timeout=args.timeout,
            )
        except urllib.error.HTTPError as err:
            body = err.read().decode("utf-8", errors="replace")
            print(json.dumps({"turn": turn, "http_error": err.code, "body": body[:2000]}, ensure_ascii=False))
            return 1
        except Exception as err:  # noqa: BLE001 - CLI surface
            print(json.dumps({"turn": turn, "error": str(err)}, ensure_ascii=False))
            return 1

        usage = payload.get("usage") or {}
        last_cached = cached_tokens(usage)
        row = {
            "turn": turn,
            "id": payload.get("id"),
            "model": payload.get("model"),
            "input_tokens": usage.get("input_tokens"),
            "cached_tokens": last_cached,
            "output_tokens": usage.get("output_tokens"),
            "num_server_side_tools_used": usage.get("num_server_side_tools_used"),
            "error": payload.get("error"),
        }
        print(json.dumps(row, ensure_ascii=False))
        if turn < args.turns and args.sleep > 0:
            time.sleep(args.sleep)

    if last_cached > 0 or args.allow_zero:
        return 0
    print("last turn cached_tokens == 0", file=sys.stderr)
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
