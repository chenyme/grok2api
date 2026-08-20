import { useQuery } from "@tanstack/react-query";
import {
  CheckCircle2,
  FileText,
  Globe2,
  KeyRound,
  ListTree,
  Network,
  Server,
  TriangleAlert,
} from "lucide-react";
import { useMemo, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";

import { Badge } from "@/components/ui/badge";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { getRequestAudit, type AuditAttemptDTO, type AuditDTO } from "@/features/audits/request-audits-api";
import { CopyButton } from "@/shared/components/copy-button";
import { ErrorState, LoadingState } from "@/shared/components/data-state";
import { cn } from "@/shared/lib/cn";
import { formatDateTime, formatNumber } from "@/shared/lib/format";

const AUDIT_DETAIL_CACHE_TIME_MS = 60_000;
const PRE_UPSTREAM_ERROR_CODES = new Set([
  "model_not_allowed",
  "upstream_cooling",
  "upstream_model_cooling",
  "upstream_model_unavailable",
  "upstream_quota_exhausted",
  "upstream_saturated",
  "upstream_unavailable",
]);

export function RequestAuditDetailDialog({
  audit,
  open,
  onOpenChange,
}: {
  audit: AuditDTO | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { t, i18n } = useTranslation();
  const detailQuery = useQuery({
    queryKey: ["request-audits", "detail", audit?.id],
    queryFn: ({ signal }) => getRequestAudit(audit?.id ?? "", signal),
    enabled: open && audit !== null,
    gcTime: AUDIT_DETAIL_CACHE_TIME_MS,
  });

  const activeAudit = detailQuery.data?.audit ?? audit;
  const attempts = detailQuery.data?.attempts ?? [];

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="flex h-[min(700px,calc(100svh-2rem))] max-h-[calc(100svh-2rem)] min-h-0 flex-col gap-0 overflow-hidden p-0 text-xs sm:max-w-[900px]">
        <DialogHeader className="shrink-0 px-5 pb-3 pt-4 pr-12">
          <div className="flex items-center gap-2">
            <DialogTitle>{t("audits.detailTitle")}</DialogTitle>
            {activeAudit ? (
              <StatusBadge
                statusCode={activeAudit.statusCode}
                failed={Boolean(activeAudit.errorCode) || activeAudit.statusCode >= 400}
              />
            ) : null}
          </div>
          <DialogDescription asChild>
            <div className="mt-1.5 flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1 text-[11px] font-normal text-muted-foreground">
              {activeAudit?.requestId ? (
                <span className="max-w-[280px] truncate" title={activeAudit.requestId}>
                  {activeAudit.requestId}
                </span>
              ) : null}
              {activeAudit?.clientIp ? (
                <>
                  {activeAudit.requestId ? <span aria-hidden="true">·</span> : null}
                  <span className="inline-flex items-center gap-1">
                    <Globe2 className="size-3" />
                    {activeAudit.clientIp}
                  </span>
                </>
              ) : null}
              {activeAudit?.operation ? (
                <>
                  {activeAudit.requestId || activeAudit.clientIp ? <span aria-hidden="true">·</span> : null}
                  <span>{activeAudit.operation}</span>
                </>
              ) : null}
              {activeAudit ? (
                <>
                  {activeAudit.requestId || activeAudit.clientIp || activeAudit.operation ? <span aria-hidden="true">·</span> : null}
                  <span>{formatDateTime(activeAudit.createdAt, i18n.language)}</span>
                </>
              ) : null}
            </div>
          </DialogDescription>
        </DialogHeader>

        {detailQuery.isPending && !activeAudit ? <LoadingState className="min-h-0 flex-1" /> : null}
        {detailQuery.isError ? (
          <ErrorState message={detailQuery.error.message} onRetry={() => void detailQuery.refetch()} />
        ) : null}

        {activeAudit ? (
          <Tabs defaultValue="overview" className="flex min-h-0 flex-1 flex-col overflow-hidden">
            <div className="shrink-0 overflow-x-auto bg-muted/15 px-4 py-2 sm:px-5">
              <TabsList className="h-8 w-max">
                <TabsTrigger value="overview" className="gap-1.5 px-3 text-xs">
                  <FileText className="size-3.5" />
                  {t("audits.requestOverview")}
                </TabsTrigger>
                <TabsTrigger value="requestMetadata" className="gap-1.5 px-3 text-xs">
                  <ListTree className="size-3.5" />
                  {t("audits.requestMetadata")}
                </TabsTrigger>
                <TabsTrigger value="attempts" className="gap-1.5 px-3 text-xs">
                  <Server className="size-3.5" />
                  {t("audits.upstreamDiagnostics")}
                  {attempts.length > 0 ? (
                    <Badge variant="secondary" className="ml-1 h-4 px-1 text-[10px] font-mono">
                      {attempts.length}
                    </Badge>
                  ) : null}
                </TabsTrigger>
              </TabsList>
            </div>

            <TabsContent value="overview" className="min-h-0 flex-1 overflow-y-auto overscroll-contain p-4 focus-visible:outline-none sm:p-5">
              <RequestOverviewPanel audit={activeAudit} />
            </TabsContent>

            <TabsContent value="requestMetadata" className="min-h-0 flex-1 overflow-hidden px-4 pb-4 pt-3 focus-visible:outline-none sm:px-5 sm:pb-5">
              <RequestMetadataPanel audit={activeAudit} />
            </TabsContent>

            <TabsContent value="attempts" className="min-h-0 flex-1 overflow-hidden focus-visible:outline-none">
              <UpstreamAttemptsPanel audit={activeAudit} attempts={attempts} />
            </TabsContent>
          </Tabs>
        ) : null}
      </DialogContent>
    </Dialog>
  );
}

function RequestOverviewPanel({ audit }: { audit: AuditDTO }) {
  const { t, i18n } = useTranslation();

  const tokenSummary = useMemo(() => {
    if (!audit.totalTokens && !audit.inputTokens && !audit.outputTokens) return null;
    const parts = [
      `${t("audits.input")} ${formatNumber(audit.inputTokens, i18n.language)}`,
    ];
    if (audit.cachedInputTokens > 0) {
      parts.push(`(${t("audits.cached")} ${formatNumber(audit.cachedInputTokens, i18n.language)})`);
    }
    parts.push(`· ${t("audits.output")} ${formatNumber(audit.outputTokens, i18n.language)}`);
    if (audit.reasoningTokens > 0) {
      parts.push(`(${t("audits.reasoning")} ${formatNumber(audit.reasoningTokens, i18n.language)})`);
    }
    parts.push(`· ${t("audits.total")} ${formatNumber(audit.totalTokens, i18n.language)}`);
    return parts.join(" ");
  }, [audit, t, i18n.language]);

  const costDisplay = useMemo(() => {
    const costTicks = audit.costInUsdTicks > 0 ? audit.costInUsdTicks : audit.estimatedCostInUsdTicks;
    if (!costTicks) return "$0";
    const usd = (costTicks / 100_000_000).toFixed(6);
    return `$${usd}${audit.costInUsdTicks <= 0 && audit.estimatedCostInUsdTicks > 0 ? ` (${t("audits.estimated")})` : ""}`;
  }, [audit, t]);

  const durationDisplay = useMemo(() => {
    let text = `${formatNumber(audit.durationMs, i18n.language)} ms`;
    if (audit.firstTokenMs) {
      text += ` (${t("audits.firstTokenMs")}: ${formatNumber(audit.firstTokenMs, i18n.language)} ms)`;
    }
    return text;
  }, [audit, t, i18n.language]);

  return (
    <div className="grid gap-2.5 sm:grid-cols-2">
      <OverviewField
        label={t("audits.targetAccount")}
        value={audit.accountName || (audit.accountId ? `#${audit.accountId}` : "-")}
      />
      <OverviewField
        label={t("audits.requestModel")}
        value={audit.modelPublicId || "-"}
        copy={Boolean(audit.modelPublicId)}
      />
      <OverviewField
        label={t("audits.upstreamModel")}
        value={audit.modelUpstreamModel || "-"}
      />
      <OverviewField
        label={t("audits.clientApiKey")}
        value={audit.clientKeyName || (audit.clientKeyId ? `#${audit.clientKeyId}` : "-")}
      />
      <OverviewField
        label={t("audits.clientIp")}
        value={audit.clientIp || "-"}
      />
      <OverviewField
        label={t("audits.egressNode")}
        value={audit.egressNodeName || (audit.egressNodeId ? `#${audit.egressNodeId}` : "-")}
      />
      <OverviewField
        label={t("audits.duration")}
        value={durationDisplay}
      />
      <OverviewField
        label={t("audits.cost")}
        value={costDisplay}
      />
      {audit.errorCode ? (
        <OverviewField
          className="sm:col-span-2"
          label={t("audits.errorLabel")}
          value={audit.errorCode}
          copy
        />
      ) : null}
      {tokenSummary ? (
        <OverviewField
          className="sm:col-span-2"
          label={t("audits.tokenUsage")}
          value={tokenSummary}
        />
      ) : null}
      {audit.mediaInputImages > 0 || audit.mediaOutputImages > 0 || audit.mediaOutputSeconds > 0 ? (
        <OverviewField
          className="sm:col-span-2"
          label={t("audits.mediaInput")}
          value={[
            audit.mediaInputImages > 0 ? `${t("audits.mediaInput")}: ${t("audits.imageCount", { count: audit.mediaInputImages })}` : "",
            audit.mediaOutputImages > 0 ? `${t("audits.mediaOutput")}: ${t("audits.imageCount", { count: audit.mediaOutputImages })}` : "",
            audit.mediaOutputSeconds > 0 ? t("audits.secondsCount", { count: audit.mediaOutputSeconds }) : "",
          ].filter(Boolean).join(" · ")}
        />
      ) : null}
    </div>
  );
}

function RequestMetadataPanel({ audit }: { audit: AuditDTO }) {
  const { t } = useTranslation();
  const headers = useMemo(() => audit.requestHeaders ?? {}, [audit.requestHeaders]);

  if (!audit.requestMethod && !audit.requestPath && Object.keys(headers).length === 0) {
    return <EmptyPanel icon={<FileText />} message={t("audits.noRequestMetadata")} />;
  }

  return (
    <div className="flex h-full min-h-0 flex-col gap-4">
      <section className="shrink-0">
        <p className="mb-2 px-1 text-[11px] font-medium text-muted-foreground">{t("audits.requestPath")}</p>
        <div className="flex h-10 min-w-0 items-center gap-3 rounded-lg bg-muted/15 px-3">
          <span className="shrink-0 text-xs text-muted-foreground">
            {audit.requestMethod || "-"}
          </span>
          <span className="min-w-0 flex-1 truncate text-xs" title={audit.requestPath}>
            {audit.requestPath || "-"}
          </span>
          {audit.requestPath ? <CopyButton value={audit.requestPath} /> : null}
        </div>
      </section>
      <section className="min-h-0 flex-1">
        <HeadersPanel title={t("audits.requestHeaders")} headers={headers} emptyMessage={t("audits.noRequestHeaders")} />
      </section>
    </div>
  );
}

function UpstreamAttemptsPanel({
  audit,
  attempts,
}: {
  audit: AuditDTO;
  attempts: AuditAttemptDTO[];
}) {
  const { t } = useTranslation();
  const [selectedNumber, setSelectedNumber] = useState<number | null>(null);

  const selectedAttempt = attempts.find((attempt) => attempt.number === selectedNumber) ?? attempts[0];

  if (attempts.length === 0) {
    const isSuccess = audit.statusCode >= 200 && audit.statusCode < 300 && !audit.errorCode;
    if (isSuccess) {
      return (
        <div className="flex h-full min-h-0 flex-col items-center justify-center gap-2 p-6 text-center text-muted-foreground">
          <CheckCircle2 className="size-8 stroke-1 text-emerald-500" />
          <p className="text-xs">{t("audits.successNoAttempts")}</p>
        </div>
      );
    }
    return (
      <div className="flex h-full min-h-0 flex-col items-center justify-center gap-2 p-6 text-center text-muted-foreground">
        <TriangleAlert className="size-8 stroke-1 text-amber-500" />
        <p className="max-w-md text-xs">
          {t(
            audit.errorCode && PRE_UPSTREAM_ERROR_CODES.has(audit.errorCode)
              ? "audits.noUpstreamAttempt"
              : "audits.noFailureAttempts"
          )}
        </p>
        {audit.errorCode ? (
          <Badge variant="outline" className="font-mono text-xs">
            {audit.errorCode}
          </Badge>
        ) : null}
      </div>
    );
  }

  const terminalAttemptNumber = Math.max(...attempts.map((attempt) => attempt.number));

  return (
    <div className="grid h-full min-h-0 flex-1 grid-rows-[auto_minmax(0,1fr)] lg:grid-cols-[190px_minmax(0,1fr)] lg:grid-rows-1">
      <aside className="flex min-h-0 min-w-0 flex-col overflow-hidden border-b border-border/40 bg-muted/25 px-4 py-2 sm:px-5 lg:border-r lg:border-b-0 lg:pr-2.5">
        <p className="mb-0.5 shrink-0 text-xs text-muted-foreground">{t("audits.attemptTimeline")}</p>
        <div className="flex max-h-28 gap-1 overflow-auto lg:min-h-0 lg:max-h-none lg:flex-1 lg:flex-col">
          {attempts.map((attempt) => (
            <AttemptButton
              key={attempt.id}
              attempt={attempt}
              statusCode={attempt.upstreamStatusCode || (attempt.number === terminalAttemptNumber ? audit.statusCode : 0)}
              selected={attempt.number === selectedAttempt.number}
              onClick={() => setSelectedNumber(attempt.number)}
            />
          ))}
        </div>
      </aside>
      <AttemptDetail key={selectedAttempt.id} attempt={selectedAttempt} />
    </div>
  );
}

function AttemptButton({ attempt, statusCode, selected, onClick }: { attempt: AuditAttemptDTO; statusCode: number; selected: boolean; onClick: () => void }) {
  const { t } = useTranslation();
  const Icon = attempt.source === "upstream_http" ? Server : attempt.source === "gateway_transport" ? Network : KeyRound;
  return (
    <button
      type="button"
      className={cn(
        "flex h-8 w-36 shrink-0 items-center justify-between gap-2 rounded-lg px-2.5 text-left text-xs outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring/50 lg:w-full",
        selected ? "bg-accent text-accent-foreground" : "hover:bg-accent/60"
      )}
      aria-pressed={selected}
      onClick={onClick}
    >
      <span className="flex min-w-0 items-center gap-2 truncate">
        <Icon className="size-3.5 shrink-0" />
        {t("audits.attemptNumber", { number: attempt.number })}
      </span>
      {statusCode ? (
        <StatusBadge statusCode={statusCode} failed={attempt.stage === "response_stream"} />
      ) : (
        <span className="shrink-0 text-xs text-muted-foreground">—</span>
      )}
    </button>
  );
}

function AttemptDetail({ attempt }: { attempt: AuditAttemptDTO }) {
  const { t } = useTranslation();
  const hasBody = Boolean(attempt.responseBody);
  const hasHeaders = Object.keys(attempt.responseHeaders).length > 0;
  const hasErrors = attempt.errorChain.length > 0;
  return (
    <main className="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden">
      <Tabs defaultValue="overview" className="flex min-h-0 flex-1 flex-col overflow-hidden px-4 pb-4 sm:px-5">
        <div className="flex shrink-0 flex-wrap items-center gap-2.5 py-3">
          <AttemptSummary attempt={attempt} />
          <div className="ml-auto max-w-full shrink-0 overflow-x-auto pb-0.5">
            <TabsList className="h-7 w-max">
              <TabsTrigger value="overview" className="h-6 px-2.5 text-xs">{t("audits.overview")}</TabsTrigger>
              {hasBody ? <TabsTrigger value="body" className="h-6 px-2.5 text-xs">{t("audits.responseBody")}</TabsTrigger> : null}
              {hasHeaders ? <TabsTrigger value="headers" className="h-6 px-2.5 text-xs">{t("audits.responseHeaders")}</TabsTrigger> : null}
              {hasErrors ? <TabsTrigger value="errors" className="h-6 px-2.5 text-xs">{t("audits.errorChain")}</TabsTrigger> : null}
            </TabsList>
          </div>
        </div>
        <TabsContent value="overview" className="min-h-0 flex-1 overflow-y-auto">
          <AttemptOverview attempt={attempt} />
        </TabsContent>
        {hasBody ? <TabsContent value="body" className="min-h-0 flex-1 overflow-hidden pt-2">
          <AttemptResponseBody attempt={attempt} />
        </TabsContent> : null}
        {hasHeaders ? <TabsContent value="headers" className="min-h-0 flex-1 overflow-hidden pt-2">
          <HeadersPanel title={t("audits.responseHeaders")} headers={attempt.responseHeaders} />
        </TabsContent> : null}
        {hasErrors ? <TabsContent value="errors" className="min-h-0 flex-1 overflow-hidden pt-2">
          <ErrorChainPanel attempt={attempt} />
        </TabsContent> : null}
      </Tabs>
    </main>
  );
}

function AttemptResponseBody({ attempt }: { attempt: AuditAttemptDTO }) {
  const { t } = useTranslation();
  const displayValue = useMemo(() => formattedResponseBody(attempt), [attempt]);
  return (
    <CodePanel
      value={attempt.responseBody}
      displayValue={displayValue}
      emptyMessage={t("audits.emptyResponseBody")}
      encoding={attempt.responseBodyEncoding}
      truncated={attempt.responseBodyTruncated}
    />
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
    : attempt.source === "gateway_transport"
    ? t("audits.gatewayTransportFailure")
    : t("audits.credentialFailure");
  return (
    <div className="flex min-w-0 items-center gap-2">
      <Icon className="size-4 shrink-0 text-destructive" />
      <p className="min-w-0 truncate font-medium">{title}</p>
    </div>
  );
}

function AttemptOverview({ attempt }: { attempt: AuditAttemptDTO }) {
  const { t, i18n } = useTranslation();
  return (
    <div className="grid gap-x-10 gap-y-4 px-1 py-3 sm:grid-cols-2">
      <OverviewField label={t("audits.attemptStartedAt")} value={formatDateTime(attempt.startedAt, i18n.language)} />
      <OverviewField label={t("audits.duration")} value={`${formatNumber(attempt.durationMs, i18n.language)} ms`} />
      <OverviewField label={t("audits.targetAccount")} value={attempt.accountName || (attempt.accountId ? `#${attempt.accountId}` : "-")} />
      <OverviewField label={t("audits.requestMethod")} value={attempt.method || "-"} />
      <OverviewField label={t("audits.requestPath")} value={attempt.requestPath || "-"} />
      <OverviewField label={t("audits.upstreamStatus")} value={attempt.upstreamStatus || (attempt.upstreamStatusCode ? String(attempt.upstreamStatusCode) : "-")} />
      <OverviewField className="sm:col-span-2" label={t("audits.upstreamUrl")} value={attempt.upstreamUrl || t("audits.upstreamUrlUnavailable")} copy={Boolean(attempt.upstreamUrl)} />
      {attempt.transportError ? (
        <OverviewField
          className="sm:col-span-2"
          label={attempt.source === "gateway_transport" ? t("audits.transportError") : t("audits.attemptError")}
          value={attempt.transportError}
          copy
        />
      ) : null}
    </div>
  );
}

function OverviewField({ className, label, value, copy }: { className?: string; label: string; value: string; copy?: boolean }) {
  return (
    <div className={cn("flex min-w-0 items-start gap-3 rounded-lg bg-muted/25 p-3", className)}>
      <div className="min-w-0 flex-1">
        <p className="text-[11px] text-muted-foreground">{label}</p>
        <p className="mt-0.5 break-all text-xs font-medium" title={value}>
          {value}
        </p>
      </div>
      {copy ? (
        <div className="shrink-0 pt-0.5">
          <CopyButton value={value} />
        </div>
      ) : null}
    </div>
  );
}

function CodePanel({
  value,
  displayValue,
  emptyMessage,
  encoding,
  truncated,
}: {
  value: string;
  displayValue: string;
  emptyMessage: string;
  encoding: string;
  truncated: boolean;
}) {
  const { t } = useTranslation();
  if (!value) return <EmptyPanel icon={<FileText />} message={emptyMessage} />;
  return (
    <div className="flex h-full min-h-0 flex-col overflow-hidden rounded-lg bg-muted/20">
      <div className="flex h-10 shrink-0 items-center justify-between px-3">
        <span className="flex min-w-0 items-center gap-2 text-muted-foreground text-[11px]">
          <span>{t("audits.bodyEncoding", { encoding })}</span>
          {truncated ? <Badge variant="outline" className="text-[10px]">{t("audits.bodyTruncated")}</Badge> : null}
        </span>
        <CopyButton value={value} />
      </div>
      <div className="min-h-0 flex-1 overflow-auto p-3">
        <pre className="font-mono text-[11px] leading-relaxed whitespace-pre-wrap break-all select-text">
          {displayValue}
        </pre>
      </div>
    </div>
  );
}

function HeadersPanel({ title, headers, emptyMessage }: { title?: string; headers: Record<string, string[]>; emptyMessage?: string }) {
  const { t } = useTranslation();
  const entries = useMemo(() => Object.entries(headers).sort(([left], [right]) => left.localeCompare(right)), [headers]);
  const copyValue = useMemo(() => JSON.stringify(headers, null, 2), [headers]);
  if (entries.length === 0) return <EmptyPanel icon={<FileText />} message={emptyMessage ?? t("audits.emptyResponseHeaders")} />;
  return (
    <div className="flex h-full min-h-0 flex-col overflow-hidden rounded-lg bg-muted/15">
      <div className="flex h-10 shrink-0 items-center justify-between px-3">
        <span className="flex min-w-0 items-center gap-2 text-[11px]">
          {title ? <span className="truncate font-medium text-foreground">{title}</span> : null}
          <span className="text-muted-foreground">{t("audits.headerItemCount", { count: entries.length })}</span>
        </span>
        <CopyButton value={copyValue} />
      </div>
      <div className="min-h-0 flex-1 space-y-0.5 overflow-auto px-2 pb-2">
        {entries.map(([name, values], entryIndex) => (
          <div key={name} className={cn("grid gap-1 rounded-md px-2.5 py-2 transition-colors hover:bg-background/70 sm:grid-cols-[180px_minmax(0,1fr)] sm:gap-4", entryIndex % 2 === 0 && "bg-background/35")}>
            <span className="break-all font-mono text-[11px] text-muted-foreground">{name}</span>
            <div className="min-w-0 space-y-1">
              {values.map((value, index) => (
                <span key={`${name}-${index}`} className={cn("block break-all font-mono text-[11px]", value === "[REDACTED]" && "font-semibold text-amber-700 dark:text-amber-300")}>{value}</span>
              ))}
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}

function ErrorChainPanel({ attempt }: { attempt: AuditAttemptDTO }) {
  const { t } = useTranslation();
  const copyValue = useMemo(() => JSON.stringify(attempt.errorChain, null, 2), [attempt.errorChain]);
  if (attempt.errorChain.length === 0) return <EmptyPanel icon={<Network />} message={t("audits.emptyErrorChain")} />;
  return (
    <div className="flex h-full min-h-0 flex-col overflow-hidden rounded-lg bg-muted/15">
      <div className="flex h-10 shrink-0 items-center justify-between px-3">
        <span className="text-muted-foreground text-[11px]">{t("audits.errorFrameCount", { count: attempt.errorChain.length })}</span>
        <CopyButton value={copyValue} />
      </div>
      <ol className="min-h-0 flex-1 space-y-3 overflow-auto p-3">
        {attempt.errorChain.map((frame, index) => (
          <li key={`${frame.type}-${index}`} className="rounded-md bg-background/50 p-2.5">
            <div className="flex items-center gap-2 text-muted-foreground text-[11px]">
              <span>#{index + 1}</span>
              <span className="break-all font-medium text-foreground">{frame.type}</span>
            </div>
            <p className="mt-1.5 font-mono text-[11px] whitespace-pre-wrap break-words">{frame.message}</p>
          </li>
        ))}
      </ol>
    </div>
  );
}

function EmptyPanel({ icon, message }: { icon: ReactNode; message: string }) {
  return (
    <div className="flex h-full min-h-40 flex-col items-center justify-center gap-2 rounded-lg bg-muted/15 px-6 text-center text-muted-foreground [&_svg]:size-6 [&_svg]:stroke-1">
      <span>{icon}</span>
      <p className="text-xs">{message}</p>
    </div>
  );
}

function formattedResponseBody(attempt: AuditAttemptDTO): string {
  if (attempt.responseBodyEncoding !== "utf8") return attempt.responseBody;
  const contentType = Object.entries(attempt.responseHeaders).find(([name]) => name.toLowerCase() === "content-type")?.[1].join(";") ?? "";
  if (attempt.stage !== "response_stream" && !contentType.toLowerCase().includes("json")) return attempt.responseBody;
  return formatJSONBody(attempt.responseBody);
}

function formatJSONBody(value: string): string {
  try {
    return JSON.stringify(JSON.parse(value), null, 2);
  } catch {
    return value;
  }
}

function StatusBadge({ statusCode, failed = false }: { statusCode: number; failed?: boolean }) {
  const className = statusCode >= 500
    ? "bg-red-500/10 text-red-700 dark:text-red-300 border-red-500/30"
    : statusCode >= 400
    ? "bg-amber-500/10 text-amber-700 dark:text-amber-300 border-amber-500/30"
    : failed
    ? "bg-amber-500/10 text-amber-700 dark:text-amber-300 border-amber-500/30"
    : statusCode >= 200 && statusCode < 300
    ? "bg-emerald-500/10 text-emerald-700 dark:text-emerald-300 border-emerald-500/30"
    : "bg-muted text-muted-foreground";
  return (
    <Badge variant="outline" className={cn("h-5 min-w-8 justify-center px-1.5 text-xs font-normal", className)}>
      {statusCode}
    </Badge>
  );
}
