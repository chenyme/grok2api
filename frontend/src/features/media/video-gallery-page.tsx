import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { AlertCircle, CheckCircle2, Clock, Eye, ListVideo, Loader2, RefreshCw, Search, Trash2 } from "lucide-react";
import type { LucideIcon } from "lucide-react";
import { useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { Table, TableActionCell, TableActionHead, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { deleteVideos, getVideoStats, listVideos } from "@/features/media/media-api";
import type { MediaJobDTO, VideoStatsDTO } from "@/features/media/types";
import { EmptyState, ErrorState, TableLoadingRow } from "@/shared/components/data-state";
import { DataTableFilters } from "@/shared/components/data-table-filters";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { PageHeader } from "@/shared/components/page-header";
import { Pagination } from "@/shared/components/pagination";
import { SortableTableHead } from "@/shared/components/sortable-table-head";
import { VirtualTableBody } from "@/shared/components/virtual-table-body";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { cn } from "@/shared/lib/cn";
import { formatDateTime, formatNumber } from "@/shared/lib/format";
import { nextTableSort, type SortOrder, type TableSort } from "@/shared/lib/table-sort";

type VideoStatusFilter = MediaJobDTO["status"] | "";

const statusOptions: MediaJobDTO["status"][] = ["queued", "in_progress", "completed", "failed"];

export function VideoGalleryPage() {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [search, setSearch] = useState("");
  const [statusFilter, setStatusFilter] = useState<VideoStatusFilter>("");
  const [selected, setSelected] = useState<Set<string>>(() => new Set());
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [previewing, setPreviewing] = useState<MediaJobDTO | null>(null);
  const [sort, setSort] = useState<TableSort>({ field: "createdAt", order: "desc" });
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
  const pageIDs = result?.items.filter(isTerminalVideoJob).map((job) => job.id) ?? [];
  const selectedOnPage = pageIDs.filter((id) => selected.has(id));
  const allPageSelected = pageIDs.length > 0 && selectedOnPage.length === pageIDs.length;

  const deleteMutation = useMutation({
    mutationFn: () => deleteVideos([...selected]),
    onSuccess: (deleteResult) => {
      if (result && selectedOnPage.length === result.items.length && page > 1) setPage(page - 1);
      if (previewing && selected.has(previewing.id)) setPreviewing(null);
      setSelected(new Set());
      setDeleteOpen(false);
      void queryClient.invalidateQueries({ queryKey: ["media", "videos"] });
      toast.success(t("media.videos.deleted", { count: deleteResult.deleted }));
    },
    onError: (error) => {
      void queryClient.invalidateQueries({ queryKey: ["media", "videos"] });
      toast.error(error instanceof Error ? error.message : t("errors.generic"));
    },
  });

  function refreshAll(): void {
    void videosQuery.refetch();
    void statsQuery.refetch();
  }

  function changeSort(field: string, initialOrder: SortOrder): void {
    setSort((current) => nextTableSort(current, field, initialOrder));
    setPage(1);
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

  function toggleVideo(id: string, checked: boolean): void {
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
        title={t("media.videos.title")}
        description={t("media.videos.description")}
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
            <div className="flex w-full items-center gap-2 sm:w-auto">
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
              <DataTableFilters filters={[{
                id: "status",
                label: t("media.videos.status"),
                value: statusFilter,
                onChange: (value) => { setStatusFilter(value as VideoStatusFilter); setPage(1); },
                options: statusOptions.map((status) => ({ value: status, label: t(`media.videoStatus.${status}`) })),
              }]} />
              {(normalizedSearch || statusFilter) && result ? <span className="hidden whitespace-nowrap text-xs tabular-nums text-muted-foreground md:inline">{t("media.videos.pageSummary", { count: result.items.length, total: result.total })}</span> : null}
            </div>
            {selected.size > 0 ? (
              <div className="flex h-8 items-center gap-2">
                <span className="text-xs text-muted-foreground">{t("common.selectedCount", { count: selected.size })}</span>
                <Button variant="secondary" size="sm" className="text-destructive hover:text-destructive" onClick={() => setDeleteOpen(true)}><Trash2 />{t("common.delete")}</Button>
              </div>
            ) : <VideoSummary stats={statsQuery.data} loading={statsQuery.isPending} unavailable={statsQuery.isError} locale={i18n.language} />}
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
          <Table viewportRows={20} rowHeight={72} className="min-w-[1096px] table-fixed text-xs">
            <colgroup>
              <col className="w-10" />
              <col className="w-64" />
              <col className="w-40" />
              <col className="w-40" />
              <col className="w-28" />
              <col className="w-40" />
              <col className="w-44" />
              <col className="w-10" />
            </colgroup>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead><Checkbox checked={allPageSelected ? true : selectedOnPage.length > 0 ? "indeterminate" : false} disabled={pageIDs.length === 0} onCheckedChange={(checked) => togglePage(checked === true)} aria-label={t("common.selectPage")} /></TableHead>
                <SortableTableHead field="prompt" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("media.videos.prompt")}</SortableTableHead>
                <SortableTableHead field="model" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("media.videos.model")}</SortableTableHead>
                <SortableTableHead field="status" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("media.videos.statusProgress")}</SortableTableHead>
                <SortableTableHead field="spec" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("media.videos.spec")}</SortableTableHead>
                <SortableTableHead field="account" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("media.videos.owner")}</SortableTableHead>
                <SortableTableHead field="createdAt" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("media.videos.time")}</SortableTableHead>
                <TableActionHead />
              </TableRow>
            </TableHeader>
            {videosQuery.isPending ? (
              <TableBody><TableLoadingRow colSpan={8} /></TableBody>
            ) : (
              <VirtualTableBody items={result?.items ?? []} colSpan={8} rowHeight={72} renderRow={(job) => (
                <TableRow className="group h-[72px]" data-state={selected.has(job.id) ? "selected" : undefined} key={job.id}>
                  <TableCell>
                    <Checkbox checked={selected.has(job.id)} disabled={!isTerminalVideoJob(job)} onCheckedChange={(checked) => toggleVideo(job.id, checked === true)} aria-label={t("common.selectItem", { name: job.id })} />
                  </TableCell>
                  <TableCell className="min-w-0">
                    <div className="min-w-0">
                      <span className="block truncate text-xs font-medium" title={job.prompt}>{job.prompt || "-"}</span>
                      <span className="mt-0.5 block truncate font-mono text-[10px] text-muted-foreground" title={job.id}>{job.id}</span>
                    </div>
                  </TableCell>
                  <TableCell className="min-w-0"><span className="block truncate" title={job.model}>{job.model || "-"}</span></TableCell>
                  <TableCell><VideoProgress status={job.status} value={job.progress} errorMessage={job.errorMessage} locale={i18n.language} /></TableCell>
                  <TableCell>
                    <div className="space-y-0.5 text-xs">
                      <span className="block truncate" title={formatSpec(job)}>{formatSpec(job)}</span>
                      <span className="block text-[11px] text-muted-foreground">{t("media.videos.seconds", { count: job.seconds })}</span>
                    </div>
                  </TableCell>
                  <TableCell className="min-w-0">
                    <div className="min-w-0 space-y-0.5">
                      <span className="block truncate" title={job.accountName}>{job.accountName || "-"}</span>
                      <span className="block truncate text-[11px] text-muted-foreground" title={job.clientKeyName}>{job.clientKeyName || "-"}</span>
                    </div>
                  </TableCell>
                  <TableCell><VideoTimes job={job} locale={i18n.language} /></TableCell>
                  <TableActionCell>
                    {job.status === "completed" ? (
                      <Button type="button" variant="ghost" size="icon" className="size-8" disabled={!job.assetId} title={job.assetId ? t("media.videos.preview") : t("media.videos.previewUnavailable")} onClick={() => setPreviewing(job)} aria-label={job.assetId ? t("media.videos.preview") : t("media.videos.previewUnavailable")}><Eye /></Button>
                    ) : null}
                  </TableActionCell>
                </TableRow>
              )} />
            )}
          </Table>
        ) : null}
      </DataTableShell>

      <AlertDialog open={deleteOpen} onOpenChange={setDeleteOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("media.videos.deleteTitle", { count: selected.size })}</AlertDialogTitle>
            <AlertDialogDescription>{t("media.videos.deleteDescription")}</AlertDialogDescription>
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

      <Dialog open={Boolean(previewing)} onOpenChange={(open) => !open && setPreviewing(null)}>
        <DialogContent className="max-h-[calc(100vh-2rem)] max-w-4xl overflow-hidden">
          <DialogHeader className="min-w-0 pr-8">
            <DialogTitle className="line-clamp-2 min-w-0 break-words leading-6" title={previewing?.prompt || undefined}>{previewing?.prompt || t("media.videos.previewTitle")}</DialogTitle>
            <DialogDescription className="min-w-0 truncate font-mono" title={previewing?.id}>{previewing?.id}</DialogDescription>
          </DialogHeader>
          {previewing?.assetId ? (
            <VideoPreview key={previewing.assetId} assetId={previewing.assetId} />
          ) : null}
        </DialogContent>
      </Dialog>
    </div>
  );
}

function VideoPreview({ assetId }: { assetId: string }) {
  const { t } = useTranslation();
  const videoRef = useRef<HTMLVideoElement>(null);
  const [state, setState] = useState<"loading" | "ready" | "error">("loading");

  function retry(): void {
    setState("loading");
    videoRef.current?.load();
  }

  return (
    <div className="relative flex min-h-56 w-full items-center justify-center overflow-hidden rounded-lg bg-black sm:min-h-80">
      <video
        ref={videoRef}
        className={cn("h-auto max-h-[70vh] w-auto max-w-full object-contain", state === "error" && "invisible")}
        src={videoAssetURL(assetId)}
        controls
        playsInline
        preload="auto"
        onLoadStart={() => setState("loading")}
        onLoadedMetadata={(event) => showFirstVideoFrame(event.currentTarget)}
        onLoadedData={() => setState("ready")}
        onCanPlay={() => setState("ready")}
        onEnded={(event) => showFirstVideoFrame(event.currentTarget)}
        onError={() => setState("error")}
      />
      {state === "loading" ? (
        <div className="pointer-events-none absolute inset-0 flex items-center justify-center bg-black">
          <Spinner className="size-5 text-white" />
          <span className="sr-only">{t("common.loading")}</span>
        </div>
      ) : null}
      {state === "error" ? (
        <div className="absolute inset-0 flex flex-col items-center justify-center gap-3 bg-black px-6 text-center text-white">
          <AlertCircle className="size-6 text-red-400" />
          <p className="text-sm">{t("media.videos.previewUnavailable")}</p>
          <Button type="button" variant="secondary" size="sm" onClick={retry}>
            <RefreshCw />
            {t("common.retry")}
          </Button>
        </div>
      ) : null}
    </div>
  );
}

function showFirstVideoFrame(video: HTMLVideoElement): void {
  if (!Number.isFinite(video.duration) || video.duration <= 0) return;
  video.currentTime = Math.min(0.01, video.duration / 2);
}

function isTerminalVideoJob(job: MediaJobDTO): boolean {
  return job.status === "completed" || job.status === "failed";
}

function VideoSummary({ stats, loading, unavailable, locale }: { stats?: VideoStatsDTO; loading: boolean; unavailable: boolean; locale: string }) {
  const { t } = useTranslation();
  if (loading) return <div className="flex h-8 items-center"><Spinner className="size-3.5" /></div>;
  const value = (count: number | undefined) => unavailable ? "-" : formatNumber(count ?? 0, locale, 0);
  return (
    <div className="flex h-8 w-full items-center gap-4 overflow-x-auto whitespace-nowrap text-xs sm:w-auto">
      <VideoSummaryItem icon={ListVideo} label={t("media.videos.totalJobs")} value={value(stats?.totalJobs)} tone="text-muted-foreground" />
      <span className="h-3 w-px shrink-0 bg-border" aria-hidden="true" />
      <VideoSummaryItem icon={Clock} label={t("media.videos.queued")} value={value(stats?.queued)} tone="text-amber-600 dark:text-amber-400" />
      <VideoSummaryItem icon={Loader2} label={t("media.videos.inProgress")} value={value(stats?.inProgress)} tone="text-sky-600 dark:text-sky-400" />
      <VideoSummaryItem icon={CheckCircle2} label={t("media.videos.completed")} value={value(stats?.completed)} tone="text-emerald-600 dark:text-emerald-400" />
      <VideoSummaryItem icon={AlertCircle} label={t("media.videos.failed")} value={value(stats?.failed)} tone="text-red-600 dark:text-red-400" />
    </div>
  );
}

function VideoSummaryItem({ icon: Icon, label, value, tone }: { icon: LucideIcon; label: string; value: string; tone: string }) {
  return (
    <span className="inline-flex shrink-0 items-center gap-1.5">
      <Icon className={cn("size-3.5", tone)} />
      <span className="text-muted-foreground">{label}</span>
      <strong className="font-medium tabular-nums">{value}</strong>
    </span>
  );
}

function VideoStatus({ status, errorMessage }: { status: MediaJobDTO["status"]; errorMessage?: string }) {
  const { t } = useTranslation();
  const tone = statusTone(status);
  const statusLabel = (
    <span className={cn("inline-flex items-center gap-1.5 whitespace-nowrap text-xs", tone.text)}>
      <span className={cn("size-1.5 rounded-full", tone.dot)} />
      {t(`media.videoStatus.${status}`)}
    </span>
  );
  if (!errorMessage) return statusLabel;
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span className="inline-flex cursor-help" tabIndex={0}>{statusLabel}</span>
      </TooltipTrigger>
      <TooltipContent side="top" className="max-w-80 whitespace-normal break-words text-left leading-relaxed">
        {errorMessage}
      </TooltipContent>
    </Tooltip>
  );
}

function VideoProgress({ status, value, errorMessage, locale }: { status: MediaJobDTO["status"]; value: number; errorMessage?: string; locale: string }) {
  const normalized = Math.max(0, Math.min(100, value));
  return (
    <div className="w-28 space-y-2">
      <div className="flex items-center justify-between gap-2">
        <VideoStatus status={status} errorMessage={errorMessage} />
        <span className="shrink-0 text-[11px] tabular-nums text-muted-foreground">{formatNumber(normalized, locale, 0)}%</span>
      </div>
      <span className="h-1 w-full overflow-hidden rounded-full bg-muted">
        <span className={cn("block h-full rounded-full", progressTone(status))} style={{ width: `${normalized}%` }} />
      </span>
    </div>
  );
}

function VideoTimes({ job, locale }: { job: MediaJobDTO; locale: string }) {
  const { t } = useTranslation();
  return (
    <div className="space-y-1 whitespace-nowrap text-[11px]">
      <div className="flex items-center gap-1.5"><span className="w-7 text-muted-foreground">{t("media.videos.createdShort")}</span><span>{formatDateTime(job.createdAt, locale)}</span></div>
      <div className="flex items-center gap-1.5"><span className="w-7 text-muted-foreground">{t("media.videos.completedShort")}</span><span className={job.completedAt ? undefined : "text-muted-foreground"}>{formatDateTime(job.completedAt, locale)}</span></div>
    </div>
  );
}

function progressTone(status: MediaJobDTO["status"]): string {
  switch (status) {
    case "completed":
      return "bg-emerald-500";
    case "failed":
      return "bg-red-500";
    case "in_progress":
      return "bg-sky-500";
    case "queued":
      return "bg-amber-500";
  }
}

function statusTone(status: MediaJobDTO["status"]): { dot: string; text: string } {
  switch (status) {
    case "completed":
      return { dot: "bg-emerald-500", text: "text-emerald-700 dark:text-emerald-300" };
    case "failed":
      return { dot: "bg-red-500", text: "text-red-700 dark:text-red-300" };
    case "in_progress":
      return { dot: "bg-sky-500", text: "text-sky-700 dark:text-sky-300" };
    case "queued":
      return { dot: "bg-amber-500", text: "text-amber-700 dark:text-amber-300" };
  }
}

function formatSpec(job: MediaJobDTO): string {
  return [job.size, job.quality].filter(Boolean).join(" · ") || "-";
}

function videoAssetURL(assetID: string): string {
  return `/v1/media/videos/${encodeURIComponent(assetID)}`;
}
