"""In-memory OAuth session outcomes for admin UI polling (single-worker friendly)."""

from typing import Any

_outcomes: dict[str, dict[str, Any]] = {}


def set_outcome(state: str, *, status: str, message: str = "", email: str | None = None) -> None:
    _outcomes[state] = {
        "status": status,
        "message": message,
        **({"email": email} if email else {}),
    }


def peek_outcome(state: str) -> dict[str, Any] | None:
    return _outcomes.get(state)


def pop_outcome(state: str) -> dict[str, Any] | None:
    return _outcomes.pop(state, None)


def clear_outcome(state: str) -> None:
    _outcomes.pop(state, None)


__all__ = ["set_outcome", "peek_outcome", "pop_outcome", "clear_outcome"]