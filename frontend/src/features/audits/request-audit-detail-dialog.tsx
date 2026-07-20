import { useQuery } from "@tanstack/react-query";
import { Braces, FileText, KeyRound, Network, Server, TriangleAlert } from "lucide-react";
import { useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";

import { Badge } from "@/components/ui/badge";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { getRequestAudit, type AuditAttemptDTO, type AuditDTO } from "@/features/audits/request-audits-api";
import { CopyButton } from "@/shared/components/copy-button";
import { ErrorState, LoadingState } from "@/shared/components/data-state";
import { cn } from "@/shared/lib/cn";
import { formatDateTime, formatNumber } from "@/shared/lib/format";

export function RequestAuditDetailDialog({ audit, open, onOpenChange }: { audit: AuditDTO | null; open: boolean; onOpenChange: (open: boolean) => void }) {
  const { t, i18n } = useTranslation();
  const [selectedNumber, setSelectedNumber] = useState<number | null>(null);
  const detailQuery = useQuery({
    queryKey: ["request-audits", "detail", audit?.id],
    queryFn: () => getRequestAudit(audit?.id ?? ""),
    enabled: open && audit !== null,
  });

  const attempts = detailQuery.data?.attempts ?? [];
  const selectedAttempt = attempts.find((attempt) => attempt.number === selectedNumber) ?? attempts[0];

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="flex h-[min(620px,calc(100svh-2rem))] max-h-[calc(100svh-2rem)] min-h-0 flex-col gap-0 overflow-hidden p-0 text-xs sm:max-w-4xl">
        <DialogHeader className="shrink-0 px-5 py-4 pr-12">
          <DialogTitle>{t("audits.detailTitle")}</DialogTitle>
          <DialogDescription className="flex min-w-0 flex-wrap gap-x-4 gap-y-0.5">
            <span className="truncate" title={audit?.requestId}>{audit?.requestId}</span>
            {audit ? <span>{formatDateTime(audit.createdAt, i18n.language)}</span> : null}
          </DialogDescription>
        </DialogHeader>

        {detailQuery.isPending ? <LoadingState className="min-h-0 flex-1" /> : null}
        {detailQuery.isError ? <ErrorState message={detailQuery.error.message} onRetry={() => void detailQuery.refetch()} /> : null}
        {detailQuery.data ? (
          attempts.length > 0 && selectedAttempt ? (
            <div className="grid min-h-0 flex-1 grid-rows-[auto_minmax(0,1fr)] lg:grid-cols-[190px_minmax(0,1fr)] lg:grid-rows-1">
              <aside className="flex min-h-0 min-w-0 flex-col overflow-hidden bg-muted/25 p-2.5">
                <p className="mb-1 shrink-0 px-2 text-muted-foreground">{t("audits.attemptTimeline")}</p>
                <div className="flex max-h-28 gap-1 overflow-auto lg:min-h-0 lg:max-h-none lg:flex-1 lg:flex-col">
                  {attempts.map((attempt) => (
                    <AttemptButton
                      key={attempt.id}
                      attempt={attempt}
                      selected={attempt.number === selectedAttempt.number}
                      onClick={() => setSelectedNumber(attempt.number)}
                    />
                  ))}
                </div>
              </aside>
              <AttemptDetail key={selectedAttempt.id} attempt={selectedAttempt} />
            </div>
          ) : (
            <div className="flex min-h-0 flex-1 flex-col items-center justify-center gap-2 px-6 text-center text-muted-foreground">
              <TriangleAlert className="size-7 stroke-1" />
              <p>{t("audits.noFailureAttempts")}</p>
              {detailQuery.data.audit.errorCode ? <span className="max-w-full break-words">{detailQuery.data.audit.errorCode}</span> : null}
            </div>
          )
        ) : null}
      </DialogContent>
    </Dialog>
  );
}

function AttemptButton({ attempt, selected, onClick }: { attempt: AuditAttemptDTO; selected: boolean; onClick: () => void }) {
  const { t } = useTranslation();
  const Icon = attempt.source === "upstream_http" ? Server : attempt.source === "gateway_transport" ? Network : KeyRound;
  return (
    <button
      type="button"
      className={cn("w-48 shrink-0 rounded-md px-2.5 py-2 text-left outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring/50 lg:w-full", selected ? "bg-accent text-accent-foreground" : "hover:bg-accent/60")}
      aria-pressed={selected}
      onClick={onClick}
    >
      <span className="flex items-center justify-between gap-2">
        <span className="flex min-w-0 items-center gap-2"><Icon className="size-3.5 shrink-0" />{t("audits.attemptNumber", { number: attempt.number })}</span>
        {attempt.upstreamStatusCode ? <StatusBadge statusCode={attempt.upstreamStatusCode} failed={attempt.stage === "response_stream"} /> : null}
      </span>
    </button>
  );
}

function AttemptDetail({ attempt }: { attempt: AuditAttemptDTO }) {
  const { t } = useTranslation();
  const headersText = JSON.stringify(attempt.responseHeaders, null, 2);
  const errorChainText = JSON.stringify(attempt.errorChain, null, 2);
  return (
    <main className="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden">
      <Tabs defaultValue="overview" className="min-h-0 flex-1 overflow-hidden px-4 pb-4 sm:px-5">
        <div className="flex shrink-0 flex-wrap items-center gap-x-4 gap-y-2 py-2">
          <AttemptSummary attempt={attempt} />
          <div className="ml-auto max-w-full overflow-x-auto">
            <TabsList>
              <TabsTrigger value="overview">{t("audits.overview")}</TabsTrigger>
              <TabsTrigger value="body">{t("audits.responseBody")}</TabsTrigger>
              <TabsTrigger value="headers">{t("audits.responseHeaders")}</TabsTrigger>
              <TabsTrigger value="errors">{t("audits.errorChain")}</TabsTrigger>
            </TabsList>
          </div>
        </div>
        <TabsContent value="overview" className="min-h-0 flex-1 overflow-y-auto">
          <AttemptOverview attempt={attempt} />
        </TabsContent>
        <TabsContent value="body" className="min-h-0 flex-1 overflow-hidden pt-3">
          <CodePanel value={attempt.responseBody} displayValue={formattedResponseBody(attempt)} emptyMessage={t("audits.emptyResponseBody")} encoding={attempt.responseBodyEncoding} truncated={attempt.responseBodyTruncated} />
        </TabsContent>
        <TabsContent value="headers" className="min-h-0 flex-1 overflow-hidden pt-3">
          <HeadersPanel headers={attempt.responseHeaders} copyValue={headersText} />
        </TabsContent>
        <TabsContent value="errors" className="min-h-0 flex-1 overflow-hidden pt-3">
          <ErrorChainPanel attempt={attempt} copyValue={errorChainText} />
        </TabsContent>
      </Tabs>
    </main>
  );
}

function AttemptSummary({ attempt }: { attempt: AuditAttemptDTO }) {
  const { t } = useTranslation();
  const isHTTP = attempt.source === "upstream_http";
  const isStreamFailure = isHTTP && attempt.stage === "response_stream";
  const Icon = isHTTP ? Server : attempt.source === "gateway_transport" ? Network : KeyRound;
  const title = isStreamFailure
    ? t("audits.upstreamStreamFailure", { status: attempt.upstreamStatusCode ?? "-" })
    : isHTTP
    ? t("audits.upstreamHttpFailure", { status: attempt.upstreamStatusCode ?? "-" })
    : attempt.source === "gateway_transport" ? t("audits.gatewayTransportFailure") : t("audits.credentialFailure");
  return (
    <div className="flex min-w-0 items-center gap-2">
      <Icon className="size-4 shrink-0 text-destructive" />
      <p className="min-w-0 truncate">{title}</p>
    </div>
  );
}

function AttemptOverview({ attempt }: { attempt: AuditAttemptDTO }) {
  const { t, i18n } = useTranslation();
  return (
    <div className="grid gap-x-10 gap-y-6 px-1 py-4 sm:grid-cols-2">
      <OverviewField label={t("audits.attemptStartedAt")} value={formatDateTime(attempt.startedAt, i18n.language)} />
      <OverviewField label={t("audits.duration")} value={`${formatNumber(attempt.durationMs, i18n.language)} ms`} />
      <OverviewField label={t("audits.account")} value={attempt.accountName || (attempt.accountId ? `#${attempt.accountId}` : "-")} />
      <OverviewField label={t("audits.requestMethod")} value={attempt.method || "-"} />
      <OverviewField label={t("audits.requestPath")} value={attempt.requestPath || "-"} />
      <OverviewField label={t("audits.upstreamStatus")} value={attempt.upstreamStatus || (attempt.upstreamStatusCode ? String(attempt.upstreamStatusCode) : "-")} />
      <OverviewField className="sm:col-span-2" label={t("audits.upstreamUrl")} value={attempt.upstreamUrl || t("audits.upstreamUrlUnavailable")} copy={Boolean(attempt.upstreamUrl)} />
      {attempt.transportError ? <OverviewField className="sm:col-span-2" label={attempt.source === "gateway_transport" ? t("audits.transportError") : t("audits.attemptError")} value={attempt.transportError} copy /> : null}
    </div>
  );
}

function OverviewField({ className, label, value, copy }: { className?: string; label: string; value: string; copy?: boolean }) {
  return (
    <div className={cn("flex min-w-0 items-start gap-4", className)}>
      <div className="min-w-0 flex-1">
        <p className="text-muted-foreground">{label}</p>
        <p className="mt-1 break-all" title={value}>{value}</p>
      </div>
      {copy ? (
        <div className="shrink-0 pt-4">
          <CopyButton value={value} />
        </div>
      ) : null}
    </div>
  );
}

function CodePanel({ value, displayValue, emptyMessage, encoding, truncated }: { value: string; displayValue: string; emptyMessage: string; encoding: string; truncated: boolean }) {
  const { t } = useTranslation();
  if (!value) return <EmptyPanel icon={<FileText />} message={emptyMessage} />;
  return (
    <div className="flex h-full min-h-0 flex-col overflow-hidden rounded-md bg-muted/25">
      <div className="flex h-10 shrink-0 items-center justify-between px-3">
        <span className="flex min-w-0 items-center gap-2 text-muted-foreground">
          <span>{t("audits.bodyEncoding", { encoding })}</span>
          {truncated ? <Badge variant="outline">{t("audits.bodyTruncated")}</Badge> : null}
        </span>
        <CopyButton value={value} />
      </div>
      <div className="min-h-0 flex-1 overflow-auto whitespace-pre-wrap break-words p-3">{displayValue}</div>
    </div>
  );
}

function HeadersPanel({ headers, copyValue }: { headers: Record<string, string[]>; copyValue: string }) {
  const { t } = useTranslation();
  const entries = Object.entries(headers);
  if (entries.length === 0) return <EmptyPanel icon={<Braces />} message={t("audits.emptyResponseHeaders")} />;
  return (
    <div className="flex h-full min-h-0 flex-col overflow-hidden rounded-md bg-muted/15">
      <div className="flex h-10 shrink-0 items-center justify-between px-3">
        <span className="text-muted-foreground">{t("audits.headerCount", { count: entries.length })}</span>
        <CopyButton value={copyValue} />
      </div>
      <div className="min-h-0 flex-1 space-y-1 overflow-auto px-2 pb-2">
        {entries.map(([name, values]) => (
          <div key={name} className="grid gap-1 rounded-md px-2 py-2 hover:bg-background/60 sm:grid-cols-[180px_minmax(0,1fr)] sm:gap-4">
            <span className="break-all text-muted-foreground">{name}</span>
            <div className="min-w-0 space-y-1">{values.map((value, index) => <span key={`${name}-${index}`} className="block break-all">{value}</span>)}</div>
          </div>
        ))}
      </div>
    </div>
  );
}

function ErrorChainPanel({ attempt, copyValue }: { attempt: AuditAttemptDTO; copyValue: string }) {
  const { t } = useTranslation();
  if (attempt.errorChain.length === 0) return <EmptyPanel icon={<Network />} message={t("audits.emptyErrorChain")} />;
  return (
    <div className="flex h-full min-h-0 flex-col overflow-hidden rounded-md bg-muted/15">
      <div className="flex h-10 shrink-0 items-center justify-between px-3">
        <span className="text-muted-foreground">{t("audits.errorFrameCount", { count: attempt.errorChain.length })}</span>
        <CopyButton value={copyValue} />
      </div>
      <ol className="min-h-0 flex-1 space-y-4 overflow-auto px-3 pb-3">
        {attempt.errorChain.map((frame, index) => (
          <li key={`${frame.type}-${index}`}>
            <div className="flex items-center gap-2 text-muted-foreground"><span>#{index + 1}</span><span className="break-all">{frame.type}</span></div>
            <p className="mt-2 whitespace-pre-wrap break-words">{frame.message}</p>
          </li>
        ))}
      </ol>
    </div>
  );
}

function EmptyPanel({ icon, message }: { icon: ReactNode; message: string }) {
  return <div className="flex h-full min-h-40 flex-col items-center justify-center gap-2 rounded-md bg-muted/15 text-muted-foreground [&_svg]:size-6 [&_svg]:stroke-1"><span>{icon}</span><p>{message}</p></div>;
}

function formattedResponseBody(attempt: AuditAttemptDTO): string {
  if (attempt.responseBodyEncoding !== "utf8") return attempt.responseBody;
  const contentType = Object.entries(attempt.responseHeaders).find(([name]) => name.toLowerCase() === "content-type")?.[1].join(";") ?? "";
  if (attempt.stage !== "response_stream" && !contentType.toLowerCase().includes("json")) return attempt.responseBody;
  try {
    return JSON.stringify(JSON.parse(attempt.responseBody), null, 2);
  } catch {
    return attempt.responseBody;
  }
}

function StatusBadge({ statusCode, failed = false }: { statusCode: number; failed?: boolean }) {
  const className = failed
    ? "bg-amber-500/10 text-amber-700 dark:text-amber-300"
    : statusCode >= 500
    ? "bg-red-500/10 text-red-700 dark:text-red-300"
    : statusCode >= 400 ? "bg-amber-500/10 text-amber-700 dark:text-amber-300"
      : statusCode >= 200 && statusCode < 300 ? "bg-emerald-500/10 text-emerald-700 dark:text-emerald-300" : "bg-muted text-muted-foreground";
  return <Badge variant="secondary" className={cn("min-w-9 justify-center px-1.5", className)}>{statusCode}</Badge>;
}
