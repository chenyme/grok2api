import { ChevronsLeft, ChevronsRight, ChevronLeft, ChevronRight } from "lucide-react";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { cn } from "@/shared/lib/cn";

const PAGE_SIZE_OPTIONS = [20, 100, 500, 1000, 2000] as const;

export function Pagination({ page, pageSize, total, onPageChange, onPageSizeChange, className }: { page: number; pageSize: number; total: number; onPageChange: (page: number) => void; onPageSizeChange?: (pageSize: number) => void; className?: string }) {
  const { t } = useTranslation();
  const pages = Math.max(1, Math.ceil(total / pageSize));

  return (
    <div className={cn("flex w-full flex-wrap items-center justify-between gap-3", className)}>
      <div className="flex items-center gap-1">
        <Button variant="ghost" size="icon" className="size-8" disabled={page <= 1} onClick={() => onPageChange(1)} aria-label={t("common.firstPage")}><ChevronsLeft /></Button>
        <Button variant="ghost" size="icon" className="size-8" disabled={page <= 1} onClick={() => onPageChange(page - 1)} aria-label={t("common.previousPage")}><ChevronLeft /></Button>
        <span className="min-w-20 px-2 text-center text-xs text-muted-foreground">{t("common.pageOf", { page, pages })}</span>
        <Button variant="ghost" size="icon" className="size-8" disabled={page >= pages} onClick={() => onPageChange(page + 1)} aria-label={t("common.nextPage")}><ChevronRight /></Button>
        <Button variant="ghost" size="icon" className="size-8" disabled={page >= pages} onClick={() => onPageChange(pages)} aria-label={t("common.lastPage")}><ChevronsRight /></Button>
      </div>
      {onPageSizeChange ? <PageSizeSelector pageSize={pageSize} onChange={onPageSizeChange} /> : <span />}
    </div>
  );
}

export function CursorPagination({ page, pageSize, hasMore, disabled = false, onFirstPage, onPreviousPage, onNextPage, onPageSizeChange, className }: { page: number; pageSize: number; hasMore: boolean; disabled?: boolean; onFirstPage: () => void; onPreviousPage: () => void; onNextPage: () => void; onPageSizeChange: (pageSize: number) => void; className?: string }) {
  const { t } = useTranslation();

  return (
    <div className={cn("flex w-full flex-wrap items-center justify-between gap-3", className)}>
      <div className="flex items-center gap-1">
        <Button variant="ghost" size="icon" className="size-8" disabled={disabled || page <= 1} onClick={onFirstPage} aria-label={t("common.firstPage")}><ChevronsLeft /></Button>
        <Button variant="ghost" size="icon" className="size-8" disabled={disabled || page <= 1} onClick={onPreviousPage} aria-label={t("common.previousPage")}><ChevronLeft /></Button>
        <span className="min-w-20 px-2 text-center text-xs text-muted-foreground">{t("audits.cursorPage", { page })}</span>
        <Button variant="ghost" size="icon" className="size-8" disabled={disabled || !hasMore} onClick={onNextPage} aria-label={t("common.nextPage")}><ChevronRight /></Button>
      </div>
      <PageSizeSelector pageSize={pageSize} disabled={disabled} onChange={onPageSizeChange} />
    </div>
  );
}

function PageSizeSelector({ pageSize, disabled = false, onChange }: { pageSize: number; disabled?: boolean; onChange: (pageSize: number) => void }) {
  const { t } = useTranslation();
  return (
    <div className="flex items-center gap-2 text-xs text-muted-foreground">
      <span>{t("common.perPage")}</span>
      <Select value={String(pageSize)} disabled={disabled} onValueChange={(value) => onChange(Number(value))}>
        <SelectTrigger className="h-8 w-[76px] rounded-md bg-secondary px-3 text-xs shadow-none" aria-label={t("common.perPage")}><SelectValue /></SelectTrigger>
        <SelectContent align="end">
          {PAGE_SIZE_OPTIONS.map((value) => <SelectItem key={value} value={String(value)}>{value}</SelectItem>)}
        </SelectContent>
      </Select>
      <span>{t("common.rows")}</span>
    </div>
  );
}
