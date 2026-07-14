import { useQuery } from "@tanstack/react-query";
import { Braces, Clock, FileText, KeyRound, Network, Server, TriangleAlert } from "lucide-react";
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
      <DialogContent className="flex h-[min(760px,calc(100svh-2rem))] max-h-[calc(100svh-2rem)] min-h-0 flex-col gap-0 overflow-hidden p-0 sm:max-w-6xl">
        <DialogHeader className="shrink-0 border-b px-5 py-4 pr-12">
          <div className="flex min-w-0 flex-wrap items-center gap-2">
            <DialogTitle>{t("audits.detailTitle")}</DialogTitle>
            {audit ? <StatusBadge statusCode={audit.statusCode} /> : null}
            {audit?.errorCode ? <code className="min-w-0 truncate text-xs text-destructive" title={audit.errorCode}>{audit.errorCode}</code> : null}
          </div>
          <DialogDescription className="flex min-w-0 flex-wrap gap-x-4 gap-y-0.5">
            <span className="truncate" title={audit?.requestId}>{audit?.requestId}</span>
            {audit ? <span>{formatDateTime(audit.createdAt, i18n.language)}</span> : null}
            {audit ? <span>{t("audits.failedAttemptCount", { count: audit.attemptCount })}</span> : null}
          </DialogDescription>
        </DialogHeader>

        {detailQuery.isPending ? <LoadingState className="min-h-0 flex-1" /> : null}
        {detailQuery.isError ? <ErrorState message={detailQuery.error.message} onRetry={() => void detailQuery.refetch()} /> : null}
        {detailQuery.data ? (
          attempts.length > 0 && selectedAttempt ? (
            <div className="grid min-h-0 flex-1 grid-rows-[auto_minmax(0,1fr)] lg:grid-cols-[250px_minmax(0,1fr)] lg:grid-rows-1">
              <aside className="flex min-h-0 min-w-0 flex-col overflow-hidden border-b bg-muted/20 p-3 lg:border-b-0 lg:border-r">
                <p className="mb-2 shrink-0 px-2 text-[11px] font-medium uppercase tracking-wide text-muted-foreground">{t("audits.attemptTimeline")}</p>
                <div className="flex max-h-36 gap-2 overflow-auto lg:min-h-0 lg:max-h-none lg:flex-1 lg:flex-col">
                  {attempts.map((attempt) => (
                    <AttemptButton
                      key={attempt.id}
                      attempt={attempt}
                      locale={i18n.language}
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
              <p className="text-sm">{t("audits.noFailureAttempts")}</p>
              {detailQuery.data.audit.errorCode ? <code className="max-w-full break-words text-xs">{detailQuery.data.audit.errorCode}</code> : null}
            </div>
          )
        ) : null}
      </DialogContent>
    </Dialog>
  );
}

function AttemptButton({ attempt, locale, selected, onClick }: { attempt: AuditAttemptDTO; locale: string; selected: boolean; onClick: () => void }) {
  const { t } = useTranslation();
  const Icon = attempt.source === "upstream_http" ? Server : attempt.source === "gateway_transport" ? Network : KeyRound;
  return (
    <button
      type="button"
      className={cn("w-56 shrink-0 rounded-md border px-3 py-2.5 text-left outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring/50 lg:w-full", selected ? "border-border bg-background shadow-sm" : "border-transparent hover:bg-muted/70")}
      aria-pressed={selected}
      onClick={onClick}
    >
      <span className="flex items-center justify-between gap-2">
        <span className="flex min-w-0 items-center gap-2 text-xs font-medium"><Icon className="size-3.5 shrink-0" />{t("audits.attemptNumber", { number: attempt.number })}</span>
        {attempt.upstreamStatusCode ? <StatusBadge statusCode={attempt.upstreamStatusCode} /> : null}
      </span>
      <span className="mt-1.5 block truncate text-[11px] text-muted-foreground" title={sourceLabel(attempt, t)}>{sourceLabel(attempt, t)}</span>
      <span className="mt-1 flex items-center gap-1 text-[10px] tabular-nums text-muted-foreground"><Clock className="size-3" />{formatNumber(attempt.durationMs, locale)} ms</span>
    </button>
  );
}

function AttemptDetail({ attempt }: { attempt: AuditAttemptDTO }) {
  const { t } = useTranslation();
  const headersText = JSON.stringify(attempt.responseHeaders, null, 2);
  const errorChainText = JSON.stringify(attempt.errorChain, null, 2);
  return (
    <main className="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden">
      <DiagnosticBanner attempt={attempt} />
      <Tabs defaultValue="overview" className="min-h-0 flex-1 overflow-hidden px-4 pb-4 sm:px-5">
        <div className="shrink-0 overflow-x-auto border-b py-3">
          <TabsList>
            <TabsTrigger value="overview">{t("audits.overview")}</TabsTrigger>
            <TabsTrigger value="body">{t("audits.responseBody")}</TabsTrigger>
            <TabsTrigger value="headers">{t("audits.responseHeaders")}</TabsTrigger>
            <TabsTrigger value="errors">{t("audits.errorChain")}</TabsTrigger>
          </TabsList>
        </div>
        <TabsContent value="overview" className="min-h-0 flex-1 overflow-y-auto py-4">
          <AttemptOverview attempt={attempt} />
        </TabsContent>
        <TabsContent value="body" className="min-h-0 flex-1 overflow-hidden pt-4">
          <CodePanel value={attempt.responseBody} displayValue={formattedResponseBody(attempt)} emptyMessage={t("audits.emptyResponseBody")} encoding={attempt.responseBodyEncoding} truncated={attempt.responseBodyTruncated} />
        </TabsContent>
        <TabsContent value="headers" className="min-h-0 flex-1 overflow-hidden pt-4">
          <HeadersPanel headers={attempt.responseHeaders} copyValue={headersText} />
        </TabsContent>
        <TabsContent value="errors" className="min-h-0 flex-1 overflow-hidden pt-4">
          <ErrorChainPanel attempt={attempt} copyValue={errorChainText} />
        </TabsContent>
      </Tabs>
    </main>
  );
}

function DiagnosticBanner({ attempt }: { attempt: AuditAttemptDTO }) {
  const { t } = useTranslation();
  const isHTTP = attempt.source === "upstream_http";
  const Icon = isHTTP ? Server : attempt.source === "gateway_transport" ? Network : KeyRound;
  const title = isHTTP
    ? t("audits.upstreamHttpFailure", { status: attempt.upstreamStatusCode ?? "-" })
    : attempt.source === "gateway_transport" ? t("audits.gatewayTransportFailure") : t("audits.credentialFailure");
  const description = isHTTP ? t("audits.upstreamResponseReceived") : t("audits.noUpstreamResponse");
  return (
    <div className="mx-4 mt-4 flex shrink-0 items-start gap-3 rounded-lg border border-destructive/20 bg-destructive/5 px-3.5 py-3 sm:mx-5">
      <Icon className="mt-0.5 size-4 shrink-0 text-destructive" />
      <div className="min-w-0">
        <p className="text-xs font-medium">{title}</p>
        <p className="mt-0.5 text-[11px] leading-5 text-muted-foreground">{description}</p>
      </div>
      <Badge variant="outline" className="ml-auto shrink-0 font-mono text-[10px]">{attempt.stage}</Badge>
    </div>
  );
}

function AttemptOverview({ attempt }: { attempt: AuditAttemptDTO }) {
  const { t, i18n } = useTranslation();
  return (
    <div className="space-y-4">
      <div className="grid gap-2 sm:grid-cols-2 xl:grid-cols-3">
        <InfoCard label={t("audits.attemptStartedAt")} value={formatDateTime(attempt.startedAt, i18n.language)} />
        <InfoCard label={t("audits.duration")} value={`${formatNumber(attempt.durationMs, i18n.language)} ms`} />
        <InfoCard label={t("audits.account")} value={attempt.accountName || (attempt.accountId ? `#${attempt.accountId}` : "-")} />
        <InfoCard label={t("audits.requestMethod")} value={attempt.method || "-"} mono />
        <InfoCard label={t("audits.requestPath")} value={attempt.requestPath || "-"} mono />
        <InfoCard label={t("audits.upstreamStatus")} value={attempt.upstreamStatus || (attempt.upstreamStatusCode ? String(attempt.upstreamStatusCode) : "-")} mono />
      </div>
      <DetailField label={t("audits.upstreamUrl")} value={attempt.upstreamUrl || t("audits.upstreamUrlUnavailable")} mono copy={Boolean(attempt.upstreamUrl)} />
      {attempt.transportError ? <DetailField label={attempt.source === "gateway_transport" ? t("audits.transportError") : t("audits.attemptError")} value={attempt.transportError} mono copy /> : null}
    </div>
  );
}

function InfoCard({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="min-w-0 rounded-lg bg-muted/50 px-3 py-2.5">
      <p className="text-[10px] uppercase tracking-wide text-muted-foreground">{label}</p>
      <p className={cn("mt-1 truncate text-xs", mono && "font-mono")} title={value}>{value}</p>
    </div>
  );
}

function DetailField({ label, value, mono, copy }: { label: string; value: string; mono?: boolean; copy?: boolean }) {
  return (
    <div className="overflow-hidden rounded-lg border">
      <div className="flex h-9 items-center justify-between border-b bg-muted/30 px-3">
        <span className="text-[11px] font-medium">{label}</span>
        {copy ? <CopyButton value={value} /> : null}
      </div>
      <p className={cn("break-all px-3 py-2.5 text-xs leading-5", mono && "font-mono")}>{value}</p>
    </div>
  );
}

function CodePanel({ value, displayValue, emptyMessage, encoding, truncated }: { value: string; displayValue: string; emptyMessage: string; encoding: string; truncated: boolean }) {
  const { t } = useTranslation();
  if (!value) return <EmptyPanel icon={<FileText />} message={emptyMessage} />;
  return (
    <div className="flex h-full min-h-0 flex-col overflow-hidden rounded-lg border bg-muted/20">
      <div className="flex h-10 shrink-0 items-center justify-between border-b bg-background px-3">
        <span className="flex min-w-0 items-center gap-2 text-[11px] text-muted-foreground">
          <span>{t("audits.bodyEncoding", { encoding })}</span>
          {truncated ? <Badge variant="outline" className="font-normal">{t("audits.bodyTruncated")}</Badge> : null}
        </span>
        <CopyButton value={value} />
      </div>
      <pre className="min-h-0 flex-1 overflow-auto whitespace-pre-wrap break-words p-3 font-mono text-xs leading-5">{displayValue}</pre>
    </div>
  );
}

function HeadersPanel({ headers, copyValue }: { headers: Record<string, string[]>; copyValue: string }) {
  const { t } = useTranslation();
  const entries = Object.entries(headers);
  if (entries.length === 0) return <EmptyPanel icon={<Braces />} message={t("audits.emptyResponseHeaders")} />;
  return (
    <div className="flex h-full min-h-0 flex-col overflow-hidden rounded-lg border">
      <div className="flex h-10 shrink-0 items-center justify-between border-b bg-muted/20 px-3">
        <span className="text-[11px] text-muted-foreground">{t("audits.headerCount", { count: entries.length })}</span>
        <CopyButton value={copyValue} />
      </div>
      <div className="min-h-0 flex-1 divide-y overflow-auto">
        {entries.map(([name, values]) => (
          <div key={name} className="grid gap-1 px-3 py-2.5 text-xs sm:grid-cols-[180px_minmax(0,1fr)] sm:gap-4">
            <code className="break-all text-muted-foreground">{name}</code>
            <div className="min-w-0 space-y-1">{values.map((value, index) => <code key={`${name}-${index}`} className="block break-all">{value}</code>)}</div>
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
    <div className="flex h-full min-h-0 flex-col overflow-hidden rounded-lg border">
      <div className="flex h-10 shrink-0 items-center justify-between border-b bg-muted/20 px-3">
        <span className="text-[11px] text-muted-foreground">{t("audits.errorFrameCount", { count: attempt.errorChain.length })}</span>
        <CopyButton value={copyValue} />
      </div>
      <ol className="min-h-0 flex-1 space-y-2 overflow-auto p-3">
        {attempt.errorChain.map((frame, index) => (
          <li key={`${frame.type}-${index}`} className="rounded-md bg-muted/45 p-3">
            <div className="flex items-center gap-2"><Badge variant="outline" className="font-mono text-[10px]">#{index + 1}</Badge><code className="break-all text-[11px] text-muted-foreground">{frame.type}</code></div>
            <pre className="mt-2 whitespace-pre-wrap break-words font-mono text-xs leading-5">{frame.message}</pre>
          </li>
        ))}
      </ol>
    </div>
  );
}

function EmptyPanel({ icon, message }: { icon: ReactNode; message: string }) {
  return <div className="flex h-full min-h-40 flex-col items-center justify-center gap-2 rounded-lg border border-dashed text-muted-foreground [&_svg]:size-6 [&_svg]:stroke-1"><span>{icon}</span><p className="text-xs">{message}</p></div>;
}

function formattedResponseBody(attempt: AuditAttemptDTO): string {
  if (attempt.responseBodyEncoding !== "utf8") return attempt.responseBody;
  const contentType = Object.entries(attempt.responseHeaders).find(([name]) => name.toLowerCase() === "content-type")?.[1].join(";") ?? "";
  if (!contentType.toLowerCase().includes("json")) return attempt.responseBody;
  try {
    return JSON.stringify(JSON.parse(attempt.responseBody), null, 2);
  } catch {
    return attempt.responseBody;
  }
}

function sourceLabel(attempt: AuditAttemptDTO, t: (key: string, options?: Record<string, unknown>) => string): string {
  if (attempt.source === "upstream_http") return t("audits.upstreamHttpFailure", { status: attempt.upstreamStatusCode ?? "-" });
  if (attempt.source === "gateway_transport") return t("audits.gatewayTransportFailure");
  return t("audits.credentialFailure");
}

function StatusBadge({ statusCode }: { statusCode: number }) {
  const className = statusCode >= 500
    ? "bg-red-500/10 text-red-700 dark:text-red-300"
    : statusCode >= 400 ? "bg-amber-500/10 text-amber-700 dark:text-amber-300"
      : statusCode >= 200 && statusCode < 300 ? "bg-emerald-500/10 text-emerald-700 dark:text-emerald-300" : "bg-muted text-muted-foreground";
  return <Badge variant="secondary" className={cn("min-w-9 justify-center px-1.5 tabular-nums", className)}>{statusCode}</Badge>;
}
