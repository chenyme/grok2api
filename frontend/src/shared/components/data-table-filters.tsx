import { Check, ListFilter, Search, X } from "lucide-react";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuLabel, DropdownMenuRadioGroup, DropdownMenuRadioItem, DropdownMenuSeparator, DropdownMenuSub, DropdownMenuSubContent, DropdownMenuSubTrigger, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";

// Groups turn an option into a third menu level: the option itself stays
// selectable as the unnarrowed value, and every group entry narrows it further.
type DataTableFilterOptionGroup = {
  id: string;
  label: string;
  options: Array<{ value: string; label: string; description?: string; badge?: string }>;
  emptyLabel?: string;
  loading?: boolean;
  hasMore?: boolean;
  actionLabel?: string;
  onAction?: () => void;
  noteLabel?: string;
  hideLabel?: boolean;
  // 名单内容超出该高度时内部滚动（例如 max-h-40 = 5 行），避免菜单被拉得很长。
  maxHeightClassName?: string;
};

type DataTableFilterOption = {
  value: string;
  label: string;
  groups?: DataTableFilterOptionGroup[];
  onGroupsOpenChange?: (open: boolean) => void;
  groupSearch?: { value: string; placeholder: string; onChange: (value: string) => void };
};

type DataTableOptionFilter = {
  id: string;
  label: string;
  value: string;
  options: DataTableFilterOption[];
  onChange: (value: string) => void;
  selectedLabel?: string;
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
export type { DataTableFilterOption, DataTableFilterOptionGroup };

function findGroupLabel(groups: DataTableFilterOptionGroup[], value: string): string | undefined {
  if (!value) return undefined;
  for (const group of groups) {
    const match = group.options.find((entry) => entry.value === value);
    if (match) return match.label;
  }
  return undefined;
}

function findSelectedLabel(options: DataTableFilterOption[], value: string): string | undefined {
  if (!value) return undefined;
  for (const option of options) {
    if (option.value === value) return option.label;
    const nested = option.groups ? findGroupLabel(option.groups, value) : undefined;
    if (nested) return nested;
  }
  return undefined;
}

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
          const selectedLabel = filter.selectedLabel ?? findSelectedLabel(filter.options, filter.value);
          return (
            <DropdownMenuSub key={filter.id}>
              <DropdownMenuSubTrigger>
                <span>{filter.label}</span>
                {selectedLabel ? <span className="max-w-20 truncate text-xs text-muted-foreground">{selectedLabel}</span> : null}
              </DropdownMenuSubTrigger>
              <DropdownMenuSubContent className="w-52">
                <DropdownMenuRadioGroup value={filter.value || "__all"} onValueChange={(value) => filter.onChange(value === "__all" ? "" : value)}>
                  <DropdownMenuRadioItem value="__all">{t("common.all")}</DropdownMenuRadioItem>
                  {filter.options.map((option) => {
                    if (!option.groups || option.groups.length === 0) {
                      return <DropdownMenuRadioItem key={option.value} value={option.value}>{option.label}</DropdownMenuRadioItem>;
                    }
                    const narrowedLabel = option.value === filter.value ? null : findGroupLabel(option.groups, filter.value);
                    return (
                      <DropdownMenuSub key={option.value} onOpenChange={option.onGroupsOpenChange}>
                        <DropdownMenuSubTrigger className="pr-2">
                          <span className="shrink-0 whitespace-nowrap">{option.label}</span>
                          {narrowedLabel ? <span className="ml-auto max-w-16 truncate text-xs text-muted-foreground">{narrowedLabel}</span> : null}
                          {option.value === filter.value ? <Check className="ml-auto" /> : null}
                        </DropdownMenuSubTrigger>
                        <DropdownMenuSubContent sideOffset={6} className="max-h-[min(26rem,calc(100vh-2rem))] w-72 max-w-[calc(100vw-1rem)] overflow-y-auto p-0 shadow-lg shadow-black/5">
                          {option.groupSearch ? (
                            <div className="sticky top-0 z-20 border-b bg-popover/95 px-2 py-1.5 backdrop-blur-sm" onKeyDown={(event) => event.stopPropagation()}>
                              <div className="relative">
                                <Search className="pointer-events-none absolute left-2 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground/70" />
                                <Input
                                  className="h-8 border-0 bg-transparent pl-7 text-xs shadow-none focus-visible:ring-0"
                                  value={option.groupSearch.value}
                                  placeholder={option.groupSearch.placeholder}
                                  aria-label={option.groupSearch.placeholder}
                                  onChange={(event) => option.groupSearch?.onChange(event.target.value)}
                                />
                              </div>
                            </div>
                          ) : null}
                          <DropdownMenuRadioGroup value={filter.value} onValueChange={filter.onChange}>
                            <DropdownMenuRadioItem value={option.value} className="mx-1 my-1 min-h-8 text-xs focus:bg-muted/50">{t("common.all")}</DropdownMenuRadioItem>
                            {option.groups.map((group) => (
                              <div key={group.id}>
                                <DropdownMenuSeparator />
                                {!group.hideLabel ? <DropdownMenuLabel className="px-3 py-1 text-[10px] font-normal text-muted-foreground">{group.label}</DropdownMenuLabel> : null}
                                <div className={group.maxHeightClassName}>
                                  {group.options.length === 0
                                    ? <DropdownMenuItem disabled className="text-xs text-muted-foreground">{group.emptyLabel ?? t("common.noData")}</DropdownMenuItem>
                                    : group.options.map((entry) => (
                                      <DropdownMenuRadioItem
                                        key={entry.value}
                                        value={entry.value}
                                        className={entry.description || entry.badge ? "mx-1 min-w-0 items-start py-1.5 pr-7 focus:bg-muted/50 data-[state=checked]:bg-muted/30" : "mx-1"}
                                      >
                                        {entry.description || entry.badge ? (
                                          <span className="min-w-0 flex-1">
                                            <span className="block truncate text-[13px] font-normal text-foreground">{entry.label}</span>
                                            <span className="mt-px flex min-w-0 items-center gap-1 text-[10px] leading-4 text-muted-foreground/80">
                                              {entry.description ? <span className="truncate tabular-nums">{entry.description}</span> : null}
                                              {entry.description && entry.badge ? <span aria-hidden="true">·</span> : null}
                                              {entry.badge ? <span className="shrink-0">{entry.badge}</span> : null}
                                            </span>
                                          </span>
                                        ) : entry.label}
                                      </DropdownMenuRadioItem>
                                    ))}
                                </div>
                                {group.noteLabel ? <div className="border-t px-3 py-1.5 text-[10px] leading-4 text-muted-foreground/80">{group.noteLabel}</div> : null}
                                {group.hasMore && group.onAction ? (
                                  <DropdownMenuItem
                                    disabled={group.loading}
                                    onSelect={(event) => {
                                      event.preventDefault();
                                      group.onAction?.();
                                    }}
                                  >
                                    {group.actionLabel ?? t("common.nextPage")}
                                  </DropdownMenuItem>
                                ) : null}
                              </div>
                            ))}
                          </DropdownMenuRadioGroup>
                        </DropdownMenuSubContent>
                      </DropdownMenuSub>
                    );
                  })}
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
