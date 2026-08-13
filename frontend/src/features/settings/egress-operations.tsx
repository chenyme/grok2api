import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { CircleAlert, CircleHelp, MoreHorizontal, Network, Pencil, Plus, RefreshCw, Search, Shuffle, Trash2 } from "lucide-react";
import { useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { Table, TableActionCell, TableActionHead, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import {
  createEgressSource,
  deleteEgressSource,
  getEgressOperationsConfig,
  listAllEgressNodes,
  listEgressSources,
  rebalanceEgressAccounts,
  syncEgressSource,
  testEgressNodes,
  updateEgressOperationsConfig,
  updateEgressSource,
  type EgressFallbackConfigDTO,
  type EgressFallbackMode,
  type EgressNodeDTO,
  type EgressOperationsConfigDTO,
  type EgressScope,
  type EgressSourceDTO,
  type EgressSourceInput,
} from "@/features/settings/settings-api";
import { validSubscriptionProxyURL } from "@/features/settings/settings-model";
import { formatDateTime } from "@/shared/lib/format";
import { ErrorState, LoadingState, TableLoadingRow } from "@/shared/components/data-state";
import { DataTableFilters } from "@/shared/components/data-table-filters";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { Pagination } from "@/shared/components/pagination";
import { VirtualTableBody } from "@/shared/components/virtual-table-body";

type SourceForm = Omit<EgressSourceInput, "url" | "proxyURL" | "clearProxyURL"> & { url: string; proxyEnabled: boolean; proxyURL: string };
const emptySource: SourceForm = {
  name: "", scope: "grok_build", enabled: true, url: "", proxyEnabled: false, proxyURL: "", refreshIntervalSeconds: 900, defaultAccountCapacity: 0,
};
// Eight nodes run concurrently; each checks IPv4 and IPv6 in parallel with a
// 15-second ceiling. Keeping a request to 32 nodes leaves enough headroom for
// the admin HTTP timeout.
const egressProbeBatchSize = 32;
const fallbackScopes: EgressScope[] = ["grok_build", "grok_web", "grok_console", "grok_web_asset", "grok_console_asset"];
const fallbackDescriptionKeys: Record<EgressScope, string> = {
  grok_build: "settings.egress.fallbackBuildHelp",
  grok_web: "settings.egress.fallbackWebHelp",
  grok_console: "settings.egress.fallbackConsoleHelp",
  grok_web_asset: "settings.egress.fallbackWebAssetHelp",
  grok_console_asset: "settings.egress.fallbackConsoleAssetHelp",
};

function defaultFallbacks(): Record<EgressScope, EgressFallbackConfigDTO> {
  return {
    grok_build: { mode: "none" }, grok_web: { mode: "none" },
    grok_console: { mode: "none" }, grok_web_asset: { mode: "none" }, grok_console_asset: { mode: "none" },
  };
}

const defaultOperationsForm: Omit<EgressOperationsConfigDTO, "updatedAt"> = {
  probeProvider: "cloudflare", probeIntervalSeconds: 900, autoAssignEnabled: false, autoBalanceEnabled: false, assignmentIntervalSeconds: 300, fallbacks: defaultFallbacks(),
};

function operationsFormFrom(value?: EgressOperationsConfigDTO): Omit<EgressOperationsConfigDTO, "updatedAt"> {
  if (!value) return { ...defaultOperationsForm, fallbacks: defaultFallbacks() };

  const defaults = defaultFallbacks();
  return {
    probeProvider: value.probeProvider,
    probeIntervalSeconds: value.probeIntervalSeconds,
    autoAssignEnabled: value.autoAssignEnabled,
    autoBalanceEnabled: value.autoBalanceEnabled,
    assignmentIntervalSeconds: value.assignmentIntervalSeconds,
    fallbacks: {
      grok_build: { ...defaults.grok_build, ...value.fallbacks.grok_build },
      grok_web: { ...defaults.grok_web, ...value.fallbacks.grok_web },
      grok_console: { ...defaults.grok_console, ...value.fallbacks.grok_console },
      grok_web_asset: { ...defaults.grok_web_asset, ...value.fallbacks.grok_web_asset },
      grok_console_asset: { ...defaults.grok_console_asset, ...value.fallbacks.grok_console_asset },
    },
  };
}

async function testAllEgressNodes() {
  const nodes = await listAllEgressNodes();
  const ids = nodes.items.filter((node) => node.enabled && node.proxyConfigured).map((node) => node.id);
  const result = { requested: 0, healthy: 0, unhealthy: 0, failed: 0 };
  let firstError: unknown;
  for (let index = 0; index < ids.length; index += egressProbeBatchSize) {
    const batchIDs = ids.slice(index, index + egressProbeBatchSize);
    try {
      const batch = await testEgressNodes(batchIDs);
      result.requested += batch.requested;
      result.healthy += batch.healthy;
      result.unhealthy += batch.unhealthy;
    } catch (error) {
      firstError ??= error;
      result.failed += batchIDs.length;
    }
  }
  if (result.requested === 0 && result.failed > 0) {
    throw firstError;
  }
  return result;
}

export function EgressAutomation({ scopeLabel }: { scopeLabel: (scope: EgressScope) => string }) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [operationsDraft, setOperationsDraft] = useState<Omit<EgressOperationsConfigDTO, "updatedAt"> | null>(null);
  const operationsQuery = useQuery({ queryKey: ["egress-operations"], queryFn: getEgressOperationsConfig });
  const nodesQuery = useQuery({ queryKey: ["egress-nodes", "fallback-options"], queryFn: () => listAllEgressNodes() });
  const operationsForm = operationsDraft ?? operationsFormFrom(operationsQuery.data);

  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
    void queryClient.invalidateQueries({ queryKey: ["egress-operations"] });
  };
  const testAll = useMutation({
    mutationFn: testAllEgressNodes,
    onSuccess: (value) => {
      if (value.failed > 0) toast.warning(t("settings.egress.testedPartial", value));
      else toast.success(t("settings.egress.tested", value));
    },
    onError: showError,
    onSettled: invalidate,
  });
  const rebalance = useMutation({
    mutationFn: rebalanceEgressAccounts,
    onSuccess: (value) => { invalidate(); toast.success(t("settings.egress.rebalanced", value)); },
    onError: showError,
  });
  const saveOperations = useMutation({
    mutationFn: () => updateEgressOperationsConfig(operationsForm),
    onSuccess: () => { setOperationsDraft(null); invalidate(); toast.success(t("settings.egress.automationSaved")); },
    onError: showError,
  });

  function setFallback(scope: EgressScope, fallback: EgressFallbackConfigDTO) {
    setOperationsDraft({ ...operationsForm, fallbacks: { ...operationsForm.fallbacks, [scope]: fallback } });
  }

  function setFallbackMode(scope: EgressScope, mode: EgressFallbackMode) {
    const candidates = fallbackNodeCandidates(nodesQuery.data?.items ?? [], scope);
    const current = operationsForm.fallbacks[scope];
    const currentCandidate = candidates.find((node) => node.id === current.nodeId);
    setFallback(scope, {
      mode,
      nodeId: mode === "fixed" ? (currentCandidate?.id ?? candidates[0]?.id) : undefined,
    });
  }

  return (
    <section className="space-y-8">
      <div className="space-y-3">
        <OperationSectionHeader title={t("settings.egress.automation")} help={t("settings.egress.automationHelp")}>
          <ActionTooltip label={t("settings.egress.testAllHelp")}><Button type="button" size="sm" variant="secondary" disabled={testAll.isPending} onClick={() => testAll.mutate()}>{testAll.isPending ? <Spinner /> : <Network />}{t("settings.egress.testAll")}</Button></ActionTooltip>
          <ActionTooltip label={t("settings.egress.rebalanceHelp")}><Button type="button" size="sm" variant="secondary" disabled={rebalance.isPending} onClick={() => rebalance.mutate()}>{rebalance.isPending ? <Spinner /> : <Shuffle />}{t("settings.egress.rebalance")}</Button></ActionTooltip>
          <ActionTooltip label={t("settings.egress.saveAutomationHelp")}><Button type="button" size="sm" disabled={operationsDraft === null || saveOperations.isPending} onClick={() => saveOperations.mutate()}>{saveOperations.isPending ? <Spinner /> : null}{t("common.save")}</Button></ActionTooltip>
        </OperationSectionHeader>

        {operationsQuery.isError ? <ErrorState message={operationsQuery.error.message} onRetry={() => void operationsQuery.refetch()} /> : operationsQuery.isPending ? <LoadingState /> : (
          <div className="space-y-0">
            <AutomationRow controlId="egress-probe-provider" label={t("settings.egress.probeProvider")} description={t("settings.egress.probeProviderHelp")}>
              <Select value={operationsForm.probeProvider} onValueChange={(probeProvider: "ipinfo" | "cloudflare") => setOperationsDraft({ ...operationsForm, probeProvider })}>
                <SelectTrigger id="egress-probe-provider" className="h-8 w-full"><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="ipinfo">IPinfo</SelectItem>
                  <SelectItem value="cloudflare">Cloudflare</SelectItem>
                </SelectContent>
              </Select>
            </AutomationRow>
            <AutomationRow controlId="egress-probe-interval" label={t("settings.egress.probeInterval")} description={t("settings.egress.probeIntervalHelp")}>
              <IntervalInput id="egress-probe-interval" value={operationsForm.probeIntervalSeconds} unit={t("settings.units.seconds")} onChange={(probeIntervalSeconds) => setOperationsDraft({ ...operationsForm, probeIntervalSeconds })} />
            </AutomationRow>
            <AutomationRow controlId="egress-assignment-interval" label={t("settings.egress.assignmentInterval")} description={t("settings.egress.assignmentIntervalHelp")}>
              <IntervalInput id="egress-assignment-interval" value={operationsForm.assignmentIntervalSeconds} unit={t("settings.units.seconds")} onChange={(assignmentIntervalSeconds) => setOperationsDraft({ ...operationsForm, assignmentIntervalSeconds })} />
            </AutomationRow>
            <AutomationRow controlId="egress-auto-assign" label={t("settings.egress.autoAssign")} description={t("settings.egress.autoAssignHelp")}>
              <div className="flex h-8 items-center"><Switch id="egress-auto-assign" checked={operationsForm.autoAssignEnabled} onCheckedChange={(autoAssignEnabled) => setOperationsDraft({ ...operationsForm, autoAssignEnabled })} /></div>
            </AutomationRow>
            <AutomationRow controlId="egress-auto-balance" label={t("settings.egress.autoBalance")} description={t("settings.egress.autoBalanceHelp")}>
              <div className="flex h-8 items-center"><Switch id="egress-auto-balance" checked={operationsForm.autoBalanceEnabled} onCheckedChange={(autoBalanceEnabled) => setOperationsDraft({ ...operationsForm, autoBalanceEnabled })} /></div>
            </AutomationRow>
            <div className="pt-4">
              <div className="flex items-center gap-1.5 px-0.5">
                <h3 className="text-sm font-medium tracking-tight">{t("settings.egress.fallback")}</h3>
                <Tooltip>
                  <TooltipTrigger asChild><button type="button" className="text-muted-foreground transition-colors hover:text-foreground" aria-label={t("settings.egress.fallbackHelp")}><CircleHelp className="size-3.5" /></button></TooltipTrigger>
                  <TooltipContent className="max-w-80">{t("settings.egress.fallbackHelp")}</TooltipContent>
                </Tooltip>
              </div>
              <div className="mt-3 space-y-2">
                {fallbackScopes.map((scope) => {
                  const fallback = operationsForm.fallbacks[scope];
                  const candidates = fallbackNodeCandidates(nodesQuery.data?.items ?? [], scope);
                  const selectedAvailable = candidates.some((node) => node.id === fallback.nodeId);
                  return (
                    <div className="grid min-w-0 gap-2.5 py-1 sm:grid-cols-[minmax(0,2fr)_minmax(0,1fr)] sm:items-center sm:gap-8" key={scope}>
                      <div className="min-w-0">
                        <div className="flex min-h-5 items-center"><Label className="text-xs font-medium">{scopeLabel(scope)}</Label></div>
                        <p className="mt-1 max-w-xl text-xs leading-5 text-muted-foreground">{t(fallbackDescriptionKeys[scope])}</p>
                      </div>
                      <div className={fallback.mode === "fixed" ? "grid min-w-0 gap-2 sm:grid-cols-2" : "grid min-w-0"}>
                        <Select value={fallback.mode} onValueChange={(mode) => setFallbackMode(scope, mode as EgressFallbackMode)}>
                          <SelectTrigger aria-label={t("settings.egress.fallbackMode", { scope: scopeLabel(scope) })}><SelectValue /></SelectTrigger>
                          <SelectContent>
                            <SelectItem value="none">{t("settings.egress.fallbackNone")}</SelectItem>
                            <SelectItem value="direct">{t("settings.egress.fallbackDirect")}</SelectItem>
                            <SelectItem value="fixed" disabled={candidates.length === 0}>{t("settings.egress.fallbackFixed")}</SelectItem>
                          </SelectContent>
                        </Select>
                        {fallback.mode === "fixed" ? (
                          <Select value={selectedAvailable ? (fallback.nodeId ?? "unavailable") : "unavailable"} disabled={candidates.length === 0} onValueChange={(nodeId) => setFallback(scope, { mode: "fixed", nodeId })}>
                            <SelectTrigger aria-label={t("settings.egress.fallbackNode", { scope: scopeLabel(scope) })}><SelectValue /></SelectTrigger>
                            <SelectContent>
                              {!selectedAvailable ? <SelectItem value="unavailable" disabled>{t("settings.egress.fallbackNodeUnavailable")}</SelectItem> : null}
                              {candidates.map((node) => <SelectItem key={node.id} value={node.id}>{node.name} ({scopeLabel(node.scope)})</SelectItem>)}
                            </SelectContent>
                          </Select>
                        ) : null}
                      </div>
                    </div>
                  );
                })}
              </div>
            </div>
          </div>
        )}
      </div>

    </section>
  );
}

export function EgressSources({ scopeLabel }: { scopeLabel: (scope: EgressScope) => string }) {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const [sourceEditing, setSourceEditing] = useState<EgressSourceDTO | null | undefined>(undefined);
  const [sourceForm, setSourceForm] = useState<SourceForm>(emptySource);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [search, setSearch] = useState("");
  const [scopeFilter, setScopeFilter] = useState("");
  const sourcesQuery = useQuery({ queryKey: ["egress-sources"], queryFn: () => listEgressSources() });

  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
    void queryClient.invalidateQueries({ queryKey: ["egress-sources"] });
  };
  const saveSource = useMutation({
    mutationFn: () => {
      const input: EgressSourceInput = {
        name: sourceForm.name, scope: sourceForm.scope, enabled: sourceForm.enabled,
        url: sourceForm.url.trim() || undefined,
        proxyURL: sourceForm.proxyEnabled ? (sourceForm.proxyURL.trim() || undefined) : undefined,
        clearProxyURL: Boolean(sourceEditing?.proxyConfigured && !sourceForm.proxyEnabled),
        refreshIntervalSeconds: sourceForm.refreshIntervalSeconds, defaultAccountCapacity: sourceForm.defaultAccountCapacity,
      };
      return sourceEditing ? updateEgressSource(sourceEditing.id, input) : createEgressSource(input);
    },
    onSuccess: () => { if (!sourceEditing) setPage(1); invalidate(); setSourceEditing(undefined); toast.success(t("settings.egress.sourceSaved")); },
    onError: showError,
  });
  const removeSource = useMutation({
    mutationFn: deleteEgressSource,
    onSuccess: () => { if (page > 1 && pagedSources.length === 1) setPage(page - 1); invalidate(); toast.success(t("settings.egress.sourceDeleted")); },
    onError: showError,
  });
  const syncSource = useMutation({
    mutationFn: syncEgressSource,
    onSuccess: (value) => { invalidate(); toast.success(t("settings.egress.sourceSynced", value)); },
    onError: showError,
  });

  function openSource(value?: EgressSourceDTO) {
    if (!value) {
      setSourceForm(emptySource);
      setSourceEditing(null);
      return;
    }
    setSourceForm({
      name: value.name, scope: value.scope, enabled: value.enabled, url: "", refreshIntervalSeconds: value.refreshIntervalSeconds,
      proxyEnabled: value.proxyConfigured, proxyURL: "", defaultAccountCapacity: value.defaultAccountCapacity,
    });
    setSourceEditing(value);
  }

  const normalizedSearch = search.trim().toLocaleLowerCase();
  const sources = sourcesQuery.data?.items ?? [];
  const filteredSources = sources.filter((source) => {
    if (normalizedSearch && !source.name.toLocaleLowerCase().includes(normalizedSearch)) return false;
    return !scopeFilter || source.scope === scopeFilter;
  });
  const pageCount = Math.max(1, Math.ceil(filteredSources.length / pageSize));
  const currentPage = Math.min(page, pageCount);
  const pagedSources = filteredSources.slice((currentPage - 1) * pageSize, currentPage * pageSize);
  const hasActiveFilters = Boolean(normalizedSearch || scopeFilter);
  const sourceProxyInvalid = sourceForm.proxyEnabled && Boolean(sourceForm.proxyURL.trim()) && !validSubscriptionProxyURL(sourceForm.proxyURL);

  return (
    <section className="space-y-3">
      <OperationSectionHeader title={t("settings.egress.subscriptions")} help={t("settings.egress.subscriptionsHelp")} />

      <DataTableShell
        toolbar={(
          <>
            <div className="flex w-full min-w-0 items-center gap-2 sm:w-auto">
              <div className="relative min-w-0 flex-1 sm:w-64 sm:flex-none">
                <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                <Input className="h-8 pl-9 text-xs" value={search} onChange={(event) => { setSearch(event.target.value); setPage(1); }} placeholder={t("settings.egress.searchSubscriptions")} aria-label={t("settings.egress.searchSubscriptions")} />
              </div>
              <DataTableFilters filters={[{
                id: "subscription-scope", label: t("settings.egress.scope"), value: scopeFilter, onChange: (value) => { setScopeFilter(value); setPage(1); }, options: [
                  { value: "grok_build", label: scopeLabel("grok_build") },
                  { value: "grok_web", label: scopeLabel("grok_web") },
                  { value: "grok_console", label: scopeLabel("grok_console") },
                  { value: "grok_web_asset", label: scopeLabel("grok_web_asset") },
                  { value: "grok_console_asset", label: scopeLabel("grok_console_asset") },
                ],
              }]} />
            </div>
            <ActionTooltip label={t("settings.egress.addSourceHelp")}><Button type="button" size="sm" variant="secondary" onClick={() => openSource()}><Plus />{t("settings.egress.addSource")}</Button></ActionTooltip>
          </>
        )}
        footer={filteredSources.length > 0 ? <Pagination page={currentPage} pageSize={pageSize} total={filteredSources.length} onPageChange={setPage} onPageSizeChange={(value) => { setPageSize(value); setPage(1); }} /> : undefined}
      >
        {sourcesQuery.isError ? <ErrorState message={sourcesQuery.error.message} onRetry={() => void sourcesQuery.refetch()} /> : null}
        {!sourcesQuery.isError ? <Table viewportRows={10} rowHeight={48} className="min-w-[720px] table-fixed">
          <TableHeader><TableRow className="hover:bg-transparent"><TableHead className="w-[22%]">{t("settings.egress.source")}</TableHead><TableHead className="w-[16%] text-center">{t("settings.egress.scope")}</TableHead><TableHead className="w-[13%] text-center">{t("settings.egress.subscriptionRoute")}</TableHead><TableHead className="w-[29%]">{t("settings.egress.lastSync")}</TableHead><TableHead className="w-[15%] text-center">{t("settings.egress.capacity")}</TableHead><TableActionHead /></TableRow></TableHeader>
          {sourcesQuery.isPending ? <TableBody><TableLoadingRow colSpan={6} /></TableBody> : null}
          {!sourcesQuery.isPending && pagedSources.length === 0 ? <TableBody><TableRow><TableCell colSpan={6} className="h-24 text-center text-xs text-muted-foreground">{hasActiveFilters ? t("settings.egress.noSubscriptionMatches") : t("settings.egress.noSources")}</TableCell></TableRow></TableBody> : null}
          {!sourcesQuery.isPending && pagedSources.length > 0 ? <VirtualTableBody items={pagedSources} colSpan={6} rowHeight={48} renderRow={(source) => (
            <TableRow className="group h-12" key={source.id}>
              <TableCell><div className="flex min-w-0 items-center gap-2"><span className={source.enabled ? "size-1.5 shrink-0 rounded-full bg-emerald-500" : "size-1.5 shrink-0 rounded-full bg-muted-foreground/35"} /><span className="truncate text-xs font-medium">{source.name}</span>{source.lastSyncError ? <SourceError message={source.lastSyncError} /> : null}</div></TableCell>
              <TableCell className="text-center"><Badge variant="secondary" className="text-[10px]">{scopeLabel(source.scope)}</Badge></TableCell>
              <TableCell className="text-center"><Badge variant={source.proxyConfigured ? "secondary" : "outline"} className="text-[10px]">{source.proxyConfigured ? t("settings.egress.subscriptionProxyShort") : t("settings.egress.direct")}</Badge></TableCell>
              <TableCell className="text-xs text-muted-foreground">{source.lastSyncedAt ? formatDateTime(source.lastSyncedAt, i18n.language) : t("settings.egress.never")}</TableCell>
              <TableCell className="text-center text-xs tabular-nums">{source.defaultAccountCapacity || t("settings.egress.unlimited")}</TableCell>
              <TableActionCell>
                <DropdownMenu><DropdownMenuTrigger asChild><Button type="button" size="icon" variant="ghost" className="size-8" aria-label={t("common.actions")}><MoreHorizontal /></Button></DropdownMenuTrigger><DropdownMenuContent align="end">
                  <DropdownMenuItem disabled={syncSource.isPending} onClick={() => syncSource.mutate(source.id)}><RefreshCw />{t("settings.egress.sync")}</DropdownMenuItem>
                  <DropdownMenuItem onClick={() => openSource(source)}><Pencil />{t("common.edit")}</DropdownMenuItem>
                  <DropdownMenuSeparator />
                  <DropdownMenuItem className="text-destructive focus:text-destructive" disabled={removeSource.isPending} onClick={() => removeSource.mutate(source.id)}><Trash2 />{t("common.delete")}</DropdownMenuItem>
                </DropdownMenuContent></DropdownMenu>
              </TableActionCell>
            </TableRow>
          )} /> : null}
        </Table> : null}
      </DataTableShell>

      <Dialog open={sourceEditing !== undefined} onOpenChange={(open) => { if (!open) setSourceEditing(undefined); }}>
        <DialogContent className="max-h-[calc(100svh-2rem)] overflow-y-auto sm:max-w-[520px]">
          <DialogHeader className="pr-8"><DialogTitle>{sourceEditing ? t("settings.egress.editSource") : t("settings.egress.addSource")}</DialogTitle><DialogDescription>{t("settings.egress.sourceDialogDescription")}</DialogDescription></DialogHeader>
          <form className="space-y-3.5" onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); saveSource.mutate(); }}>
            <ToggleControl label={t("settings.egress.enabled")} checked={sourceForm.enabled} onChange={(enabled) => setSourceForm({ ...sourceForm, enabled })} />
            <Control label={t("settings.egress.name")}><Input value={sourceForm.name} onChange={(event) => setSourceForm({ ...sourceForm, name: event.target.value })} /></Control>
            <Control label={t("settings.egress.scope")}><ScopeSelect value={sourceForm.scope} onChange={(scope) => setSourceForm({ ...sourceForm, scope })} scopeLabel={scopeLabel} /></Control>
            <Control label={t("settings.egress.subscriptionURL")}><Input type="password" autoComplete="new-password" placeholder={sourceEditing?.urlConfigured ? t("settings.egress.keepConfigured") : "https://..."} value={sourceForm.url} onChange={(event) => setSourceForm({ ...sourceForm, url: event.target.value })} /></Control>
            <ToggleControl label={t("settings.egress.subscriptionProxy")} checked={sourceForm.proxyEnabled} onChange={(proxyEnabled) => setSourceForm({ ...sourceForm, proxyEnabled })} />
            {sourceForm.proxyEnabled ? <Control label={t("settings.egress.subscriptionProxyURL")}><Input type="password" autoComplete="new-password" aria-invalid={sourceProxyInvalid} placeholder={sourceEditing?.proxyConfigured ? t("settings.egress.keepConfigured") : "http://proxy.example:8080"} value={sourceForm.proxyURL} onChange={(event) => setSourceForm({ ...sourceForm, proxyURL: event.target.value })} />{sourceProxyInvalid ? <p className="text-xs text-destructive">{t("settings.egress.invalidSubscriptionProxy")}</p> : null}</Control> : null}
            <div className="grid gap-3 sm:grid-cols-2">
              <Control label={t("settings.egress.refreshInterval")}><Input type="number" min={60} max={86400} value={sourceForm.refreshIntervalSeconds} onChange={(event) => setSourceForm({ ...sourceForm, refreshIntervalSeconds: Number(event.target.value) })} /></Control>
              <Control label={t("settings.egress.capacity")}><Input type="number" min={0} max={100000} placeholder={t("settings.egress.unlimited")} value={sourceForm.defaultAccountCapacity || ""} onChange={(event) => setSourceForm({ ...sourceForm, defaultAccountCapacity: Number(event.target.value) })} /></Control>
            </div>
            <DialogFooter><Button type="button" size="sm" variant="secondary" onClick={() => setSourceEditing(undefined)}>{t("common.cancel")}</Button><Button type="submit" size="sm" disabled={!sourceForm.name.trim() || (!sourceEditing && !sourceForm.url.trim()) || (sourceForm.proxyEnabled && !sourceEditing?.proxyConfigured && !sourceForm.proxyURL.trim()) || sourceProxyInvalid || saveSource.isPending}>{saveSource.isPending ? <Spinner /> : null}{t("common.save")}</Button></DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </section>
  );
}

function fallbackNodeCandidates(nodes: EgressNodeDTO[], scope: EgressScope): EgressNodeDTO[] {
  return nodes.filter((node) => node.enabled && node.proxyConfigured && !node.proxyPool && !node.accountBoundProxy && !nodeCooling(node) && supportsFallbackScope(node.scope, scope));
}

function nodeCooling(node: EgressNodeDTO): boolean {
  return node.cooldownUntil !== undefined && Date.parse(node.cooldownUntil) > Date.now();
}

function supportsFallbackScope(nodeScope: EgressScope, requestScope: EgressScope): boolean {
  if (nodeScope === requestScope) return true;
  if (requestScope === "grok_console" || requestScope === "grok_web_asset") return nodeScope === "grok_web";
  return requestScope === "grok_console_asset" && (nodeScope === "grok_console" || nodeScope === "grok_web");
}

function ScopeSelect({ value, onChange, scopeLabel }: { value: EgressScope; onChange: (value: EgressScope) => void; scopeLabel: (scope: EgressScope) => string }) {
  return <Select value={value} onValueChange={(next) => onChange(next as EgressScope)}><SelectTrigger><SelectValue /></SelectTrigger><SelectContent>{fallbackScopes.map((scope) => <SelectItem key={scope} value={scope}>{scopeLabel(scope)}</SelectItem>)}</SelectContent></Select>;
}

function OperationSectionHeader({ title, help, children }: { title: string; help: string; children?: ReactNode }) {
  return (
    <div className="flex min-h-8 flex-wrap items-center justify-between gap-3 px-1">
      <div className="flex items-center gap-1.5">
        <h3 className="text-sm font-medium tracking-tight">{title}</h3>
        <Tooltip>
          <TooltipTrigger asChild><button type="button" className="text-muted-foreground transition-colors hover:text-foreground" aria-label={help}><CircleHelp className="size-3.5" /></button></TooltipTrigger>
          <TooltipContent className="max-w-80">{help}</TooltipContent>
        </Tooltip>
      </div>
      {children ? <div className="flex flex-wrap items-center gap-1.5">{children}</div> : null}
    </div>
  );
}

function ActionTooltip({ label, children }: { label: string; children: ReactNode }) {
  return (
    <Tooltip>
      <TooltipTrigger asChild>{children}</TooltipTrigger>
      <TooltipContent className="max-w-80">{label}</TooltipContent>
    </Tooltip>
  );
}

function AutomationRow({ controlId, label, description, error, children }: { controlId: string; label: string; description: string; error?: string; children: ReactNode }) {
  return (
    <div className="min-w-0 py-4">
      <div className="grid min-w-0 gap-2.5 sm:grid-cols-[minmax(0,2fr)_minmax(0,1fr)] sm:items-center sm:gap-8">
        <div className="min-w-0">
          <div className="flex min-h-5 items-center">
            <Label htmlFor={controlId} className="text-xs font-medium">{label}</Label>
          </div>
          <p className="mt-1 max-w-xl text-xs leading-5 text-muted-foreground">{description}</p>
          {error ? <p className="mt-1 text-xs text-destructive">{error}</p> : null}
        </div>
        <div className="min-w-0">{children}</div>
      </div>
    </div>
  );
}

function IntervalInput({ id, value, unit, onChange }: { id: string; value: number; unit: string; onChange: (value: number) => void }) {
  return (
    <div className="flex min-w-0">
      <Input id={id} className="min-w-0 rounded-r-none" type="number" min={60} max={86400} value={value} onChange={(event) => onChange(Number(event.target.value))} />
      <div className="flex h-8 w-16 shrink-0 items-center rounded-r-md bg-secondary/55 px-3 text-xs text-foreground">{unit}</div>
    </div>
  );
}

function SourceError({ message }: { message: string }) {
  return (
    <Tooltip>
      <TooltipTrigger asChild><span className="inline-flex shrink-0 cursor-help text-destructive" tabIndex={0} aria-label={message}><CircleAlert className="size-3.5" /></span></TooltipTrigger>
      <TooltipContent className="max-w-80">{message}</TooltipContent>
    </Tooltip>
  );
}

function Control({ label, children }: { label: string; children: ReactNode }) {
  return <div className="space-y-2"><Label className="text-xs font-medium">{label}</Label>{children}</div>;
}

function ToggleControl({ label, checked, onChange }: { label: string; checked: boolean; onChange: (value: boolean) => void }) {
  return <div className="flex min-h-10 items-center justify-between gap-4 rounded-md bg-muted/45 px-3"><Label className="text-xs font-medium">{label}</Label><Switch checked={checked} onCheckedChange={onChange} /></div>;
}

function showError(error: unknown) {
  toast.error(error instanceof Error ? error.message : "Operation failed");
}
