import argparse
import asyncio
import json
import os
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterable

from dotenv import load_dotenv

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

load_dotenv(ROOT / ".env")

from app.core.config import config
from app.core.logger import setup_logging
from app.services.grok.services.chat import GrokChatService
from app.services.grok.processors.processor import CollectProcessor
from app.services.grok.services.usage import UsageService


async def _fetch_usage(token: str, timeout: float) -> int | None:
    usage = UsageService()
    try:
        result = await asyncio.wait_for(usage.get(token), timeout=timeout)
    except Exception as exc:
        print(f"Usage fetch failed: {exc}")
        return None
    try:
        return int(result.get("remainingTokens"))
    except Exception:
        return None


async def _run_once(
    model: str,
    mode: str,
    token: str,
    message: str,
    timeout: float,
    lock: asyncio.Lock | None = None,
) -> tuple[bool, int | None, int | None]:
    async def _execute() -> tuple[bool, int | None, int | None]:
        service = GrokChatService()
        before = await _fetch_usage(token, timeout)
        http_ok = False
        try:
            print(f"Requesting model={model} mode={mode} ...")
            response = await asyncio.wait_for(
                service.chat(
                    token=token,
                    message=message,
                    model=model,
                    mode=mode,
                    think=False,
                    stream=False,
                ),
                timeout=timeout,
            )
            http_ok = True
        except Exception as exc:
            print(f"Request failed: {exc}")
            after = await _fetch_usage(token, timeout)
            return False, before, after

        processor = CollectProcessor(model, token)
        try:
            print("Collecting response ...")
            result = await asyncio.wait_for(processor.process(response), timeout=timeout)
        except Exception as exc:
            print(f"Collect failed: {exc}")
            after = await _fetch_usage(token, timeout)
            return False, before, after

        content = (
            result.get("choices", [{}])[0]
            .get("message", {})
            .get("content", "")
            .strip()
        )
        after = await _fetch_usage(token, timeout)
        ok = http_ok and bool(content)
        return ok, before, after

    if lock is None:
        return await _execute()
    async with lock:
        return await _execute()


async def _run_test(
    grok_model: str,
    model_mode: str,
    basic_token: str,
    super_token: str,
    message: str,
    out_path: str,
    timeout: float,
    model_id: str | None = None,
    lock_map: dict[str, asyncio.Lock] | None = None,
    load_config: bool = True,
) -> tuple[dict, bool]:
    if load_config:
        await config.load()

    basic_lock = lock_map.get(basic_token) if lock_map else None
    super_lock = lock_map.get(super_token) if lock_map else None

    print("Testing basic token ...")
    basic_task = asyncio.create_task(
        _run_once(grok_model, model_mode, basic_token, message, timeout, basic_lock)
    )
    print("Testing super token ...")
    super_task = asyncio.create_task(
        _run_once(grok_model, model_mode, super_token, message, timeout, super_lock)
    )
    basic_ok, basic_before, basic_after = await basic_task
    super_ok, super_before, super_after = await super_task

    basic_delta = (
        (basic_before - basic_after)
        if (basic_before is not None and basic_after is not None)
        else None
    )
    super_delta = (
        (super_before - super_after)
        if (super_before is not None and super_after is not None)
        else None
    )

    cost_guess = _guess_cost(basic_delta, super_delta)

    payload = {
        "model_id": model_id or grok_model,
        "grok_model": grok_model,
        "model_mode": model_mode,
        "basic": {
            "ok": bool(basic_ok),
            "before": basic_before,
            "after": basic_after,
            "delta": basic_delta,
        },
        "super": {
            "ok": bool(super_ok),
            "before": super_before,
            "after": super_after,
            "delta": super_delta,
        },
        "cost_guess": cost_guess,
        "timestamp": datetime.now(timezone.utc).isoformat(),
    }

    ok = bool(basic_ok and super_ok)
    if out_path:
        _append_result(out_path, payload)
        print(f"Appended results to {out_path}")
    return payload, ok


def _guess_cost(basic_delta: int | None, super_delta: int | None) -> str | None:
    for delta in (basic_delta, super_delta):
        if delta is None:
            continue
        return "high" if delta >= 4 else "low"
    return None


def _load_tokens_file(path: str) -> dict:
    if not path:
        return {}
    file_path = Path(path)
    if not file_path.exists():
        return {}
    try:
        with file_path.open("r", encoding="utf-8") as f:
            data = json.load(f)
        if isinstance(data, dict):
            return data
    except Exception as exc:
        print(f"Failed to read tokens file: {exc}")
    return {}


def _prompt_if_missing(value: str, label: str) -> str:
    if value:
        return value
    return input(f"{label}: ").strip()


def _append_result(out_path: str, payload: dict) -> None:
    out_file = Path(out_path)
    data = []
    if out_file.exists():
        try:
            with out_file.open("r", encoding="utf-8") as f:
                existing = json.load(f)
            if isinstance(existing, list):
                data = existing
            elif isinstance(existing, dict):
                data = [existing]
        except Exception as exc:
            print(f"Failed to read existing results, overwrite: {exc}")
            data = []
    if isinstance(payload, list):
        data.extend(payload)
    else:
        data.append(payload)
    with out_file.open("w", encoding="utf-8") as f:
        json.dump(data, f, ensure_ascii=True, indent=2)


def _normalize_matrix_item(item: Any) -> dict[str, Any]:
    if isinstance(item, (list, tuple)):
        if len(item) < 3:
            return {}
        return {
            "grok_model": str(item[0]).strip(),
            "model_mode": str(item[1]).strip(),
            "model_id": str(item[2]).strip(),
        }
    if isinstance(item, dict):
        grok_model = item.get("grok_model") or item.get("model") or item.get("grok")
        model_mode = item.get("model_mode") or item.get("mode")
        model_id = item.get("model_id") or item.get("id") or item.get("name")
        tier = item.get("tier")
        if not (grok_model and model_mode and model_id):
            return {}
        normalized = {
            "grok_model": str(grok_model).strip(),
            "model_mode": str(model_mode).strip(),
            "model_id": str(model_id).strip(),
        }
        if tier:
            normalized["tier"] = str(tier).strip()
        return normalized
    return {}


def _parse_matrix_text(text: str) -> list[dict[str, Any]]:
    items: list[dict[str, Any]] = []
    for line in text.splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        parts = line.split()
        if len(parts) < 3:
            continue
        entry = {
            "grok_model": parts[0],
            "model_mode": parts[1],
            "model_id": parts[2],
        }
        if len(parts) >= 4:
            entry["tier"] = parts[3]
        items.append(entry)
    return items


def _load_matrix(matrix_file: str | None, matrix_inline: str | None) -> list[dict]:
    if matrix_inline:
        try:
            data = json.loads(matrix_inline)
            if isinstance(data, list):
                items = [_normalize_matrix_item(x) for x in data]
                return [x for x in items if x]
        except Exception:
            return _parse_matrix_text(matrix_inline)

    if matrix_file:
        file_path = Path(matrix_file)
        if file_path.exists():
            text = file_path.read_text(encoding="utf-8").strip()
            if not text:
                return []
            try:
                data = json.loads(text)
                if isinstance(data, list):
                    items = [_normalize_matrix_item(x) for x in data]
                    return [x for x in items if x]
            except Exception:
                return _parse_matrix_text(text)
    return []


def _build_token_locks(tokens: Iterable[str]) -> dict[str, asyncio.Lock]:
    locks: dict[str, asyncio.Lock] = {}
    for token in tokens:
        if token and token not in locks:
            locks[token] = asyncio.Lock()
    return locks


def _format_model_list(results: Iterable[dict]) -> str:
    lines = []
    for item in results:
        model_id = item.get("model_id") or item.get("grok_model") or ""
        grok_model = item.get("grok_model") or ""
        model_mode = item.get("model_mode") or ""
        tier = item.get("tier")
        cost_guess = item.get("cost_guess")
        cost = "Cost.HIGH" if cost_guess == "high" else "Cost.LOW"
        display_name = model_id.upper() if model_id else ""

        lines.append("        ModelInfo(")
        lines.append(f'            model_id="{model_id}",')
        lines.append(f'            grok_model="{grok_model}",')
        lines.append(f'            model_mode="{model_mode}",')
        if tier and str(tier).upper() == "SUPER":
            lines.append("            tier=Tier.SUPER,")
        lines.append(f"            cost={cost},")
        lines.append(f'            display_name="{display_name}",')
        lines.append("        ),")
        lines.append("")

    return "\n".join(lines).rstrip()


async def _run_matrix(
    matrix: list[dict],
    basic_token: str,
    super_token: str,
    message: str,
    out_path: str,
    timeout: float,
    max_concurrent: int,
) -> tuple[list[dict], bool]:
    await config.load()
    locks = _build_token_locks([basic_token, super_token])
    sem = asyncio.Semaphore(max(1, int(max_concurrent)))

    async def _one(entry: dict) -> tuple[dict, bool]:
        async with sem:
            payload, ok = await _run_test(
                entry["grok_model"],
                entry["model_mode"],
                basic_token,
                super_token,
                message,
                out_path="",
                timeout=timeout,
                model_id=entry.get("model_id"),
                lock_map=locks,
                load_config=False,
            )
            if entry.get("tier"):
                payload["tier"] = entry["tier"]
            return payload, ok

    tasks = [_one(entry) for entry in matrix]
    pairs = await asyncio.gather(*tasks)
    results = [payload for payload, _ in pairs]
    all_ok = all(ok for _, ok in pairs)

    if out_path:
        _append_result(out_path, results)
        print(f"Appended results to {out_path}")
    return results, all_ok


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Test Grok model by grok_model and model_mode using basic/super tokens."
    )
    parser.add_argument("grok_model", nargs="?", help="e.g. grok-4-1-thinking-1129")
    parser.add_argument("model_mode", nargs="?", help="e.g. MODEL_MODE_GROK_4_1_THINKING")
    parser.add_argument("--model-id", dest="model_id", help="model id for output")
    parser.add_argument("--basic-token", dest="basic_token", help="basic account token")
    parser.add_argument("--super-token", dest="super_token", help="super account token")
    parser.add_argument(
        "--tokens-file",
        default="data/model_tokens.json",
        help="path to tokens json file",
    )
    parser.add_argument("--matrix", help="inline JSON or line-based model list")
    parser.add_argument("--matrix-file", help="path to model list (json or text)")
    parser.add_argument(
        "--max-concurrent",
        type=int,
        default=2,
        help="max concurrent model tests",
    )
    parser.add_argument(
        "--emit-model-list",
        action="store_true",
        help="print ModelInfo list snippet",
    )
    parser.add_argument("--emit-model-list-out", help="write ModelInfo list snippet")
    parser.add_argument("--message", default="Ping", help="test prompt")
    parser.add_argument("--out", default="model.json", help="output json path")
    parser.add_argument("--timeout", type=float, default=120, help="timeout seconds")
    parser.add_argument("--log-level", help="log level (overrides LOG_LEVEL)")
    args = parser.parse_args()

    tokens_file_data = _load_tokens_file(args.tokens_file)
    basic_token = _prompt_if_missing(
        args.basic_token
        or tokens_file_data.get("basic_token", "")
        or os.getenv("BASIC_TOKEN", ""),
        "basic_token",
    )
    super_token = _prompt_if_missing(
        args.super_token
        or tokens_file_data.get("super_token", "")
        or os.getenv("SUPER_TOKEN", ""),
        "super_token",
    )
    if not basic_token or not super_token:
        print("basic_token and super_token are required.")
        return 2

    log_level = args.log_level or os.getenv("LOG_LEVEL", "INFO")
    setup_logging(level=log_level, json_console=False, file_logging=False)
    matrix = _load_matrix(args.matrix_file, args.matrix)
    if matrix:
        results, ok = asyncio.run(
            _run_matrix(
                matrix,
                basic_token,
                super_token,
                args.message,
                args.out,
                args.timeout,
                args.max_concurrent,
            )
        )
        if args.emit_model_list or args.emit_model_list_out:
            snippet = _format_model_list(results)
            if args.emit_model_list_out:
                Path(args.emit_model_list_out).write_text(
                    snippet + "\n", encoding="utf-8"
                )
                print(f"Model list written to {args.emit_model_list_out}")
            else:
                print(snippet)
        return 0 if ok else 1

    grok_model = args.grok_model or os.getenv("GROK_MODEL", "")
    model_mode = args.model_mode or os.getenv("MODEL_MODE", "")
    if not grok_model:
        print("grok_model is required.")
        return 2

    _payload, ok = asyncio.run(
        _run_test(
            grok_model,
            model_mode,
            basic_token,
            super_token,
            args.message,
            args.out,
            args.timeout,
            model_id=args.model_id,
            lock_map=_build_token_locks([basic_token, super_token]),
        )
    )
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main())
