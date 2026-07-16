import { useQuery } from "@tanstack/react-query";
import { Database, Image as ImageIcon, RefreshCw, Search } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { getImageStats, listImages } from "@/features/media/media-api";
import { MediaMetric } from "@/features/media/media-metric";
import type { MediaAssetDTO } from "@/features/media/types";
import { EmptyState, ErrorState, LoadingState } from "@/shared/components/data-state";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { PageHeader } from "@/shared/components/page-header";
import { Pagination } from "@/shared/components/pagination";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { formatDateTime, formatNumber } from "@/shared/lib/format";

export function GalleryPage() {
  const { t, i18n } = useTranslation();
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [search, setSearch] = useState("");
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

  function refreshAll(): void {
    void imagesQuery.refetch();
    void statsQuery.refetch();
  }

  return (
    <div className="space-y-8">
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

      <section className="grid grid-cols-[repeat(auto-fit,minmax(12rem,1fr))] gap-2">
        <MediaMetric icon={ImageIcon} loading={statsQuery.isPending} label={t("media.images.totalImages")} value={formatNumber(statsQuery.data?.totalImages ?? 0, i18n.language, 0)} detail={t("media.images.totalImagesDetail")} />
        <MediaMetric icon={Database} loading={statsQuery.isPending} label={t("media.images.totalBytes")} value={formatBytes(statsQuery.data?.totalBytes ?? 0, i18n.language)} detail={t("media.images.totalBytesDetail")} />
      </section>

      <DataTableShell
        toolbar={(
          <>
          <div className="relative w-full sm:w-80">
            <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
            <Input
              className="h-8 pl-9 text-xs"
              value={search}
              onChange={(event) => { setSearch(event.target.value); setPage(1); }}
              placeholder={t("media.images.search")}
              aria-label={t("media.images.search")}
            />
          </div>
          {result ? <span className="text-xs text-muted-foreground">{t("media.images.pageSummary", { count: result.items.length, total: result.total })}</span> : null}
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
        {imagesQuery.isPending ? <LoadingState /> : null}
        {!imagesQuery.isPending && result && result.items.length === 0 ? <EmptyState message={t(normalizedSearch ? "media.images.noMatches" : "media.images.empty")} /> : null}

        {!imagesQuery.isPending && result && result.items.length > 0 ? (
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
            {result.items.map((image) => <ImageCard key={image.id} image={image} locale={i18n.language} />)}
          </div>
        ) : null}

      </DataTableShell>
    </div>
  );
}

function ImageCard({ image, locale }: { image: MediaAssetDTO; locale: string }) {
  const { t } = useTranslation();
  // 管理端图库与 API 同源，使用相对路径避免依赖未配置或仅对外可用的公共地址。
  const imageURL = `/v1/media/images/${encodeURIComponent(image.id)}`;
  return (
    <a href={imageURL} target="_blank" rel="noreferrer" className="group overflow-hidden rounded-lg bg-card transition-colors hover:bg-secondary/45 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/40">
      <div className="aspect-square overflow-hidden bg-muted">
        <img src={imageURL} alt={image.id} loading="lazy" className="size-full object-cover transition-transform duration-200 group-hover:scale-[1.02]" />
      </div>
      <div className="space-y-2 p-3 text-xs">
        <div className="flex min-w-0 items-center justify-between gap-2">
          <span className="min-w-0 flex-1 truncate font-medium" title={image.id}>{image.id}</span>
          <span className="shrink-0 text-muted-foreground">{formatBytes(image.sizeBytes, locale)}</span>
        </div>
        <div className="flex min-w-0 items-center justify-between gap-2 text-[11px] text-muted-foreground">
          <span className="min-w-0 truncate" title={image.mimeType}>{image.mimeType || image.kind}</span>
          <span className="shrink-0 whitespace-nowrap">{formatDateTime(image.createdAt, locale)}</span>
        </div>
        <div className="truncate font-mono text-[10px] text-muted-foreground/75" title={image.sha256}>{t("media.images.sha256")}: {image.sha256}</div>
      </div>
    </a>
  );
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
