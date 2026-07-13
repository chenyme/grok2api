import type { ReactNode } from "react";

import { cn } from "@/shared/lib/cn";

type DataTableShellProps = {
  toolbar: ReactNode;
  children: ReactNode;
  footer?: ReactNode;
  className?: string;
};

export function DataTableShell({ toolbar, children, footer, className }: DataTableShellProps) {
  return (
    <section className={cn("flex min-w-0 flex-col gap-4", className)}>
      <div className="flex min-h-12 flex-wrap items-center justify-between gap-3 py-2">
        {toolbar}
      </div>
      <div className="min-w-0">
        {children}
      </div>
      {footer ? (
        <div className="flex min-h-12 items-center py-2">
          {footer}
        </div>
      ) : null}
    </section>
  );
}
