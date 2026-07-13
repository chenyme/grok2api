import { LoaderCircle } from "lucide-react";
import type { ComponentProps } from "react";
import { useTranslation } from "react-i18next";

import { cn } from "@/shared/lib/cn";

function Spinner({ className, ...props }: ComponentProps<typeof LoaderCircle>) {
  const { t } = useTranslation();

  return (
    <LoaderCircle
      role="status"
      aria-label={t("common.loading")}
      className={cn("size-4 animate-spin text-muted-foreground", className)}
      {...props}
    />
  );
}

export { Spinner };
