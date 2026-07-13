import { ArrowDown, ArrowUp, ArrowUpDown } from "lucide-react";
import type { ComponentProps, ReactNode } from "react";
import { useTranslation } from "react-i18next";

import { TableHead } from "@/components/ui/table";
import { cn } from "@/shared/lib/cn";
import type { SortOrder } from "@/shared/lib/table-sort";

type SortableTableHeadProps = Omit<ComponentProps<typeof TableHead>, "children" | "onChange"> & {
  children: ReactNode;
  field: string;
  sortBy: string;
  sortOrder: SortOrder;
  initialOrder?: SortOrder;
  align?: "left" | "center" | "right";
  onSort: (field: string, initialOrder: SortOrder) => void;
};

export function SortableTableHead({ children, field, sortBy, sortOrder, initialOrder = "asc", align = "left", className, onSort, ...props }: SortableTableHeadProps) {
  const { t } = useTranslation();
  const active = sortBy === field;
  const nextOrder = active && sortOrder === "asc" ? "desc" : active ? "asc" : initialOrder;
  const Icon = active ? sortOrder === "asc" ? ArrowUp : ArrowDown : ArrowUpDown;

  return (
    <TableHead
      aria-sort={active ? sortOrder === "asc" ? "ascending" : "descending" : "none"}
      className={className}
      {...props}
    >
      <button
        type="button"
        className={cn(
          "group/sort -mx-1 inline-flex h-8 max-w-full items-center gap-1 px-1 text-left transition-colors hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/40",
          align === "center" && "w-full justify-center text-center",
          align === "right" && "ml-auto flex w-full justify-end text-right",
        )}
        aria-label={t(nextOrder === "asc" ? "common.sortAscending" : "common.sortDescending", { column: typeof children === "string" ? children : "" })}
        onClick={() => onSort(field, initialOrder)}
      >
        <span className="truncate">{children}</span>
        <Icon className={cn("size-3 shrink-0", active ? "text-foreground" : "text-muted-foreground/45 group-hover/sort:text-muted-foreground")} aria-hidden />
      </button>
    </TableHead>
  );
}
