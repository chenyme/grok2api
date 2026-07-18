import { Cake, FileCheck2, Settings2, ShieldCheck } from "lucide-react";
import { useTranslation } from "react-i18next";

import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { DropdownMenuItem, DropdownMenuSub, DropdownMenuSubContent, DropdownMenuSubTrigger } from "@/components/ui/dropdown-menu";
import { Spinner } from "@/components/ui/spinner";
import type { AccountDTO } from "@/features/accounts/accounts-api";

export type WebAccountConfirmationAction = "acceptTerms" | "setBirthDate" | "enableNSFW";

export type WebAccountConfirmationTarget = {
  account: AccountDTO;
  action: WebAccountConfirmationAction;
};

type MenuProps = {
  account: AccountDTO;
  disabled: boolean;
  onConfirm: (target: WebAccountConfirmationTarget) => void;
};

export function WebAccountSettingsMenu({ account, disabled, onConfirm }: MenuProps) {
  const { t } = useTranslation();
  return (
    <DropdownMenuSub>
      <DropdownMenuSubTrigger disabled={disabled}>
        <Settings2 />
        {t("webAccountSettings.menu")}
      </DropdownMenuSubTrigger>
      <DropdownMenuSubContent>
        <DropdownMenuItem onClick={() => onConfirm({ account, action: "acceptTerms" })}>
          <FileCheck2 />
          {t("webAccountSettings.acceptTerms")}
        </DropdownMenuItem>
        <DropdownMenuItem onClick={() => onConfirm({ account, action: "setBirthDate" })}>
          <Cake />
          {t("webAccountSettings.setBirthDate")}
        </DropdownMenuItem>
        <DropdownMenuItem onClick={() => onConfirm({ account, action: "enableNSFW" })}>
          <ShieldCheck />
          {t("webAccountSettings.enableNSFW")}
        </DropdownMenuItem>
      </DropdownMenuSubContent>
    </DropdownMenuSub>
  );
}

type DialogsProps = {
  confirmationTarget: WebAccountConfirmationTarget | null;
  confirmationPending: boolean;
  onConfirmationClose: () => void;
  onConfirm: (target: WebAccountConfirmationTarget) => void;
};

export function WebAccountSettingsDialogs({
  confirmationTarget,
  confirmationPending,
  onConfirmationClose,
  onConfirm,
}: DialogsProps) {
  const { t } = useTranslation();
  const action = confirmationTarget?.action ?? "acceptTerms";
  const titleKey = action === "enableNSFW"
    ? "webAccountSettings.enableNSFWTitle"
    : action === "setBirthDate"
      ? "webAccountSettings.setBirthDateTitle"
      : "webAccountSettings.acceptTermsTitle";
  const descriptionKey = action === "enableNSFW"
    ? "webAccountSettings.enableNSFWDescription"
    : action === "setBirthDate"
      ? "webAccountSettings.setBirthDateDescription"
      : "webAccountSettings.acceptTermsDescription";
  const actionKey = action === "enableNSFW"
    ? "webAccountSettings.enableNSFW"
    : action === "setBirthDate"
      ? "webAccountSettings.setBirthDate"
      : "webAccountSettings.acceptTerms";

  return (
    <AlertDialog
      open={confirmationTarget !== null}
      onOpenChange={(open) => {
        if (!open && !confirmationPending) onConfirmationClose();
      }}
    >
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{t(titleKey)}</AlertDialogTitle>
          <AlertDialogDescription>{t(descriptionKey)}</AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel disabled={confirmationPending}>{t("common.cancel")}</AlertDialogCancel>
          <AlertDialogAction
            disabled={confirmationPending || confirmationTarget === null}
            onClick={(event) => {
              event.preventDefault();
              if (confirmationTarget) onConfirm(confirmationTarget);
            }}
          >
            {confirmationPending ? <Spinner /> : null}
            {t(actionKey)}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
