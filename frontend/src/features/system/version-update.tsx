import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowUpRight, RefreshCw } from "lucide-react";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { checkForUpdates, getVersionInfo, type UpdateStatus } from "@/entities/system/system-api";
import { cn } from "@/shared/lib/cn";
import { formatDateTime } from "@/shared/lib/format";

const versionQueryKey = ["system-version"] as const;

function useVersionInfo() {
  return useQuery({
    queryKey: versionQueryKey,
    queryFn: getVersionInfo,
    staleTime: 60_000,
    retry: 1,
  });
}

function useCheckForUpdates() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: checkForUpdates,
    onSuccess: (value) => queryClient.setQueryData(versionQueryKey, value),
  });
}

export function CurrentVersionLabel() {
  const versionQuery = useVersionInfo();
  const version = versionQuery.data?.currentVersion;
  if (!version) return null;
  return <span className="font-mono text-[10px] font-normal text-muted-foreground">{version}</span>;
}

export function VersionUpdateBanner() {
  const { t } = useTranslation();
  const versionQuery = useVersionInfo();
  const checkMutation = useCheckForUpdates();
  const version = versionQuery.data;
  if (!version?.updateAvailable) return null;

  return (
    <section className="flex flex-col gap-3 rounded-lg bg-amber-500/10 px-4 py-3 sm:flex-row sm:items-center sm:justify-between">
      <div className="min-w-0">
        <p className="text-sm font-medium">{t("updates.available", { version: version.latestVersion })}</p>
        <p className="mt-0.5 text-xs text-muted-foreground">{t("updates.currentSummary", { version: version.currentVersion })}</p>
      </div>
      <div className="flex shrink-0 items-center gap-0.5">
        {version.releaseUrl ? (
          <Button variant="ghost" size="sm" className="h-7 px-2.5 text-xs font-normal text-muted-foreground hover:text-foreground" asChild>
            <a href={version.releaseUrl} target="_blank" rel="noreferrer">{t("updates.viewRelease")}<ArrowUpRight className="size-3.5" /></a>
          </Button>
        ) : null}
        {version.releaseUrl ? <span className="mx-1 h-3 w-px bg-border/70" /> : null}
        <Button variant="ghost" size="sm" className="h-7 px-2.5 text-xs font-normal text-muted-foreground hover:text-foreground" disabled={checkMutation.isPending} onClick={() => checkMutation.mutate()}>
          {checkMutation.isPending ? <Spinner /> : <RefreshCw className="size-3.5" />}{t("updates.checkNow")}
        </Button>
      </div>
    </section>
  );
}

export function VersionUpdateSection() {
  const { t, i18n } = useTranslation();
  const versionQuery = useVersionInfo();
  const checkMutation = useCheckForUpdates();
  const version = versionQuery.data;
  const requestError = versionQuery.error instanceof Error ? versionQuery.error.message : "";
  const checkError = checkMutation.error instanceof Error ? checkMutation.error.message : "";
  const error = version?.error || checkError || requestError;

  return (
    <section className="space-y-4">
      <div className="flex min-h-8 items-center justify-between gap-3">
        <h2 className="text-sm font-medium">{t("updates.title")}</h2>
        <Button type="button" variant="secondary" size="sm" disabled={versionQuery.isPending || checkMutation.isPending} onClick={() => checkMutation.mutate()}>
          {versionQuery.isPending || checkMutation.isPending ? <Spinner /> : <RefreshCw />}{t("updates.checkNow")}
        </Button>
      </div>

      <div className="grid max-w-[860px] gap-x-8 gap-y-5 sm:grid-cols-2">
        <VersionField label={t("updates.currentVersion")} value={version?.currentVersion || "-"} />
        <VersionField label={t("updates.latestVersion")} value={version?.latestVersion || t("updates.notChecked")} />
        <VersionField label={t("updates.statusLabel")} value={version ? t(`updates.status.${version.status}`) : t("common.loading")} status={version?.status} />
        <VersionField label={t("updates.checkedAt")} value={version?.checkedAt ? formatDateTime(version.checkedAt, i18n.language) : t("updates.neverChecked")} />
      </div>

      {error ? <p className="max-w-[860px] text-xs leading-5 text-destructive">{error}</p> : null}

      {version?.releaseNotes || version?.releaseUrl ? (
        <div className="max-w-[860px] rounded-lg bg-muted/40 p-4">
          <div className="flex items-center justify-between gap-3">
            <h3 className="text-xs font-medium">{t("updates.releaseNotes")}</h3>
            {version.releaseUrl ? (
              <Button type="button" variant="ghost" size="sm" className="h-7 px-2 text-xs" asChild>
                <a href={version.releaseUrl} target="_blank" rel="noreferrer">{t("updates.openRelease")}<ArrowUpRight /></a>
              </Button>
            ) : null}
          </div>
          <p className="mt-2 max-h-40 overflow-y-auto whitespace-pre-wrap break-words text-xs leading-5 text-muted-foreground">
            {version.releaseNotes || t("updates.noReleaseNotes")}
          </p>
        </div>
      ) : null}

      <p className="max-w-[860px] text-xs leading-5 text-muted-foreground">{t("updates.manualOnly")}</p>
    </section>
  );
}

function VersionField({ label, value, status }: { label: string; value: string; status?: UpdateStatus }) {
  return (
    <div>
      <p className="text-xs text-muted-foreground">{label}</p>
      <div className="mt-1.5 flex items-center gap-2 text-sm font-medium">
        {status ? <span className={cn("size-1.5 rounded-full bg-muted-foreground", status === "up_to_date" && "bg-emerald-500", status === "update_available" && "bg-amber-500", status === "check_failed" && "bg-destructive")} /> : null}
        <span className="break-all">{value}</span>
      </div>
    </div>
  );
}
