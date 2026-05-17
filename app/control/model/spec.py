"""ModelSpec — the single source of truth for model metadata."""

from dataclasses import dataclass

from .enums import Capability, ModeId, Tier


@dataclass(slots=True, frozen=True)
class ModelSpec:
    """Immutable descriptor for one model variant.

    ``model_name``  is the public-facing identifier used in API requests.
    ``mode_id``     is the upstream ``modeId`` value (auto / fast / expert).
    ``tier``        determines which account pool is used (basic / super).
                    When ``prefer_best`` is True, ``tier`` only affects
                    ``pool_name()``/``pool_id()``; the actual selection order
                    is reversed (heavy → super → basic).
    ``capability``  is a bitmask of supported operations.
    ``enabled``     gates whether the model appears in ``/v1/models``.
    ``public_name`` is the human-readable display name.
    ``prefer_best`` when True, reverses pool priority to try higher-tier
                    pools first (hard priority, not soft preference).
    ``console_model`` when non-empty, route this model through the
                    ``console.x.ai/v1/responses`` endpoint instead of the
                    ``grok.com`` web chat API. The string is the actual
                    model ID sent to console.x.ai (e.g. ``"grok-4.3"``).
                    SSO cookies from grok.com work for both endpoints,
                    so basic-tier accounts can access all models this way.
    """

    model_name: str
    mode_id: ModeId
    tier: Tier
    capability: Capability
    enabled: bool
    public_name: str
    prefer_best: bool = False
    console_model: str = ""

    # --- convenience predicates ---

    def is_chat(self) -> bool:
        return bool(self.capability & Capability.CHAT)

    def is_image(self) -> bool:
        return bool(self.capability & Capability.IMAGE)

    def is_image_edit(self) -> bool:
        return bool(self.capability & Capability.IMAGE_EDIT)

    def is_video(self) -> bool:
        return bool(self.capability & Capability.VIDEO)

    def is_voice(self) -> bool:
        return bool(self.capability & Capability.VOICE)

    def is_console(self) -> bool:
        """Return True if this model routes through console.x.ai."""
        return bool(self.console_model)

    def pool_name(self) -> str:
        """Return the canonical pool string for this tier."""
        if self.tier == Tier.SUPER:
            return "super"
        if self.tier == Tier.HEAVY:
            return "heavy"
        return "basic"

    def pool_id(self) -> int:
        """Return the integer PoolId for the dataplane account table."""
        return int(self.tier)

    # 返回按优先级排序的候选池 ID
    def pool_candidates(self) -> tuple[int, ...]:
        """Return pool IDs to try in priority order.

        When ``prefer_best`` is True the order is reversed so that the
        highest-tier pool is attempted first (hard priority — the first
        pool with any available account wins; lower pools are only reached
        when all accounts in higher pools are exhausted).

        Default (prefer_best=False):
          BASIC tier  → try basic first, then super, then heavy
          SUPER tier  → try super first, then heavy
          HEAVY tier  → heavy only

        Reversed (prefer_best=True):
          BASIC tier  → try heavy first, then super, then basic
          SUPER tier  → try heavy first, then super
          HEAVY tier  → heavy only
        """
        if self.prefer_best:
            if self.tier == Tier.HEAVY:
                return (2, )  # heavy only
            if self.tier == Tier.SUPER:
                return (2, 1)  # heavy, super
            return (2, 1, 0)  # heavy, super, basic
        if self.tier == Tier.BASIC:
            return (0, 1, 2)  # basic, super, heavy
        if self.tier == Tier.SUPER:
            return (1, 2)  # super, heavy
        return (2, )  # heavy only


__all__ = ["ModelSpec"]
