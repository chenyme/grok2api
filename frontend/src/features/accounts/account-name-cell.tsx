import { Bot, Compass, Handshake, SquareTerminal, VenusAndMars, Webhook, type LucideIcon } from "lucide-react";
import { useTranslation } from "react-i18next";

import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import type { AccountDTO, AccountProvider, LinkedAccountDTO } from "@/features/accounts/accounts-api";
import { cn } from "@/shared/lib/cn";
import { formatDateTime } from "@/shared/lib/format";

const providerOrder: Record<AccountProvider, number> = {
  grok_build: 0,
  grok_web: 1,
  grok_console: 2,
};

const providerIcon: Record<AccountProvider, { icon: LucideIcon; className: string }> = {
  grok_build: { icon: SquareTerminal, className: "text-quota-product-1" },
  grok_web: { icon: Compass, className: "text-quota-product-2" },
  grok_console: { icon: Webhook, className: "text-quota-product-4" },
};

function identityDetails(name: string, email?: string, userId?: string): string[] {
  const values = [email?.trim() || name.trim(), userId?.trim()];
  const seen = new Set<string>();
  return values.filter((value): value is string => {
    if (!value) return false;
    const normalized = value.toLocaleLowerCase();
    if (seen.has(normalized)) return false;
    seen.add(normalized);
    return true;
  });
}

function accountLinks(account: AccountDTO): LinkedAccountDTO[] {
  const links = account.linkedAccounts ?? (account.linkedAccountId && account.linkedProvider
    ? [{ id: account.linkedAccountId, provider: account.linkedProvider, name: account.linkedAccountName ?? "" }]
    : []);
  return [...links].sort((left, right) => providerOrder[left.provider] - providerOrder[right.provider] || left.id.localeCompare(right.id));
}

export function AccountNameCell({ account }: { account: AccountDTO }) {
  const { t, i18n } = useTranslation();
  const links = accountLinks(account);
  const providerLabel = (provider: AccountProvider) => provider === "grok_build"
    ? t("models.providerGrokBuild")
    : provider === "grok_web"
      ? t("models.providerGrokWeb")
      : t("console.name");
  const connections = [
    { id: account.id, provider: account.provider, details: identityDetails(account.name, account.email, account.userId) },
    ...links.filter((linked) => linked.provider !== account.provider).map((linked) => ({ id: linked.id, provider: linked.provider, details: identityDetails(linked.name, linked.email, linked.userId) })),
  ].sort((left, right) => providerOrder[left.provider] - providerOrder[right.provider]);

  return (
    <div className="flex min-h-9 min-w-0 flex-col justify-center gap-0.5">
      <div className="flex min-w-0 items-center gap-1.5">
        <Tooltip>
          <TooltipTrigger asChild>
            <span className="min-w-0 truncate text-xs font-medium">{account.name}</span>
          </TooltipTrigger>
          <TooltipContent>{account.name}</TooltipContent>
        </Tooltip>
      </div>
      <div className="flex min-h-4 w-fit min-w-0 items-center">
        <Tooltip>
          <TooltipTrigger asChild>
            <div
              tabIndex={0}
              aria-label={connections.map((connection) => providerLabel(connection.provider)).join(", ")}
              className="flex min-w-0 cursor-help items-center gap-1.5 overflow-hidden rounded-sm focus-visible:outline-none"
            >
              {connections.map((connection) => {
                const { icon: ProviderIcon, className } = providerIcon[connection.provider];
                return <ProviderIcon key={`${connection.provider}:${connection.id}`} className={cn("size-3.5 shrink-0", className)} />;
              })}
            </div>
          </TooltipTrigger>
          <TooltipContent className="min-w-44 space-y-2 px-2.5 py-2">
            {connections.map((connection) => {
              const label = providerLabel(connection.provider);
              const { icon: ProviderIcon, className } = providerIcon[connection.provider];
              return (
                <div key={`${connection.provider}:${connection.id}`} className="space-y-0.5">
                  <div className="flex items-center gap-1.5 text-xs font-medium">
                    <ProviderIcon className={cn("size-3.5", className)} />
                    <span>{label}</span>
                  </div>
                  {(connection.details.length > 0 ? connection.details : [label]).map((detail, index) => (
                    <p key={detail} className={cn("max-w-64 truncate pl-5 text-xs", index > 0 && "text-primary-foreground/70")}>{detail}</p>
                  ))}
                </div>
              );
            })}
          </TooltipContent>
        </Tooltip>
        {account.termsAcceptedAt || account.nsfwEnabledAt ? (
          <>
            <span className="mx-2 h-3 w-px shrink-0 bg-border" aria-hidden="true" />
            <span className="flex items-center gap-1.5">
              {account.termsAcceptedAt ? (
                <Tooltip>
                  <TooltipTrigger asChild>
                    <span tabIndex={0} aria-label={t("webAccountSettings.acceptTerms")} className="inline-flex cursor-help text-pink-500 focus-visible:outline-none dark:text-pink-400">
                      <Handshake className="size-3.5" />
                    </span>
                  </TooltipTrigger>
                  <TooltipContent>{t("webAccountSettings.acceptTerms")} · {formatDateTime(account.termsAcceptedAt, i18n.language)}</TooltipContent>
                </Tooltip>
              ) : null}
              {account.nsfwEnabledAt ? (
                <Tooltip>
                  <TooltipTrigger asChild>
                    <span tabIndex={0} aria-label={t("accounts.nsfwEnabledMark")} className="inline-flex cursor-help text-yellow-500 focus-visible:outline-none dark:text-yellow-400">
                      <VenusAndMars className="size-3.5" />
                    </span>
                  </TooltipTrigger>
                  <TooltipContent>{t("accounts.nsfwEnabledTooltip", { time: formatDateTime(account.nsfwEnabledAt, i18n.language) })}</TooltipContent>
                </Tooltip>
              ) : null}
            </span>
          </>
        ) : null}
        {account.buildBotFlagged ? (
          <>
            <span className="mx-2 h-3 w-px shrink-0 bg-border" aria-hidden="true" />
            <Tooltip>
              <TooltipTrigger asChild>
                <span tabIndex={0} aria-label={t("accounts.botRisk")} className="inline-flex cursor-help text-amber-500 focus-visible:outline-none dark:text-amber-400">
                  <Bot className="size-3.5" />
                </span>
              </TooltipTrigger>
              <TooltipContent>{t("accounts.botRiskTooltip")}</TooltipContent>
            </Tooltip>
          </>
        ) : null}
      </div>
    </div>
  );
}
