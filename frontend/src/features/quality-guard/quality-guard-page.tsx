import { zodResolver } from "@hookform/resolvers/zod";
import { useMutation, useQuery, useQueryClient, type UseMutationResult } from "@tanstack/react-query";
import { Activity, AlertTriangle, BarChart3, Bot, Coins, Eye, Gauge, Pencil, RefreshCw, RotateCcw, RotateCw, Shield, ShieldCheck, ShieldX, TimerReset, Zap } from "lucide-react";
import { useState, type ReactNode } from "react";
import { useForm, useWatch } from "react-hook-form";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import { z } from "zod";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { getQualityGuardStatus, runQualityTest, updateQualityGuardPolicy, type QualityGuardEvent, type QualityGuardNodeState, type QualityGuardPolicy, type QualityGuardStatistics, type QualityGuardStatus, type QualityTestResult } from "@/features/quality-guard/quality-guard-api";
import { listAllEgressNodes, type EgressNodeDTO } from "@/features/settings/settings-api";
import { ErrorState } from "@/shared/components/data-state";
import { PageHeader } from "@/shared/components/page-header";
import { cn } from "@/shared/lib/cn";

export function QualityGuardPage() {
  const { t, i18n } = useTranslation();
  const [manualResults, setManualResults] = useState<Record<string, QualityGuardNodeState>>({});
  const [policyOpen, setPolicyOpen] = useState(false);
  const statusQuery = useQuery({
    queryKey: ["quality-guard"],
    queryFn: getQualityGuardStatus,
    refetchInterval: 5_000,
  });
  const nodesQuery = useQuery({
    queryKey: ["quality-guard-egress-nodes"],
    queryFn: () => listAllEgressNodes({ scope: "grok_build" }),
    refetchInterval: 15_000,
  });
  const testMutation = useMutation({
    mutationFn: ({ nodeId, status }: { nodeId: string; status: QualityGuardStatus }) => runQualityTest(nodeId, status),
    onSuccess: (result, variables) => {
      setManualResults((current) => ({ ...current, [variables.nodeId]: qualityTestState(result, variables.status) }));
      toast.success(t("qualityGuard.testComplete", { speed: formatTPS(result.visibleTokensPerSecond) }));
    },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("errors.generic")),
  });

  const refresh = () => void Promise.all([statusQuery.refetch(), nodesQuery.refetch()]);
  if (statusQuery.isError && !statusQuery.data) return <ErrorState message={statusQuery.error.message} onRetry={refresh} />;

  const status = statusQuery.data;
  const configuredNodeIDs = new Set(status?.config?.node_ids ?? []);
  const nodes = (nodesQuery.data?.items ?? []).filter((node) => !status?.config || configuredNodeIDs.has(node.id));
  const fresh = isFresh(status);
  const guardedNodes = status?.nodes ?? {};
  const quarantined = Object.values(guardedNodes).filter((node) => node.disabled_by_guard).length;
  const enabled = nodes.filter((node) => node.enabled).length;

  return (
    <div className="space-y-6">
      <PageHeader
        title={t("qualityGuard.title")}
        description={t("qualityGuard.description")}
        actions={(
          <Button variant="secondary" size="sm" onClick={refresh} disabled={statusQuery.isFetching || nodesQuery.isFetching}>
            <RefreshCw className={cn((statusQuery.isFetching || nodesQuery.isFetching) && "animate-spin")} />
            {t("common.refresh")}
          </Button>
        )}
      />

      {!status?.available ? <UnavailableState /> : (
        <>
          <section className="grid overflow-hidden rounded-lg bg-card sm:grid-cols-2 xl:grid-cols-4" aria-label={t("qualityGuard.overview")}>
            <Metric icon={fresh ? ShieldCheck : ShieldX} label={t("qualityGuard.serviceStatus")} value={fresh ? t("qualityGuard.running") : t("qualityGuard.stale")} tone={fresh ? "good" : "bad"} />
            <Metric icon={Activity} label={t("qualityGuard.mode")} value={t(`qualityGuard.modes.${status.config?.mode ?? "hybrid"}`)} />
            <Metric icon={Gauge} label={t("qualityGuard.availableNodes")} value={`${enabled} / ${nodes.length}`} />
            <Metric icon={TimerReset} label={t("qualityGuard.quarantinedNodes")} value={String(quarantined)} tone={quarantined ? "bad" : "good"} />
          </section>

          {status.statistics ? <StatisticsPanel statistics={status.statistics} locale={i18n.language} /> : null}

          <section className="overflow-hidden rounded-lg bg-card" aria-labelledby="guard-nodes-title">
            <div className="flex flex-col gap-2 border-b px-4 py-4 sm:flex-row sm:items-center sm:justify-between sm:px-5">
              <div>
                <h2 id="guard-nodes-title" className="text-sm font-medium">{t("qualityGuard.nodes")}</h2>
                <p className="mt-1 text-xs text-muted-foreground">{t("qualityGuard.nodesHelp")}</p>
              </div>
              <span className="text-xs text-muted-foreground">{t("qualityGuard.updatedAt", { time: formatTime(status.updatedAt, i18n.language) })}</span>
            </div>
            <div className="overflow-x-auto">
              <Table className="min-w-[900px]">
                <TableHeader><TableRow>
                  <TableHead>{t("qualityGuard.node")}</TableHead><TableHead>{t("qualityGuard.state")}</TableHead>
                  <TableHead className="text-right">{t("qualityGuard.visibleTPS")}</TableHead><TableHead className="text-right">{t("qualityGuard.firstToken")}</TableHead>
                  <TableHead>{t("qualityGuard.source")}</TableHead><TableHead>{t("qualityGuard.strikes")}</TableHead>
                  <TableHead>{t("qualityGuard.lastObserved")}</TableHead><TableHead className="w-28 text-right">{t("common.actions")}</TableHead>
                </TableRow></TableHeader>
                <TableBody>
                  {nodes.map((node) => <NodeRow key={node.id} node={node} state={manualResults[node.id] ?? guardedNodes[node.id]} locale={i18n.language} status={status} mutation={testMutation} />)}
                </TableBody>
              </Table>
            </div>
          </section>

          <div className="grid gap-3 xl:grid-cols-[minmax(0,3fr)_minmax(300px,2fr)]">
            <EventList events={status.recentEvents ?? []} locale={i18n.language} />
            <Policy status={status} onEdit={() => setPolicyOpen(true)} />
          </div>
          {policyOpen ? <PolicyEditor open onOpenChange={setPolicyOpen} status={status} /> : null}
        </>
      )}
    </div>
  );
}

function StatisticsPanel({ statistics, locale }: { statistics: QualityGuardStatistics; locale: string }) {
  const { t } = useTranslation();
  const anomalies = statistics.active.soft + statistics.active.hard + statistics.passive.soft + statistics.passive.hard;
  const checks = statistics.active.total + statistics.passive.total;
  const items = [
    { icon: BarChart3, label: t("qualityGuard.statisticsChecks"), value: formatCount(checks, locale), detail: t("qualityGuard.statisticsChecksHelp") },
    { icon: Bot, label: t("qualityGuard.statisticsActive"), value: formatCount(statistics.active.total, locale), detail: t("qualityGuard.statisticsActiveDetail", { healthy: formatCount(statistics.active.healthy, locale), errors: formatCount(statistics.active.errors, locale) }) },
    { icon: Eye, label: t("qualityGuard.statisticsPassive"), value: formatCount(statistics.passive.total, locale), detail: t("qualityGuard.statisticsPassiveDetail", { healthy: formatCount(statistics.passive.healthy, locale) }) },
    { icon: Coins, label: t("qualityGuard.statisticsTokens"), value: formatCount(statistics.active.visible_tokens, locale), detail: t("qualityGuard.statisticsTokensHelp") },
    { icon: AlertTriangle, label: t("qualityGuard.statisticsAnomalies"), value: formatCount(anomalies, locale), detail: t("qualityGuard.statisticsAnomalyDetail", { soft: formatCount(statistics.active.soft + statistics.passive.soft, locale), hard: formatCount(statistics.active.hard + statistics.passive.hard, locale) }) },
    { icon: Shield, label: t("qualityGuard.statisticsQuarantines"), value: formatCount(statistics.actions.quarantined, locale), detail: t("qualityGuard.statisticsActionDetail", { restored: formatCount(statistics.actions.restored, locale), suppressed: formatCount(statistics.actions.suppressed, locale) }) },
  ];
  return <section className="overflow-hidden rounded-lg bg-card" aria-labelledby="guard-statistics-title">
    <div className="px-4 py-4 sm:px-5">
      <h2 id="guard-statistics-title" className="text-sm font-medium">{t("qualityGuard.statistics")}</h2>
      <p className="mt-1 text-xs text-muted-foreground">{t("qualityGuard.statisticsSince", { time: formatTime(statistics.started_at, locale) })}</p>
    </div>
    <div className="grid border-t sm:grid-cols-2 xl:grid-cols-3">
      {items.map(({ icon: Icon, label, value, detail }) => <div key={label} className="flex min-h-24 gap-3 border-b p-4 last:border-b-0 sm:[&:nth-child(odd)]:border-r sm:[&:nth-last-child(-n+2)]:border-b-0 xl:border-r xl:[&:nth-child(3n)]:border-r-0 xl:[&:nth-last-child(-n+3)]:border-b-0">
        <span className="flex size-8 shrink-0 items-center justify-center rounded-md bg-secondary text-muted-foreground"><Icon className="size-4" /></span>
        <div className="min-w-0"><p className="text-xs text-muted-foreground">{label}</p><p className="mt-1 text-lg font-medium tabular-nums">{value}</p><p className="mt-1 truncate text-[11px] text-muted-foreground" title={detail}>{detail}</p></div>
      </div>)}
    </div>
  </section>;
}

function Metric({ icon: Icon, label, value, tone }: { icon: typeof Activity; label: string; value: string; tone?: "good" | "bad" }) {
  return <div className="flex min-h-24 items-center gap-3 border-b p-4 last:border-b-0 sm:[&:nth-child(odd)]:border-r xl:border-b-0 xl:border-r xl:last:border-r-0">
    <span className={cn("flex size-9 shrink-0 items-center justify-center rounded-md bg-secondary text-muted-foreground", tone === "good" && "text-emerald-600 dark:text-emerald-400", tone === "bad" && "text-destructive")}><Icon className="size-4" /></span>
    <div className="min-w-0"><p className="text-xs text-muted-foreground">{label}</p><p className="mt-1 truncate text-lg font-medium tabular-nums">{value}</p></div>
  </div>;
}

function NodeRow({ node, state, locale, status, mutation }: { node: EgressNodeDTO; state?: QualityGuardNodeState; locale: string; status: QualityGuardStatus; mutation: UseMutationResult<QualityTestResult, Error, { nodeId: string; status: QualityGuardStatus }> }) {
  const { t } = useTranslation();
  const testing = mutation.isPending && mutation.variables?.nodeId === node.id;
  const classification = state?.last_classification || "unknown";
  return <TableRow>
    <TableCell><div className="font-medium">{node.name}</div><div className="mt-0.5 text-[11px] text-muted-foreground">ID {node.id}</div></TableCell>
    <TableCell><StateBadge node={node} state={state} /></TableCell>
    <TableCell className={cn("text-right font-mono text-xs tabular-nums", classification === "hard" && "font-medium text-destructive", classification === "soft" && "text-amber-600 dark:text-amber-400")}>{state?.last_observed_at ? formatTPS(state.last_visible_tps) : "-"}</TableCell>
    <TableCell className="text-right font-mono text-xs tabular-nums">{state?.last_first_token_ms ? `${state.last_first_token_ms} ms` : "-"}</TableCell>
    <TableCell className="text-xs text-muted-foreground">{state?.last_source ? t(`qualityGuard.sources.${state.last_source}`) : "-"}</TableCell>
    <TableCell className="text-xs tabular-nums">{state ? `${state.passive_soft_strikes} / ${state.active_soft_strikes} / ${state.error_strikes}` : "-"}</TableCell>
    <TableCell className="text-xs text-muted-foreground">{formatTime(state?.last_observed_at, locale)}</TableCell>
    <TableCell className="text-right"><Button variant="ghost" size="sm" disabled={testing || !status.config} onClick={() => mutation.mutate({ nodeId: node.id, status })}><RotateCw className={cn(testing && "animate-spin")} />{t("qualityGuard.test")}</Button></TableCell>
  </TableRow>;
}

function StateBadge({ node, state }: { node: EgressNodeDTO; state?: QualityGuardNodeState }) {
  const { t } = useTranslation();
  if (state?.disabled_by_guard) return <Badge variant="destructive">{t("qualityGuard.quarantined")}</Badge>;
  if (!node.enabled) return <Badge variant="secondary">{t("common.disabled")}</Badge>;
  if (state?.error_strikes) return <Badge variant="outline" className="border-amber-500/40 text-amber-700 dark:text-amber-400">{t("qualityGuard.probeFailed")}</Badge>;
  if (state?.last_classification === "hard" || state?.last_classification === "soft") return <Badge variant="outline" className="border-amber-500/40 text-amber-700 dark:text-amber-400">{t("qualityGuard.suspect")}</Badge>;
  if (state?.last_classification === "healthy") return <Badge variant="outline" className="border-emerald-500/40 text-emerald-700 dark:text-emerald-400">{t("qualityGuard.healthy")}</Badge>;
  return <Badge variant="secondary">{t("qualityGuard.pending")}</Badge>;
}

function EventList({ events, locale }: { events: QualityGuardEvent[]; locale: string }) {
  const { t } = useTranslation();
  return <section className="rounded-lg bg-card p-4 sm:p-5" aria-labelledby="guard-events-title">
    <h2 id="guard-events-title" className="text-sm font-medium">{t("qualityGuard.events")}</h2>
    {events.length === 0 ? <p className="mt-8 text-center text-xs text-muted-foreground">{t("qualityGuard.noEvents")}</p> : <div className="mt-3 space-y-1">
      {[...events].reverse().slice(0, 10).map((event, index) => <div key={`${event.ts}-${index}`} className="grid grid-cols-[minmax(0,1fr)_auto] gap-4 rounded-md px-2 py-2 hover:bg-secondary/40">
        <div className="min-w-0"><p className="truncate text-xs font-medium">{event.node_name || `ID ${event.node_id}`} · {t(`qualityGuard.eventTypes.${event.event}`)}</p><p className="mt-1 truncate text-[11px] text-muted-foreground">{t(`qualityGuard.reasons.${event.reason || "unknown"}`)}{event.visible_tps ? ` · ${formatTPS(event.visible_tps)}` : ""}</p></div>
        <time className="text-[11px] text-muted-foreground">{formatTime(event.ts, locale)}</time>
      </div>)}
    </div>}
  </section>;
}

function Policy({ status, onEdit }: { status: QualityGuardStatus; onEdit: () => void }) {
  const { t } = useTranslation();
  const config = status.config;
  if (!config) return null;
  const rows = [
    [t("qualityGuard.softThreshold"), `${formatTPS(config.soft_tps)} × ${config.consecutive_soft}`],
    [t("qualityGuard.hardThreshold"), formatTPS(config.hard_tps)],
    [t("qualityGuard.activeInterval"), formatDuration(config.active_interval_seconds)],
    [t("qualityGuard.passiveInterval"), formatDuration(config.passive_poll_seconds)],
    [t("qualityGuard.quarantineDuration"), formatDuration(config.quarantine_seconds)],
    [t("qualityGuard.minimumNodes"), String(config.min_healthy_nodes)],
  ];
  return <section className="rounded-lg bg-card p-4 sm:p-5" aria-labelledby="guard-policy-title">
    <div className="flex items-center justify-between gap-3">
      <div className="flex items-center gap-2"><Zap className="size-4 text-muted-foreground" /><h2 id="guard-policy-title" className="text-sm font-medium">{t("qualityGuard.policy")}</h2></div>
      <Button type="button" variant="ghost" size="sm" onClick={onEdit} disabled={!status.editable}><Pencil />{t("qualityGuard.editPolicy")}</Button>
    </div>
    <dl className="mt-4 grid grid-cols-2 gap-x-5 gap-y-4">{rows.map(([label, value]) => <div key={label}><dt className="text-[11px] text-muted-foreground">{label}</dt><dd className="mt-1 text-sm font-medium tabular-nums">{value}</dd></div>)}</dl>
  </section>;
}

const policySchema = z.object({
  mode: z.enum(["active", "passive", "hybrid"]),
  activeIntervalSeconds: z.number().int().min(60).max(86400),
  passivePollSeconds: z.number().int().min(1).max(300),
  softTPS: z.number().min(1).max(10000),
  hardTPS: z.number().min(1).max(10000),
  consecutiveSoft: z.number().int().min(1).max(20),
  consecutiveErrors: z.number().int().min(1).max(20),
  quarantineSeconds: z.number().int().min(30).max(86400),
  minHealthyNodes: z.number().int().min(1).max(1000),
}).refine((value) => value.softTPS < value.hardTPS, { path: ["hardTPS"], message: "softThresholdMustBeLower" });

const DEFAULT_POLICY: QualityGuardPolicy = {
  mode: "hybrid", activeIntervalSeconds: 1800, passivePollSeconds: 5,
  softTPS: 500, hardTPS: 1000, consecutiveSoft: 2, consecutiveErrors: 2,
  quarantineSeconds: 300, minHealthyNodes: 3,
};

function PolicyEditor({ open, onOpenChange, status }: { open: boolean; onOpenChange: (open: boolean) => void; status: QualityGuardStatus }) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const nodeCount = status.config?.node_ids.length ?? 1;
  const form = useForm<QualityGuardPolicy>({ resolver: zodResolver(policySchema), defaultValues: policyFromStatus(status) });
  const mode = useWatch({ control: form.control, name: "mode" });
  const softTPS = useWatch({ control: form.control, name: "softTPS" });
  const hardTPS = useWatch({ control: form.control, name: "hardTPS" });
  const thresholdsInvalid = Number.isFinite(softTPS) && Number.isFinite(hardTPS) && softTPS >= hardTPS;
  const mutation = useMutation({
    mutationFn: updateQualityGuardPolicy,
    onSuccess: () => {
      toast.success(t("qualityGuard.policySaved"));
      onOpenChange(false);
      void queryClient.invalidateQueries({ queryKey: ["quality-guard"] });
      window.setTimeout(() => void queryClient.invalidateQueries({ queryKey: ["quality-guard"] }), 1_500);
    },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("errors.generic")),
  });

  const setMode = (value: QualityGuardPolicy["mode"]) => form.setValue("mode", value, { shouldDirty: true, shouldValidate: true });
  const resetDefaults = () => form.reset({ ...DEFAULT_POLICY, minHealthyNodes: Math.min(DEFAULT_POLICY.minHealthyNodes, nodeCount) });

  return <Dialog open={open} onOpenChange={onOpenChange}>
    <DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-2xl">
      <DialogHeader><DialogTitle>{t("qualityGuard.editPolicyTitle")}</DialogTitle><DialogDescription>{t("qualityGuard.editPolicyDescription")}</DialogDescription></DialogHeader>
      <form className="space-y-5" onSubmit={form.handleSubmit((value) => mutation.mutate(value))}>
        <div className="space-y-2">
          <Label>{t("qualityGuard.mode")}</Label>
          <div role="radiogroup" aria-label={t("qualityGuard.mode")} className="grid grid-cols-3 rounded-md bg-secondary p-1">
            {(["passive", "hybrid", "active"] as const).map((value) => <button key={value} type="button" role="radio" aria-checked={mode === value} onClick={() => setMode(value)} className={cn("h-8 rounded-sm px-2 text-xs text-muted-foreground transition-colors", mode === value && "bg-background font-medium text-foreground shadow-sm")}>{t(`qualityGuard.modes.${value}`)}</button>)}
          </div>
        </div>
        <div className="grid gap-4 sm:grid-cols-2">
          <PolicyField id="guard-active-interval" label={t("qualityGuard.activeIntervalSeconds")} error={form.formState.errors.activeIntervalSeconds?.message}><Input id="guard-active-interval" type="number" min={60} max={86400} step={60} {...form.register("activeIntervalSeconds", { valueAsNumber: true })} /></PolicyField>
          <PolicyField id="guard-passive-interval" label={t("qualityGuard.passiveIntervalSeconds")} error={form.formState.errors.passivePollSeconds?.message}><Input id="guard-passive-interval" type="number" min={1} max={300} {...form.register("passivePollSeconds", { valueAsNumber: true })} /></PolicyField>
          <PolicyField id="guard-soft-tps" label={t("qualityGuard.softThreshold")} error={form.formState.errors.softTPS?.message}><Input id="guard-soft-tps" type="number" min={1} max={10000} step="any" {...form.register("softTPS", { valueAsNumber: true })} /></PolicyField>
          <PolicyField id="guard-hard-tps" label={t("qualityGuard.hardThreshold")} error={form.formState.errors.hardTPS?.message}><Input id="guard-hard-tps" type="number" min={1} max={10000} step="any" {...form.register("hardTPS", { valueAsNumber: true })} /></PolicyField>
          <PolicyField id="guard-soft-strikes" label={t("qualityGuard.consecutiveSoft")} error={form.formState.errors.consecutiveSoft?.message}><Input id="guard-soft-strikes" type="number" min={1} max={20} {...form.register("consecutiveSoft", { valueAsNumber: true })} /></PolicyField>
          <PolicyField id="guard-error-strikes" label={t("qualityGuard.consecutiveErrors")} error={form.formState.errors.consecutiveErrors?.message}><Input id="guard-error-strikes" type="number" min={1} max={20} {...form.register("consecutiveErrors", { valueAsNumber: true })} /></PolicyField>
          <PolicyField id="guard-quarantine-seconds" label={t("qualityGuard.quarantineSeconds")} error={form.formState.errors.quarantineSeconds?.message}><Input id="guard-quarantine-seconds" type="number" min={30} max={86400} step={30} {...form.register("quarantineSeconds", { valueAsNumber: true })} /></PolicyField>
          <PolicyField id="guard-minimum-nodes" label={t("qualityGuard.minimumNodes")} error={form.formState.errors.minHealthyNodes?.message}><Input id="guard-minimum-nodes" type="number" min={1} max={nodeCount} {...form.register("minHealthyNodes", { valueAsNumber: true, max: nodeCount })} /></PolicyField>
        </div>
        {thresholdsInvalid ? <p className="text-xs text-destructive">{t("qualityGuard.softThresholdMustBeLower")}</p> : null}
        <DialogFooter className="gap-2 sm:justify-between">
          <Button type="button" variant="ghost" size="sm" onClick={resetDefaults}><RotateCcw />{t("qualityGuard.restoreDefaults")}</Button>
          <div className="flex justify-end gap-2"><Button type="button" variant="secondary" size="sm" onClick={() => onOpenChange(false)}>{t("common.cancel")}</Button><Button type="submit" size="sm" disabled={mutation.isPending}>{t("common.save")}</Button></div>
        </DialogFooter>
      </form>
    </DialogContent>
  </Dialog>;
}

function PolicyField({ id, label, error, children }: { id: string; label: string; error?: string; children: ReactNode }) {
  const { t } = useTranslation();
  return <div className="space-y-2"><Label htmlFor={id}>{label}</Label>{children}{error && error !== "softThresholdMustBeLower" ? <p className="text-xs text-destructive">{t("qualityGuard.invalidPolicyValue")}</p> : null}</div>;
}

function policyFromStatus(status: QualityGuardStatus): QualityGuardPolicy {
  const config = status.config;
  if (!config) return DEFAULT_POLICY;
  return {
    mode: config.mode, activeIntervalSeconds: config.active_interval_seconds,
    passivePollSeconds: config.passive_poll_seconds, softTPS: config.soft_tps, hardTPS: config.hard_tps,
    consecutiveSoft: config.consecutive_soft, consecutiveErrors: config.consecutive_errors,
    quarantineSeconds: config.quarantine_seconds, minHealthyNodes: config.min_healthy_nodes,
  };
}

function UnavailableState() {
  const { t } = useTranslation();
  return <div className="flex min-h-72 flex-col items-center justify-center rounded-lg bg-card px-6 text-center"><ShieldX className="size-7 text-muted-foreground" /><h2 className="mt-4 text-sm font-medium">{t("qualityGuard.unavailable")}</h2><p className="mt-2 max-w-md text-xs leading-5 text-muted-foreground">{t("qualityGuard.unavailableHelp")}</p></div>;
}

function isFresh(status?: QualityGuardStatus): boolean {
  if (!status?.available || !status.updatedAt || !status.config) return false;
  return Date.now() / 1000 - status.updatedAt < Math.max(60, status.config.passive_poll_seconds * 3);
}
function qualityTestState(result: QualityTestResult, status: QualityGuardStatus): QualityGuardNodeState {
  const softTPS = status.config?.soft_tps ?? 500;
  const hardTPS = status.config?.hard_tps ?? 1000;
  let classification = "healthy";
  let reason = "within_threshold";
  if (!result.expectedMatched) { classification = "soft"; reason = "expected_marker_missing"; }
  else if (result.visibleTokens < 32) { classification = "soft"; reason = "insufficient_visible_tokens"; }
  else if (result.visibleTokensPerSecond >= hardTPS) { classification = "hard"; reason = "hard_tps"; }
  else if (result.visibleTokensPerSecond >= softTPS) { classification = "soft"; reason = "soft_tps"; }
  const now = Date.now() / 1000;
  return {
    active_soft_strikes: classification === "soft" ? 1 : classification === "hard" ? (status.config?.consecutive_soft ?? 2) : 0,
    passive_soft_strikes: 0, error_strikes: 0, quarantined_until: 0, disabled_by_guard: false,
    last_reason: reason, last_probe_at: now, last_observed_at: now, last_source: "active",
    last_classification: classification, last_visible_tps: result.visibleTokensPerSecond,
    last_visible_tokens: result.visibleTokens, last_first_token_ms: result.firstTokenMs, last_duration_ms: result.durationMs,
  };
}
function formatTPS(value: number): string { return `${new Intl.NumberFormat(undefined, { maximumFractionDigits: 1 }).format(value)} Token/s`; }
function formatCount(value: number, locale: string): string { return new Intl.NumberFormat(locale, { maximumFractionDigits: 0 }).format(value); }
function formatDuration(seconds: number): string { if (seconds < 60) return `${seconds}s`; if (seconds % 3600 === 0) return `${seconds / 3600}h`; return `${seconds / 60}m`; }
function formatTime(value: number | undefined, locale: string): string { return value ? new Intl.DateTimeFormat(locale, { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", second: "2-digit" }).format(new Date(value * 1000)) : "-"; }
