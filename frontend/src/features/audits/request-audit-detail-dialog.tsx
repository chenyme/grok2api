import { useQuery } from "@tanstack/react-query";
import {
  Check,
  CheckCircle2,
  Code2,
  Copy,
  FileCode,
  FileText,
  Globe2,
  KeyRound,
  Network,
  Server,
  TriangleAlert,
} from "lucide-react";
import { useMemo, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { getRequestAudit, type AuditAttemptDTO, type AuditDTO } from "@/features/audits/request-audits-api";
import { copyToClipboard } from "@/shared/clipboard";
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
      <DialogContent className="flex h-[min(680px,calc(100svh-2rem))] max-h-[calc(100svh-2rem)] min-h-0 flex-col gap-0 overflow-hidden p-0 text-xs sm:max-w-4xl">
        <DialogHeader className="shrink-0 border-b border-border/60 px-5 py-3.5 pr-12">
          <div className="flex items-center gap-2">
            <DialogTitle className="text-sm font-semibold">{t("audits.detailTitle")}</DialogTitle>
            {activeAudit ? (
              <StatusBadge
                statusCode={activeAudit.statusCode}
                failed={Boolean(activeAudit.errorCode) || activeAudit.statusCode >= 400}
              />
            ) : null}
          </div>
          <DialogDescription className="mt-1 flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1">
            <span className="font-mono text-muted-foreground truncate max-w-[240px]" title={activeAudit?.requestId}>
              {activeAudit?.requestId}
            </span>
            {activeAudit?.clientIp ? (
              <Badge variant="outline" className="text-[10px] px-1.5 py-0">
                <Globe2 className="mr-1 size-2.5" />
                {activeAudit.clientIp}
              </Badge>
            ) : null}
            {activeAudit?.operation ? (
              <Badge variant="secondary" className="text-[10px] px-1.5 py-0 uppercase">
                {activeAudit.operation}
              </Badge>
            ) : null}
            {activeAudit ? (
              <span className="text-[11px] text-muted-foreground">
                {formatDateTime(activeAudit.createdAt, i18n.language)}
              </span>
            ) : null}
          </DialogDescription>
        </DialogHeader>

        {detailQuery.isPending && !activeAudit ? <LoadingState className="min-h-0 flex-1" /> : null}
        {detailQuery.isError ? (
          <ErrorState message={detailQuery.error.message} onRetry={() => void detailQuery.refetch()} />
        ) : null}

        {activeAudit ? (
          <Tabs defaultValue="overview" className="flex min-h-0 flex-1 flex-col overflow-hidden">
            <div className="flex shrink-0 items-center justify-between border-b border-border/40 bg-muted/20 px-5 py-2">
              <TabsList className="h-8">
                <TabsTrigger value="overview" className="gap-1.5 px-3 text-xs">
                  <FileText className="size-3.5" />
                  {t("audits.requestOverview")}
                </TabsTrigger>
                <TabsTrigger value="requestBody" className="gap-1.5 px-3 text-xs">
                  <Code2 className="size-3.5" />
                  {t("audits.clientRequestBody")}
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

            <TabsContent value="overview" className="min-h-0 flex-1 overflow-y-auto p-5 focus-visible:outline-none">
              <RequestOverviewPanel audit={activeAudit} />
            </TabsContent>

            <TabsContent value="requestBody" className="min-h-0 flex-1 overflow-hidden p-4 focus-visible:outline-none">
              <ClientRequestBodyPanel audit={activeAudit} />
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
    return `$${usd}${audit.costInUsdTicks <= 0 && audit.estimatedCostInUsdTicks > 0 ? " (估算)" : ""}`;
  }, [audit]);

  const durationDisplay = useMemo(() => {
    let text = `${formatNumber(audit.durationMs, i18n.language)} ms`;
    if (audit.firstTokenMs) {
      text += ` (${t("audits.firstTokenMs")}: ${formatNumber(audit.firstTokenMs, i18n.language)} ms)`;
    }
    return text;
  }, [audit, t, i18n.language]);

  return (
    <div className="grid gap-x-8 gap-y-4 sm:grid-cols-2">
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
            audit.mediaInputImages > 0 ? `${t("audits.mediaInput")}: ${audit.mediaInputImages} 张` : "",
            audit.mediaOutputImages > 0 ? `${t("audits.mediaOutput")}: ${audit.mediaOutputImages} 张` : "",
            audit.mediaOutputSeconds > 0 ? `${audit.mediaOutputSeconds} 秒` : "",
          ].filter(Boolean).join(" · ")}
        />
      ) : null}
    </div>
  );
}

function ClientRequestBodyPanel({ audit }: { audit: AuditDTO }) {
  const { t } = useTranslation();
  const [activeView, setActiveView] = useState<"raw_http" | "body" | "headers">("raw_http");
  const [copiedType, setCopiedType] = useState<string | null>(null);

  const rawBody = audit.requestBody ?? "";
  const method = audit.requestMethod || "POST";
  const path = audit.requestPath || "";
  const headers = audit.requestHeaders ?? {};

  const { formattedJson, headerText, rawHttpText, markdownHttpBlock, markdownJsonBlock, byteSize } = useMemo(() => {
    let formatted = rawBody;
    if (rawBody) {
      try {
        const parsed = JSON.parse(rawBody);
        formatted = JSON.stringify(parsed, null, 2);
      } catch {
        formatted = rawBody;
      }
    }

    const headerLines = Object.entries(headers)
      .map(([key, values]) => `${key}: ${values.join(", ")}`)
      .join("\n");

    const requestLine = path ? `${method} ${path} HTTP/1.1` : "";
    const rawHttp = [requestLine, headerLines, "", formatted]
      .filter((segment, idx) => idx === 2 ? true : Boolean(segment))
      .join("\n");

    const mdHttp = "```http\n" + rawHttp + "\n```";
    const mdJson = formatted ? "```json\n" + formatted + "\n```" : "";
    const size = new Blob([rawBody]).size;

    return {
      formattedJson: formatted,
      headerText: headerLines,
      rawHttpText: rawHttp,
      markdownHttpBlock: mdHttp,
      markdownJsonBlock: mdJson,
      byteSize: size,
    };
  }, [rawBody, method, path, headers]);

  async function handleCopy(type: "raw" | "json" | "markdown_http" | "markdown_json" | "headers") {
    let textToCopy = rawHttpText;
    if (type === "json") textToCopy = formattedJson;
    else if (type === "markdown_http") textToCopy = markdownHttpBlock;
    else if (type === "markdown_json") textToCopy = markdownJsonBlock;
    else if (type === "headers") textToCopy = headerText;

    const ok = await copyToClipboard(textToCopy);
    if (ok) {
      setCopiedType(type);
      setTimeout(() => setCopiedType(null), 1800);
      toast.success(t("common.copied"));
    } else {
      toast.error(t("common.copyFailed"));
    }
  }

  if (!rawBody && Object.keys(headers).length === 0 && !path) {
    return <EmptyPanel icon={<FileText />} message={t("audits.noRequestBody")} />;
  }

  return (
    <div className="flex h-full min-h-0 flex-col overflow-hidden rounded-md border border-border/50 bg-muted/20">
      <div className="flex h-11 shrink-0 flex-wrap items-center justify-between gap-2 border-b border-border/40 bg-muted/40 px-3">
        <div className="flex items-center gap-2">
          {method && path ? (
            <Badge variant="outline" className="font-mono text-[10px] uppercase font-semibold">
              {method} {path}
            </Badge>
          ) : null}
          {byteSize > 0 ? (
            <Badge variant="secondary" className="font-mono text-[10px]">
              {formatByteSize(byteSize)}
            </Badge>
          ) : null}
          {Object.keys(headers).length > 0 ? (
            <Badge variant="outline" className="text-[10px] text-muted-foreground">
              {Object.keys(headers).length} headers
            </Badge>
          ) : null}
        </div>

        <div className="flex items-center gap-1.5">
          <div className="flex rounded-md bg-muted/60 p-0.5 text-[11px]">
            <button
              type="button"
              onClick={() => setActiveView("raw_http")}
              className={cn("rounded px-2 py-0.5 transition-colors cursor-pointer", activeView === "raw_http" ? "bg-background font-medium shadow-xs text-foreground" : "text-muted-foreground hover:text-foreground")}
            >
              Raw HTTP
            </button>
            <button
              type="button"
              onClick={() => setActiveView("body")}
              className={cn("rounded px-2 py-0.5 transition-colors cursor-pointer", activeView === "body" ? "bg-background font-medium shadow-xs text-foreground" : "text-muted-foreground hover:text-foreground")}
            >
              Body JSON
            </button>
            <button
              type="button"
              onClick={() => setActiveView("headers")}
              className={cn("rounded px-2 py-0.5 transition-colors cursor-pointer", activeView === "headers" ? "bg-background font-medium shadow-xs text-foreground" : "text-muted-foreground hover:text-foreground")}
            >
              Headers
            </button>
          </div>

          <Button
            type="button"
            variant="ghost"
            size="sm"
            className="h-7 gap-1 px-2 text-[11px] text-muted-foreground hover:text-foreground cursor-pointer"
            onClick={() => void handleCopy(activeView === "body" ? "json" : activeView === "headers" ? "headers" : "raw")}
            title={activeView === "body" ? t("audits.copyBodyJson") : activeView === "headers" ? t("audits.copyHeaders") : t("audits.copyRawHttp")}
          >
            {copiedType === "raw" || copiedType === "json" || copiedType === "headers" ? <Check className="size-3.5 text-emerald-500" /> : <Copy className="size-3.5" />}
            {copiedType === "raw" || copiedType === "json" || copiedType === "headers" ? t("common.copied") : activeView === "body" ? t("audits.copyJson") : activeView === "headers" ? t("audits.copyHeaders") : "复制 HTTP"}
          </Button>
          <Button
            type="button"
            variant="ghost"
            size="sm"
            className="h-7 gap-1 px-2 text-[11px] text-muted-foreground hover:text-foreground cursor-pointer"
            onClick={() => void handleCopy(activeView === "body" ? "markdown_json" : "markdown_http")}
            title={t("audits.copyMarkdownCode")}
          >
            {copiedType === "markdown_http" || copiedType === "markdown_json" ? (
              <Check className="size-3.5 text-emerald-500" />
            ) : (
              <FileCode className="size-3.5" />
            )}
            {copiedType === "markdown_http" || copiedType === "markdown_json" ? t("common.copied") : t("audits.copyMarkdown")}
          </Button>
        </div>
      </div>

      <div className="min-h-0 flex-1 overflow-auto p-3">
        {activeView === "raw_http" ? (
          <pre className="font-mono text-[11px] leading-relaxed whitespace-pre-wrap break-all select-text">
            {rawHttpText}
          </pre>
        ) : activeView === "body" ? (
          formattedJson ? (
            <pre className="font-mono text-[11px] leading-relaxed whitespace-pre-wrap break-all select-text">
              {formattedJson}
            </pre>
          ) : (
            <p className="text-muted-foreground text-[11px] p-2">{t("audits.noRequestBody")}</p>
          )
        ) : (
          Object.keys(headers).length > 0 ? (
            <div className="space-y-1 font-mono text-[11px]">
              {Object.entries(headers).map(([key, values]) => (
                <div key={key} className="flex gap-2 py-1 border-b border-border/20 last:border-0">
                  <span className="font-semibold text-primary/80 min-w-[160px] shrink-0">{key}:</span>
                  <span className="text-muted-foreground break-all">{values.join(", ")}</span>
                </div>
              ))}
            </div>
          ) : (
            <p className="text-muted-foreground text-[11px] p-2">{t("audits.noRequestHeaders")}</p>
          )
        )}
      </div>
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

  return (
    <div className="grid h-full min-h-0 flex-1 grid-rows-[auto_minmax(0,1fr)] lg:grid-cols-[190px_minmax(0,1fr)] lg:grid-rows-1">
      <aside className="flex min-h-0 min-w-0 flex-col overflow-hidden border-b border-border/40 bg-muted/25 p-2.5 lg:border-r lg:border-b-0">
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
  );
}

function AttemptButton({ attempt, selected, onClick }: { attempt: AuditAttemptDTO; selected: boolean; onClick: () => void }) {
  const { t } = useTranslation();
  const Icon = attempt.source === "upstream_http" ? Server : attempt.source === "gateway_transport" ? Network : KeyRound;
  return (
    <button
      type="button"
      className={cn(
        "w-48 shrink-0 rounded-md px-2.5 py-2 text-left outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring/50 lg:w-full",
        selected ? "bg-accent text-accent-foreground font-medium" : "hover:bg-accent/60"
      )}
      aria-pressed={selected}
      onClick={onClick}
    >
      <span className="flex items-center justify-between gap-2">
        <span className="flex min-w-0 items-center gap-2 truncate">
          <Icon className="size-3.5 shrink-0" />
          {t("audits.attemptNumber", { number: attempt.number })}
        </span>
        {attempt.upstreamStatusCode ? (
          <StatusBadge statusCode={attempt.upstreamStatusCode} failed={attempt.stage === "response_stream"} />
        ) : null}
      </span>
    </button>
  );
}

function AttemptDetail({ attempt }: { attempt: AuditAttemptDTO }) {
  const { t } = useTranslation();
  return (
    <main className="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden">
      <Tabs defaultValue="overview" className="flex min-h-0 flex-1 flex-col overflow-hidden px-4 pb-4 sm:px-5">
        <div className="flex shrink-0 flex-wrap items-center gap-x-4 gap-y-2 py-2">
          <AttemptSummary attempt={attempt} />
          <div className="ml-auto max-w-full overflow-x-auto">
            <TabsList className="h-7">
              <TabsTrigger value="overview" className="text-xs px-2.5">{t("audits.overview")}</TabsTrigger>
              <TabsTrigger value="body" className="text-xs px-2.5">{t("audits.responseBody")}</TabsTrigger>
              <TabsTrigger value="headers" className="text-xs px-2.5">{t("audits.responseHeaders")}</TabsTrigger>
              <TabsTrigger value="errors" className="text-xs px-2.5">{t("audits.errorChain")}</TabsTrigger>
            </TabsList>
          </div>
        </div>
        <TabsContent value="overview" className="min-h-0 flex-1 overflow-y-auto">
          <AttemptOverview attempt={attempt} />
        </TabsContent>
        <TabsContent value="body" className="min-h-0 flex-1 overflow-hidden pt-2">
          <AttemptResponseBody attempt={attempt} />
        </TabsContent>
        <TabsContent value="headers" className="min-h-0 flex-1 overflow-hidden pt-2">
          <HeadersPanel headers={attempt.responseHeaders} />
        </TabsContent>
        <TabsContent value="errors" className="min-h-0 flex-1 overflow-hidden pt-2">
          <ErrorChainPanel attempt={attempt} />
        </TabsContent>
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
    <div className={cn("flex min-w-0 items-start gap-3 rounded-md bg-muted/20 p-2.5", className)}>
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
    <div className="flex h-full min-h-0 flex-col overflow-hidden rounded-md bg-muted/25 border border-border/40">
      <div className="flex h-9 shrink-0 items-center justify-between px-3 border-b border-border/30">
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

function HeadersPanel({ headers }: { headers: Record<string, string[]> }) {
  const { t } = useTranslation();
  const entries = useMemo(() => Object.entries(headers), [headers]);
  const copyValue = useMemo(() => JSON.stringify(headers, null, 2), [headers]);
  if (entries.length === 0) return <EmptyPanel icon={<FileText />} message={t("audits.emptyResponseHeaders")} />;
  return (
    <div className="flex h-full min-h-0 flex-col overflow-hidden rounded-md bg-muted/15 border border-border/40">
      <div className="flex h-9 shrink-0 items-center justify-between px-3 border-b border-border/30">
        <span className="text-muted-foreground text-[11px]">{t("audits.headerCount", { count: entries.length })}</span>
        <CopyButton value={copyValue} />
      </div>
      <div className="min-h-0 flex-1 space-y-1 overflow-auto p-2">
        {entries.map(([name, values]) => (
          <div key={name} className="grid gap-1 rounded-md px-2 py-1.5 hover:bg-background/60 sm:grid-cols-[180px_minmax(0,1fr)] sm:gap-4">
            <span className="break-all font-mono text-[11px] text-muted-foreground">{name}</span>
            <div className="min-w-0 space-y-1">
              {values.map((value, index) => (
                <span key={`${name}-${index}`} className="block break-all font-mono text-[11px]">{value}</span>
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
    <div className="flex h-full min-h-0 flex-col overflow-hidden rounded-md bg-muted/15 border border-border/40">
      <div className="flex h-9 shrink-0 items-center justify-between px-3 border-b border-border/30">
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
    <div className="flex h-full min-h-40 flex-col items-center justify-center gap-2 rounded-md bg-muted/15 text-muted-foreground [&_svg]:size-6 [&_svg]:stroke-1">
      <span>{icon}</span>
      <p className="text-xs">{message}</p>
    </div>
  );
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

function formatByteSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(2)} MB`;
}

function StatusBadge({ statusCode, failed = false }: { statusCode: number; failed?: boolean }) {
  const className = failed
    ? "bg-amber-500/10 text-amber-700 dark:text-amber-300 border-amber-500/30"
    : statusCode >= 500
    ? "bg-red-500/10 text-red-700 dark:text-red-300 border-red-500/30"
    : statusCode >= 400
    ? "bg-amber-500/10 text-amber-700 dark:text-amber-300 border-amber-500/30"
    : statusCode >= 200 && statusCode < 300
    ? "bg-emerald-500/10 text-emerald-700 dark:text-emerald-300 border-emerald-500/30"
    : "bg-muted text-muted-foreground";
  return (
    <Badge variant="outline" className={cn("min-w-9 justify-center px-1.5 font-mono text-[11px]", className)}>
      {statusCode}
    </Badge>
  );
}
