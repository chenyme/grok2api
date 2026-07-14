import { Check, Copy } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { copyToClipboard } from "@/shared/clipboard";
import { cn } from "@/shared/lib/cn";

export function CopyButton({
  value,
  className,
  disabled,
  onCopied,
  onError,
}: {
  value: string;
  className?: string;
  disabled?: boolean;
  onCopied?: () => void;
  onError?: (error: unknown) => void;
}) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);

  async function handleClick() {
    const ok = await copyToClipboard(value);
    if (ok) {
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
      onCopied?.();
    } else {
      const message = t("common.copyFailed");
      toast.error(message);
      onError?.(new Error(message));
    }
  }

  const label = copied ? t("common.copied") : t("common.copy");
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Button type="button" variant="ghost" size="icon" className={cn("size-7 shrink-0 text-muted-foreground", className)} aria-label={label} disabled={disabled} onClick={handleClick}>
          {copied ? <Check /> : <Copy />}
        </Button>
      </TooltipTrigger>
      <TooltipContent>{label}</TooltipContent>
    </Tooltip>
  );
}
