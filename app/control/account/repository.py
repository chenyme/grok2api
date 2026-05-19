"""Account repository protocol — the contract every backend must satisfy."""

from typing import Protocol, runtime_checkable

from .commands import AccountPatch, AccountUpsert, BulkReplacePoolCommand, ListAccountsQuery
from .models import (
    AccountChangeSet,
    AccountMutationResult,
    AccountPage,
    AccountRecord,
    RuntimeSnapshot,
)


@runtime_checkable
class AccountRepository(Protocol):
    """Storage contract shared by all account backends."""

    async def initialize(self) -> None:
        """Create schema / tables / indices if they do not exist."""
        ...

    async def get_revision(self) -> int:
        """Return the current global revision counter."""
        ...

    async def runtime_snapshot(self) -> RuntimeSnapshot:
        """Return all non-deleted accounts for hot-path bootstrap."""
        ...

    async def scan_changes(
        self,
        since_revision: int,
        *,
        limit: int = 5000,
    ) -> AccountChangeSet:
        """Return records modified after *since_revision* (inclusive)."""
        ...

    async def upsert_accounts(
        self,
        items: list[AccountUpsert],
    ) -> AccountMutationResult:
        """Insert or replace accounts.  Pushes revision."""
        ...

    async def patch_accounts(
        self,
        patches: list[AccountPatch],
    ) -> AccountMutationResult:
        """Apply partial updates to existing accounts.  Pushes revision."""
        ...

    async def delete_accounts(
        self,
        tokens: list[str],
    ) -> AccountMutationResult:
        """Soft-delete accounts (set deleted_at).  Pushes revision."""
        ...

    async def get_accounts(
        self,
        tokens: list[str],
    ) -> list[AccountRecord]:
        """Fetch accounts by token list."""
        ...

    async def list_accounts(
        self,
        query: ListAccountsQuery,
    ) -> AccountPage:
        """Return a paginated, filtered, sorted account list."""
        ...

    async def replace_pool(
        self,
        command: BulkReplacePoolCommand,
    ) -> AccountMutationResult:
        """Atomically replace all accounts in a pool."""
        ...

    async def get_stats(self) -> dict:
        """Return aggregated stats (status/pool/usage/quota counts)."""
        ...

    async def close(self) -> None:
        """Release database connections / file handles."""
        ...


__all__ = ["AccountRepository"]
