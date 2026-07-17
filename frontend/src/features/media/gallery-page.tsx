import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowUpRight, Database, Image as ImageIcon, RefreshCw, Search, Trash2 } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { deleteImages, getImageStats, listImages } from "@/features/media/media-api";
import type { MediaAssetDTO } from "@/features/media/types";
import { ErrorState } from "@/shared/components/data-state";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { PageHeader } from "@/shared/components/page-header";
import { Pagination } from "@/shared/components/pagination";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { cn } from "@/shared/lib/cn";
import { formatDateTime, formatNumber } from "@/shared/lib/format";

export function GalleryPage() {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [search, setSearch] = useState("");
  const [selected, setSelected] = useState<Set<string>>(() => new Set());
  const [deleteOpen, setDeleteOpen] = useState(false);
  const debouncedSearch = useDebouncedValue(search);
  const normalizedSearch = debouncedSearch.trim();

  const imagesQuery = useQuery({
    queryKey: ["media", "images", page, pageSize, normalizedSearch],
    queryFn: () => listImages({ page, pageSize, search: normalizedSearch || undefined }),
  });
  const statsQuery = useQuery({
    queryKey: ["media", "images", "stats"],
    queryFn: getImageStats,
    staleTime: 30_000,
  });

  const result = imagesQuery.data;
  const refreshing = imagesQuery.isFetching || statsQuery.isFetching;
  const pageIDs = result?.items.map((image) => image.id) ?? [];
  const selectedOnPage = pageIDs.filter((id) => selected.has(id));
  const allPageSelected = pageIDs.length > 0 && selectedOnPage.length === pageIDs.length;

  const deleteMutation = useMutation({
    mutationFn: () => deleteImages([...selected]),
    onSuccess: (deleteResult) => {
      if (result && selectedOnPage.length === result.items.length && page > 1) setPage(page - 1);
      setSelected(new Set());
      setDeleteOpen(false);
      void queryClient.invalidateQueries({ queryKey: ["media", "images"] });
      toast.success(t("media.images.deleted", { count: deleteResult.deleted }));
    },
    onError: (error) => {
      void queryClient.invalidateQueries({ queryKey: ["media", "images"] });
      toast.error(error instanceof Error ? error.message : t("errors.generic"));
    },
  });

  function refreshAll(): void {
    void imagesQuery.refetch();
    void statsQuery.refetch();
  }

  function togglePage(checked: boolean): void {
    setSelected((current) => {
      const next = new Set(current);
      for (const id of pageIDs) {
        if (checked) next.add(id);
        else next.delete(id);
      }
      return next;
    });
  }

  function toggleImage(id: string, checked: boolean): void {
    setSelected((current) => {
      const next = new Set(current);
      if (checked) next.add(id);
      else next.delete(id);
      return next;
    });
  }

  return (
    <div className="space-y-5">
      <PageHeader
        title={t("media.images.title")}
        description={t("media.images.description")}
        actions={(
          <Button variant="secondary" size="sm" onClick={refreshAll} disabled={refreshing}>
            <RefreshCw className={refreshing ? "animate-spin" : undefined} />
            {t("common.refresh")}
          </Button>
        )}
      />

      <DataTableShell
        toolbar={(
          <>
            <div className="flex w-full min-w-0 items-center gap-3 sm:w-auto">
              <Checkbox checked={allPageSelected ? true : selectedOnPage.length > 0 ? "indeterminate" : false} onCheckedChange={(checked) => togglePage(checked === true)} aria-label={t("common.selectPage")} />
              <div className="relative min-w-0 flex-1 sm:w-72 sm:flex-none">
                <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                <Input
                  className="h-8 pl-9 text-xs"
                  value={search}
                  onChange={(event) => { setSearch(event.target.value); setPage(1); }}
                  placeholder={t("media.images.search")}
                  aria-label={t("media.images.search")}
                />
              </div>
              {normalizedSearch && result ? <span className="hidden whitespace-nowrap text-xs tabular-nums text-muted-foreground md:inline">{t("media.images.pageSummary", { count: result.items.length, total: result.total })}</span> : null}
            </div>
            {selected.size > 0 ? (
              <div className="flex h-8 items-center gap-2">
                <span className="text-xs text-muted-foreground">{t("common.selectedCount", { count: selected.size })}</span>
                <Button variant="secondary" size="sm" className="text-destructive hover:text-destructive" onClick={() => setDeleteOpen(true)}><Trash2 />{t("common.delete")}</Button>
              </div>
            ) : (
              <GallerySummary
                loading={statsQuery.isPending}
                unavailable={statsQuery.isError}
                totalImages={statsQuery.data?.totalImages ?? 0}
                totalBytes={statsQuery.data?.totalBytes ?? 0}
                locale={i18n.language}
              />
            )}
          </>
        )}
        footer={result && result.total > 0 ? (
          <Pagination
            page={result.page}
            pageSize={result.pageSize}
            total={result.total}
            onPageChange={setPage}
            onPageSizeChange={(value) => { setPageSize(value); setPage(1); }}
          />
        ) : undefined}
      >
        {imagesQuery.isError ? <ErrorState message={imagesQuery.error.message} onRetry={() => void imagesQuery.refetch()} /> : null}
        {imagesQuery.isPending ? <ImageGridLoading /> : null}
        {!imagesQuery.isPending && result && result.items.length === 0 ? <GalleryEmptyState message={t(normalizedSearch ? "media.images.noMatches" : "media.images.empty")} /> : null}

        {!imagesQuery.isPending && result && result.items.length > 0 ? (
          <div className="grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-4 2xl:grid-cols-5">
            {result.items.map((image) => <ImageCard key={image.id} image={image} locale={i18n.language} selectionMode={selected.size > 0} selected={selected.has(image.id)} onSelectedChange={(checked) => toggleImage(image.id, checked)} />)}
          </div>
        ) : null}
      </DataTableShell>

      <AlertDialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("media.images.deleteTitle", { count: selected.size })}</AlertDialogTitle>
            <AlertDialogDescription>{t("media.images.deleteDescription")}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction className="bg-destructive text-white hover:bg-destructive/90" disabled={deleteMutation.isPending} onClick={() => deleteMutation.mutate()}>
              {deleteMutation.isPending ? <Spinner /> : <Trash2 />}
              {t("common.delete")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function ImageCard({ image, locale, selectionMode, selected, onSelectedChange }: { image: MediaAssetDTO; locale: string; selectionMode: boolean; selected: boolean; onSelectedChange: (checked: boolean) => void }) {
  const { t } = useTranslation();
  // 管理端图库与 API 同源，使用相对路径避免依赖未配置或仅对外可用的公共地址。
  const imageURL = `/v1/media/images/${encodeURIComponent(image.id)}`;
  return (
    <article className="group relative min-w-0">
      <Checkbox
        checked={selected}
        onCheckedChange={(checked) => onSelectedChange(checked === true)}
        aria-label={t("common.selectItem", { name: image.id })}
        className={cn("absolute left-2 top-2 z-10 bg-background/90 shadow-sm backdrop-blur-sm transition-opacity md:opacity-0 md:group-hover:opacity-100 md:group-focus-within:opacity-100", selected && "md:opacity-100")}
      />
      <a
        href={imageURL}
        target="_blank"
        rel="noreferrer"
        aria-label={selectionMode ? t("common.selectItem", { name: image.id }) : t("media.images.openImage", { id: image.id })}
        className={cn("block min-w-0 rounded-lg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/40", selectionMode && "cursor-pointer")}
        onClick={(event) => {
          if (!selectionMode) return;
          event.preventDefault();
          onSelectedChange(!selected);
        }}
      >
        <div className="relative aspect-square overflow-hidden rounded-lg bg-muted">
          <img src={imageURL} alt={image.id} loading="lazy" decoding="async" className="size-full object-cover transition-transform duration-300 ease-out group-hover:scale-[1.025]" />
          {selected ? <span className="pointer-events-none absolute inset-0 rounded-lg ring-1 ring-inset ring-primary/70" aria-hidden="true" /> : null}
          {!selectionMode ? (
            <span className="absolute right-2 top-2 flex size-7 items-center justify-center rounded-full bg-background/85 text-foreground opacity-0 shadow-sm backdrop-blur-sm transition-opacity group-hover:opacity-100 group-focus-within:opacity-100">
              <ArrowUpRight className="size-3.5" />
            </span>
          ) : null}
        </div>
        <div className="space-y-1 px-0.5 pt-2.5 text-xs">
          <div className="flex min-w-0 items-center justify-between gap-2">
            <span className="min-w-0 flex-1 truncate font-medium" title={image.id}>{image.id}</span>
            <span className="shrink-0 text-[11px] tabular-nums text-muted-foreground">{formatBytes(image.sizeBytes, locale)}</span>
          </div>
          <div className="flex min-w-0 items-center justify-between gap-2 text-[11px] text-muted-foreground">
            <span className="min-w-0 truncate uppercase" title={image.mimeType}>{formatMediaType(image)}</span>
            <span className="shrink-0 whitespace-nowrap">{formatDateTime(image.createdAt, locale)}</span>
          </div>
        </div>
      </a>
    </article>
  );
}

function GallerySummary({ loading, unavailable, totalImages, totalBytes, locale }: { loading: boolean; unavailable: boolean; totalImages: number; totalBytes: number; locale: string }) {
  const { t } = useTranslation();
  return (
    <div className="flex h-8 items-center gap-4 whitespace-nowrap text-xs" aria-busy={loading}>
      <span className="inline-flex items-center gap-1.5">
        <ImageIcon className="size-3.5 text-emerald-600 dark:text-emerald-400" />
        <span className="text-muted-foreground">{t("media.images.totalImages")}</span>
        <strong className="font-medium tabular-nums">{loading ? <Spinner className="size-3" /> : unavailable ? "-" : formatNumber(totalImages, locale, 0)}</strong>
      </span>
      <span className="h-3 w-px bg-border" aria-hidden="true" />
      <span className="inline-flex items-center gap-1.5">
        <Database className="size-3.5 text-sky-600 dark:text-sky-400" />
        <span className="text-muted-foreground">{t("media.images.totalBytes")}</span>
        <strong className="font-medium tabular-nums">{loading ? <Spinner className="size-3" /> : unavailable ? "-" : formatBytes(totalBytes, locale)}</strong>
      </span>
    </div>
  );
}

function GalleryEmptyState({ message }: { message: string }) {
  return (
    <div className="flex min-h-72 flex-col items-center justify-center gap-3 text-center text-muted-foreground">
      <span className="flex size-10 items-center justify-center rounded-full bg-muted/70">
        <ImageIcon className="size-5 stroke-1.5" />
      </span>
      <p className="text-sm">{message}</p>
    </div>
  );
}

function ImageGridLoading() {
  return (
    <div className="grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-4 2xl:grid-cols-5" aria-hidden="true">
      {Array.from({ length: 10 }, (_, index) => (
        <div key={index} className="min-w-0">
          <div className="aspect-square animate-pulse rounded-lg bg-muted" />
          <div className="space-y-2 px-0.5 pt-2.5">
            <div className="h-3 w-3/4 animate-pulse rounded bg-muted" />
            <div className="h-2.5 w-1/2 animate-pulse rounded bg-muted" />
          </div>
        </div>
      ))}
    </div>
  );
}

function formatMediaType(image: MediaAssetDTO): string {
  const subtype = image.mimeType.split("/")[1]?.split(";")[0]?.trim();
  return subtype || image.kind || "-";
}

function formatBytes(value: number, locale: string): string {
  if (!Number.isFinite(value) || value <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let size = value;
  let unitIndex = 0;
  while (size >= 1024 && unitIndex < units.length - 1) {
    size /= 1024;
    unitIndex += 1;
  }
  return `${new Intl.NumberFormat(locale, { maximumFractionDigits: unitIndex === 0 || size >= 10 ? 0 : 1 }).format(size)} ${units[unitIndex]}`;
}
