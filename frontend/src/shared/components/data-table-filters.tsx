import { ListFilter, X } from "lucide-react";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuRadioGroup, DropdownMenuRadioItem, DropdownMenuSeparator, DropdownMenuSub, DropdownMenuSubContent, DropdownMenuSubTrigger, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";

type DataTableOptionFilter = {
  id: string;
  label: string;
  value: string;
  options: Array<{ value: string; label: string }>;
  onChange: (value: string) => void;
};

type DataTableTextFilter = {
  id: string;
  label: string;
  value: string;
  placeholder?: string;
  onChange: (value: string) => void;
  type: "text";
};

export type DataTableFilter = DataTableOptionFilter | DataTableTextFilter;

export function DataTableFilters({ filters }: { filters: DataTableFilter[] }) {
  const { t } = useTranslation();
  const activeCount = filters.filter((filter) => filter.value !== "").length;

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="secondary" size="sm" className="text-muted-foreground">
          <ListFilter />
          {t("common.filter")}
          {activeCount > 0 ? <span className="min-w-4 text-center text-[11px] tabular-nums text-foreground">{activeCount}</span> : null}
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" className="w-52">
        {filters.map((filter) => {
          if (!("options" in filter)) {
            return (
              <DropdownMenuSub key={filter.id}>
                <DropdownMenuSubTrigger>
                  <span>{filter.label}</span>
                  {filter.value ? <span className="max-w-20 truncate text-xs text-muted-foreground">{filter.value}</span> : null}
                </DropdownMenuSubTrigger>
                <DropdownMenuSubContent className="w-64 p-2" onKeyDown={(event) => event.stopPropagation()}>
                  <Input
                    id={`table-filter-${filter.id}`}
                    className="h-8 text-xs"
                    value={filter.value}
                    placeholder={filter.placeholder}
                    aria-label={filter.label}
                    onChange={(event) => filter.onChange(event.target.value)}
                  />
                </DropdownMenuSubContent>
              </DropdownMenuSub>
            );
          }
          const selectedLabel = filter.options.find((option) => option.value === filter.value)?.label;
          return (
            <DropdownMenuSub key={filter.id}>
              <DropdownMenuSubTrigger>
                <span>{filter.label}</span>
                {selectedLabel ? <span className="max-w-20 truncate text-xs text-muted-foreground">{selectedLabel}</span> : null}
              </DropdownMenuSubTrigger>
              <DropdownMenuSubContent className="w-48">
                <DropdownMenuRadioGroup value={filter.value || "__all"} onValueChange={(value) => filter.onChange(value === "__all" ? "" : value)}>
                  <DropdownMenuRadioItem value="__all">{t("common.all")}</DropdownMenuRadioItem>
                  {filter.options.map((option) => <DropdownMenuRadioItem key={option.value} value={option.value}>{option.label}</DropdownMenuRadioItem>)}
                </DropdownMenuRadioGroup>
              </DropdownMenuSubContent>
            </DropdownMenuSub>
          );
        })}
        {activeCount > 0 ? (
          <>
            <DropdownMenuSeparator />
            <DropdownMenuItem onSelect={() => filters.forEach((filter) => filter.onChange(""))}><X />{t("common.clearFilters")}</DropdownMenuItem>
          </>
        ) : null}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
