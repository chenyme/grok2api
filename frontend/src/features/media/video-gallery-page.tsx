import { useQuery } from "@tanstack/react-query";
import { AlertCircle, CheckCircle2, Clock, ListVideo, Loader2, RefreshCw, Search } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHeader, TableRow } from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { downloadVideoContent, getVideoStats, listVideos } from "@/features/media/media-api";
import { MediaMetric } from "@/features/media/media-metric";
import type { MediaJobDTO } from "@/features/media/types";
import { EmptyState, ErrorState, LoadingState, TableLoadingRow } from "@/shared/components/data-state";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { PageHeader } from "@/shared/components/page-header";
import { Pagination } from "@/shared/components/pagination";
import { SortableTableHead } from "@/shared/components/sortable-table-head";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { cn } from "@/shared/lib/cn";
import { formatDateTime, formatNumber } from "@/shared/lib/format";
import { nextTableSort, type SortOrder, type TableSort } from "@/shared/lib/table-sort";

type VideoStatusFilter = MediaJobDTO["status"] | "";

const statusOptions: VideoStatusFilter[] = ["", "queued", "in_progress", "completed", "failed"];
const videoGalleryTimeZone = "Asia/Shanghai";

export function VideoGalleryPage() {
  const { t, i18n } = useTranslation();
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [search, setSearch] = useState("");
  const [statusFilter, setStatusFilter] = useState<VideoStatusFilter>("");
  const [sort, setSort] = useState<TableSort>({ field: "createdAt", order: "desc" });
  const [previewJob, setPreviewJob] = useState<MediaJobDTO | null>(null);
  const debouncedSearch = useDebouncedValue(search);
  const normalizedSearch = debouncedSearch.trim();

  const videosQuery = useQuery({
    queryKey: ["media", "videos", page, pageSize, statusFilter, normalizedSearch, sort.field, sort.order],
    queryFn: () => listVideos({ page, pageSize, status: statusFilter, search: normalizedSearch || undefined, sortBy: sort.field, sortOrder: sort.order }),
  });
  const statsQuery = useQuery({
    queryKey: ["media", "videos", "stats"],
    queryFn: getVideoStats,
    staleTime: 30_000,
  });

  const result = videosQuery.data;
  const refreshing = videosQuery.isFetching || statsQuery.isFetching;

  function refreshAll(): void {
    void videosQuery.refetch();
    void statsQuery.refetch();
  }

  function changeSort(field: string, initialOrder: SortOrder): void {
    setSort((current) => nextTableSort(current, field, initialOrder));
    setPage(1);
  }

  return (
    <div className="space-y-8">
      <PageHeader
        title={t("media.videos.title")}
        description={t("media.videos.description")}
        actions={(
          <Button variant="secondary" size="sm" onClick={refreshAll} disabled={refreshing}>
            <RefreshCw className={refreshing ? "animate-spin" : undefined} />
            {t("common.refresh")}
          </Button>
        )}
      />

      <section className="grid grid-cols-[repeat(auto-fit,minmax(12rem,1fr))] gap-2">
        <MediaMetric icon={ListVideo} loading={statsQuery.isPending} label={t("media.videos.totalJobs")} value={formatNumber(statsQuery.data?.totalJobs ?? 0, i18n.language, 0)} />
        <MediaMetric icon={Clock} loading={statsQuery.isPending} label={t("media.videos.queued")} value={formatNumber(statsQuery.data?.queued ?? 0, i18n.language, 0)} />
        <MediaMetric icon={Loader2} loading={statsQuery.isPending} label={t("media.videos.inProgress")} value={formatNumber(statsQuery.data?.inProgress ?? 0, i18n.language, 0)} />
        <MediaMetric icon={CheckCircle2} loading={statsQuery.isPending} label={t("media.videos.completed")} value={formatNumber(statsQuery.data?.completed ?? 0, i18n.language, 0)} />
        <MediaMetric icon={AlertCircle} loading={statsQuery.isPending} label={t("media.videos.failed")} value={formatNumber(statsQuery.data?.failed ?? 0, i18n.language, 0)} />
      </section>

      <DataTableShell
        toolbar={(
          <>
            <div className="flex w-full flex-wrap items-center gap-2 sm:w-auto">
              <div className="relative min-w-0 flex-1 sm:w-72 sm:flex-none">
                <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                <Input
                  className="h-8 pl-9 text-xs"
                  value={search}
                  onChange={(event) => { setSearch(event.target.value); setPage(1); }}
                  placeholder={t("media.videos.search")}
                  aria-label={t("media.videos.search")}
                />
              </div>
              <Select value={statusFilter || "all"} onValueChange={(value) => { setStatusFilter(value === "all" ? "" : value as VideoStatusFilter); setPage(1); }}>
                <SelectTrigger className="w-36" aria-label={t("media.videos.statusFilter")}>
                  <SelectValue />
                </SelectTrigger>
                <SelectContent align="start">
                  {statusOptions.map((status) => (
                    <SelectItem key={status || "all"} value={status || "all"}>{status ? t(`media.videoStatus.${status}`) : t("common.all")}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            {result ? <span className="text-xs text-muted-foreground">{t("media.videos.pageSummary", { count: result.items.length, total: result.total })}</span> : null}
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
        {videosQuery.isError ? <ErrorState message={videosQuery.error.message} onRetry={() => void videosQuery.refetch()} /> : null}
        {result && result.items.length === 0 ? <EmptyState message={t("media.videos.empty")} /> : null}
        {videosQuery.isPending || (result && result.items.length > 0) ? (
          <Table className="min-w-[1180px] table-fixed text-xs">
            <colgroup>
              <col className="w-[25%]" />
              <col className="w-[13%]" />
              <col className="w-[10%]" />
              <col className="w-[9%]" />
              <col className="w-[10%]" />
              <col className="w-[12%]" />
              <col className="w-[10%]" />
              <col className="w-[11%]" />
            </colgroup>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <SortableTableHead field="prompt" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("media.videos.prompt")}</SortableTableHead>
                <SortableTableHead field="model" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("media.videos.model")}</SortableTableHead>
                <SortableTableHead field="status" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort}>{t("media.videos.status")}</SortableTableHead>
                <SortableTableHead field="progress" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" align="center" onSort={changeSort}>{t("media.videos.progress")}</SortableTableHead>
                <SortableTableHead field="spec" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("media.videos.spec")}</SortableTableHead>
                <SortableTableHead field="account" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("media.videos.owner")}</SortableTableHead>
                <SortableTableHead field="createdAt" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("media.videos.createdAt")}</SortableTableHead>
                <SortableTableHead field="completedAt" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("media.videos.completedAt")}</SortableTableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {videosQuery.isPending ? <TableLoadingRow colSpan={8} /> : result?.items.map((job) => (
                <TableRow key={job.id}>
                  <TableCell className="min-w-0 py-3">
                    <div className="min-w-0">
                      <PromptCell prompt={job.prompt} />
                      <JobIdCell job={job} onPreview={() => setPreviewJob(job)} />
                      {job.errorMessage ? <span className="mt-1 block truncate text-[11px] text-destructive" title={job.errorMessage}>{job.errorMessage}</span> : null}
                    </div>
                  </TableCell>
                  <TableCell className="min-w-0 py-3"><span className="block truncate" title={job.model}>{job.model || "-"}</span></TableCell>
                  <TableCell className="py-3 text-center"><VideoStatusBadge status={job.status} /></TableCell>
                  <TableCell className="py-3 text-center"><ProgressValue value={job.progress} locale={i18n.language} /></TableCell>
                  <TableCell className="py-3">
                    <div className="space-y-0.5 text-xs">
                      <span className="block truncate" title={formatSpec(job)}>{formatSpec(job)}</span>
                      <span className="block text-[11px] text-muted-foreground">{t("media.videos.seconds", { count: job.seconds })}</span>
                    </div>
                  </TableCell>
                  <TableCell className="min-w-0 py-3">
                    <div className="min-w-0 space-y-0.5">
                      <span className="block truncate" title={job.accountName}>{job.accountName || "-"}</span>
                      <span className="block truncate text-[11px] text-muted-foreground" title={job.clientKeyName}>{job.clientKeyName || "-"}</span>
                    </div>
                  </TableCell>
                  <TableCell className="whitespace-nowrap py-3 text-xs text-muted-foreground">{formatDateTime(job.createdAt, i18n.language, videoGalleryTimeZone)}</TableCell>
                  <TableCell className="whitespace-nowrap py-3 text-xs text-muted-foreground">{formatDateTime(job.completedAt, i18n.language, videoGalleryTimeZone)}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        ) : null}
      </DataTableShell>

      {previewJob ? (
        <VideoPreviewDialog key={previewJob.id} job={previewJob} onOpenChange={(open) => { if (!open) setPreviewJob(null); }} />
      ) : null}
    </div>
  );
}

function PromptCell({ prompt }: { prompt: string }) {
  if (!prompt) {
    return <span className="block truncate text-xs font-medium">-</span>;
  }
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span className="block cursor-default truncate text-xs font-medium">{prompt}</span>
      </TooltipTrigger>
      <TooltipContent
        side="bottom"
        align="start"
        className="max-h-64 max-w-sm overflow-auto whitespace-pre-wrap break-words bg-popover px-3 py-2 text-left text-xs text-popover-foreground shadow-md"
      >
        {prompt}
      </TooltipContent>
    </Tooltip>
  );
}

function JobIdCell({ job, onPreview }: { job: MediaJobDTO; onPreview: () => void }) {
  const { t } = useTranslation();
  if (!job.previewAvailable) {
    return <span className="mt-0.5 block truncate font-mono text-[10px] text-muted-foreground" title={job.id}>{job.id}</span>;
  }
  return (
    <button
      type="button"
      className="mt-0.5 block max-w-full truncate text-left font-mono text-[10px] text-primary underline-offset-2 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
      title={t("media.videos.previewAction")}
      aria-label={t("media.videos.previewActionAria", { id: job.id })}
      onClick={onPreview}
    >
      {job.id}
    </button>
  );
}

function VideoPreviewDialog({ job, onOpenChange }: { job: MediaJobDTO; onOpenChange: (open: boolean) => void }) {
  const { t } = useTranslation();
  const objectUrlRef = useRef<string | null>(null);
  const [objectUrl, setObjectUrl] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;

    void downloadVideoContent(job.id)
      .then((blob) => {
        if (cancelled) return;
        const url = URL.createObjectURL(blob);
        objectUrlRef.current = url;
        setObjectUrl(url);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        const message = err instanceof Error && err.message ? err.message : t("media.videos.previewFailed");
        setError(message);
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });

    return () => {
      cancelled = true;
      if (objectUrlRef.current) {
        URL.revokeObjectURL(objectUrlRef.current);
        objectUrlRef.current = null;
      }
    };
  }, [job.id, t]);

  return (
    <Dialog open onOpenChange={onOpenChange}>
      <DialogContent className="flex max-h-[min(720px,calc(100svh-2rem))] w-[calc(100%-2rem)] max-w-3xl flex-col gap-0 overflow-hidden p-0">
        <DialogHeader className="shrink-0 border-b px-5 py-4 pr-12">
          <DialogTitle>{t("media.videos.previewTitle")}</DialogTitle>
          <DialogDescription className="truncate font-mono text-xs" title={job?.id}>
            {job?.id}
          </DialogDescription>
        </DialogHeader>
        <div className="flex min-h-0 flex-1 flex-col bg-black/95 p-4">
          {loading ? (
            <div className="flex min-h-48 flex-1 flex-col items-center justify-center gap-2 text-muted-foreground">
              <LoadingState className="min-h-0" />
              <p className="text-sm">{t("media.videos.previewLoading")}</p>
            </div>
          ) : null}
          {!loading && error ? (
            <div className="flex min-h-48 flex-1 flex-col items-center justify-center gap-2 px-4 text-center">
              <AlertCircle className="size-7 text-destructive" />
              <p className="text-sm text-destructive">{error}</p>
            </div>
          ) : null}
          {!loading && !error && objectUrl ? (
            <video
              key={objectUrl}
              className="max-h-[min(560px,calc(100svh-12rem))] w-full rounded-md bg-black"
              controls
              preload="metadata"
              src={objectUrl}
            />
          ) : null}
        </div>
      </DialogContent>
    </Dialog>
  );
}

function VideoStatusBadge({ status }: { status: MediaJobDTO["status"] }) {
  const { t } = useTranslation();
  return (
    <Badge variant="secondary" className={cn("whitespace-nowrap", statusClassName(status))}>
      {t(`media.videoStatus.${status}`)}
    </Badge>
  );
}

function ProgressValue({ value, locale }: { value: number; locale: string }) {
  const normalized = Math.max(0, Math.min(100, value));
  return (
    <div className="mx-auto flex w-20 flex-col items-center gap-1">
      <span className="text-xs tabular-nums">{formatNumber(normalized, locale, 0)}%</span>
      <span className="h-1.5 w-full overflow-hidden rounded-full bg-muted">
        <span className="block h-full rounded-full bg-primary" style={{ width: `${normalized}%` }} />
      </span>
    </div>
  );
}

function statusClassName(status: MediaJobDTO["status"]): string {
  switch (status) {
    case "completed":
      return "bg-emerald-500/10 text-emerald-700 dark:text-emerald-300";
    case "failed":
      return "bg-red-500/10 text-red-700 dark:text-red-300";
    case "in_progress":
      return "bg-sky-500/10 text-sky-700 dark:text-sky-300";
    case "queued":
      return "bg-amber-500/10 text-amber-700 dark:text-amber-300";
  }
}

function formatSpec(job: MediaJobDTO): string {
  return [job.size, job.quality].filter(Boolean).join(" · ") || "-";
}
