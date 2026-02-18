"""
Reverse retry utilities.
"""

import asyncio
import inspect
import random
from typing import Callable, Any, Optional

from app.core.logger import logger
from app.core.config import get_config
from app.core.exceptions import UpstreamException


class RetryContext:
    """Retry context."""

    def __init__(self):
        self.attempt = 0
        self.max_retry = int(get_config("retry.max_retry"))
        self.retry_codes = get_config("retry.retry_status_codes")
        self.last_error = None
        self.last_status = None
        self.total_delay = 0.0
        self.retry_budget = float(get_config("retry.retry_budget"))

        # Backoff parameters
        self.backoff_base = float(get_config("retry.retry_backoff_base"))
        self.backoff_factor = float(get_config("retry.retry_backoff_factor"))
        self.backoff_max = float(get_config("retry.retry_backoff_max"))

        # Decorrelated jitter state
        self._last_delay = self.backoff_base

    def should_retry(self, status_code: int) -> bool:
        """Check if should retry."""
        if self.attempt >= self.max_retry:
            return False
        if status_code not in self.retry_codes:
            return False
        if self.total_delay >= self.retry_budget:
            return False
        return True

    def record_error(self, status_code: int, error: Exception):
        """Record error information."""
        self.last_status = status_code
        self.last_error = error
        self.attempt += 1

    def calculate_delay(self, status_code: int, retry_after: Optional[float] = None) -> float:
        """
        Calculate backoff delay time.

        Args:
            status_code: HTTP status code
            retry_after: Retry-After header value (seconds)

        Returns:
            Delay time (seconds)
        """
        # Use Retry-After if available
        if retry_after is not None and retry_after > 0:
            delay = min(retry_after, self.backoff_max)
            self._last_delay = delay
            return delay

        # Use decorrelated jitter for 429
        if status_code == 429:
            # decorrelated jitter: delay = random(base, last_delay * 3)
            delay = random.uniform(self.backoff_base, self._last_delay * 3)
            delay = min(delay, self.backoff_max)
            self._last_delay = delay
            return delay

        # Use exponential backoff + full jitter for other status codes
        exp_delay = self.backoff_base * (self.backoff_factor**self.attempt)
        delay = random.uniform(0, min(exp_delay, self.backoff_max))
        return delay

    def record_delay(self, delay: float):
        """Record delay time."""
        self.total_delay += delay


def extract_retry_after(error: Exception) -> Optional[float]:
    """
    Extract Retry-After value from exception.

    Args:
        error: Exception object

    Returns:
        Retry-After value (seconds), or None
    """
    if not isinstance(error, UpstreamException):
        return None

    details = error.details or {}

    # Try to get Retry-After from details
    retry_after = details.get("retry_after")
    if retry_after is not None:
        try:
            return float(retry_after)
        except (ValueError, TypeError):
            pass

    # Try to get Retry-After from headers
    headers = details.get("headers", {})
    if isinstance(headers, dict):
        retry_after = headers.get("Retry-After") or headers.get("retry-after")
        if retry_after is not None:
            try:
                return float(retry_after)
            except (ValueError, TypeError):
                pass

    return None


async def retry_on_status(
    func: Callable,
    *args,
    extract_status: Callable[[Exception], Optional[int]] = None,
    on_retry: Callable[[int, int, Exception, float], Any] = None,
    **kwargs,
) -> Any:
    """
    Generic retry function.

    Args:
        func: Retry function
        *args: Function arguments
        extract_status: Function to extract status code from exception
        on_retry: Callback function for retry (attempt, status_code, error, delay).
            Can be sync or async.
        **kwargs: Function keyword arguments

    Returns:
        Function execution result

    Raises:
        Last failed exception
    """
    ctx = RetryContext()

    # Status code extractor
    if extract_status is None:

        def extract_status(e: Exception) -> Optional[int]:
            if isinstance(e, UpstreamException):
                # Try to get status code from details, fallback to status_code attribute
                if e.details and "status" in e.details:
                    return e.details["status"]
                return getattr(e, "status_code", None)
            return None

    while ctx.attempt <= ctx.max_retry:
        try:
            result = await func(*args, **kwargs)

            # Record log
            if ctx.attempt > 0:
                logger.info(
                    f"Retry succeeded after {ctx.attempt} attempts, "
                    f"total delay: {ctx.total_delay:.2f}s"
                )

            return result

        except Exception as e:
            # Extract status code
            status_code = extract_status(e)

            if status_code is None:
                # Error cannot be identified as retryable
                logger.error(f"Non-retryable error: {e}")
                raise

            # Record error
            ctx.record_error(status_code, e)

            # Check if should retry
            if ctx.should_retry(status_code):
                # Extract Retry-After
                retry_after = extract_retry_after(e)

                # Calculate delay
                delay = ctx.calculate_delay(status_code, retry_after)

                # Check if exceeds budget
                if ctx.total_delay + delay > ctx.retry_budget:
                    logger.warning(
                        f"Retry budget exhausted: {ctx.total_delay:.2f}s + {delay:.2f}s > {ctx.retry_budget}s"
                    )
                    raise

                ctx.record_delay(delay)

                logger.warning(
                    f"Retry {ctx.attempt}/{ctx.max_retry} for status {status_code}, "
                    f"waiting {delay:.2f}s (total: {ctx.total_delay:.2f}s)"
                    + (f", Retry-After: {retry_after}s" if retry_after else "")
                )

                # Callback
                if on_retry:
                    result = on_retry(ctx.attempt, status_code, e, delay)
                    if inspect.isawaitable(result):
                        await result

                await asyncio.sleep(delay)
                continue
            else:
                # Not retryable or retry budget exhausted
                if status_code in ctx.retry_codes:
                    logger.error(
                        f"Retry exhausted after {ctx.attempt} attempts, "
                        f"last status: {status_code}, total delay: {ctx.total_delay:.2f}s"
                    )
                else:
                    logger.error(f"Non-retryable status code: {status_code}")

                # Raise last failed exception
                raise


__all__ = [
    "RetryContext",
    "retry_on_status",
    "extract_retry_after",
]
