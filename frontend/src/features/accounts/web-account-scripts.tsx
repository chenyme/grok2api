import { Cake, Handshake, VenusAndMars, type LucideIcon } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Checkbox } from "@/components/ui/checkbox";
import { Spinner } from "@/components/ui/spinner";
import type { AccountTaskProgressDTO, WebAccountScriptActions } from "@/features/accounts/accounts-api";
import { cn } from "@/shared/lib/cn";

type Props = {
  targets: readonly string[] | "all";
  pending: boolean;
  progress: AccountTaskProgressDTO | null;
  onClose: () => void;
  onRun: (actions: WebAccountScriptActions) => void;
};

const defaultActions: WebAccountScriptActions = {
  acceptTerms: true,
  setBirthDate: true,
  enableNSFW: true,
};

export function WebAccountScriptsDialog({ targets, pending, progress, onClose, onRun }: Props) {
  const { t } = useTranslation();
  const [actions, setActions] = useState<WebAccountScriptActions>(defaultActions);
  const hasAction = actions.acceptTerms || actions.setBirthDate || actions.enableNSFW;

  function updateAction(action: keyof WebAccountScriptActions, checked: boolean): void {
    setActions((current) => {
      if (action === "enableNSFW") {
        return { ...current, enableNSFW: checked, setBirthDate: checked || current.setBirthDate };
      }
      if (action === "setBirthDate" && current.enableNSFW && !checked) {
        return current;
      }
      return { ...current, [action]: checked };
    });
  }

  const items: Array<{
    action: keyof WebAccountScriptActions;
    icon: LucideIcon;
    label: string;
    description: string;
    locked?: boolean;
  }> = [
    {
      action: "acceptTerms",
      icon: Handshake,
      label: t("webAccountSettings.acceptTerms"),
      description: t("webAccountScripts.acceptTermsDescription"),
    },
    {
      action: "setBirthDate",
      icon: Cake,
      label: t("webAccountSettings.setBirthDate"),
      description: t("webAccountScripts.setBirthDateDescription"),
      locked: actions.enableNSFW,
    },
    {
      action: "enableNSFW",
      icon: VenusAndMars,
      label: t("webAccountSettings.enableNSFW"),
      description: t("webAccountScripts.enableNSFWDescription"),
    },
  ];

  return (
    <AlertDialog open onOpenChange={(open) => { if (!open) onClose(); }}>
      <AlertDialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto">
        <AlertDialogHeader>
          <AlertDialogTitle>{t(targets === "all" ? "webAccountScripts.allTitle" : "webAccountScripts.selectedTitle", { count: targets === "all" ? 0 : targets.length })}</AlertDialogTitle>
          <AlertDialogDescription>{t(targets === "all" ? "webAccountScripts.allDescription" : "webAccountScripts.selectedDescription")}</AlertDialogDescription>
        </AlertDialogHeader>
        <div className="space-y-2">
          <p className="text-xs font-medium">{t("webAccountScripts.operations")}</p>
          <div className="space-y-1 rounded-md bg-muted/40 p-1">
            {items.map(({ action, icon: Icon, label, description, locked }) => (
              <label
                key={action}
                className={cn(
                  "flex items-start gap-3 rounded-sm px-3 py-2.5 transition-colors",
                  pending || locked ? "cursor-not-allowed opacity-70" : "cursor-pointer hover:bg-background/70",
                )}
              >
                <Checkbox
                  className="mt-0.5"
                  checked={actions[action]}
                  disabled={pending || locked}
                  onCheckedChange={(checked) => updateAction(action, checked === true)}
                />
                <Icon className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
                <span className="min-w-0 space-y-0.5">
                  <span className="block text-xs font-medium">{label}</span>
                  <span className="block text-xs text-muted-foreground">{description}</span>
                </span>
              </label>
            ))}
          </div>
        </div>
        <AlertDialogFooter>
          <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
          <AlertDialogAction
            disabled={pending || !hasAction}
            onClick={(event) => {
              event.preventDefault();
              onRun(actions);
            }}
          >
            {pending ? <Spinner /> : null}
            {pending && progress
              ? <span className="tabular-nums">{progress.completed} / {progress.total}</span>
              : t("webAccountScripts.run")}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
