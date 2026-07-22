import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { CircleAlert, CircleHelp, MoreHorizontal, Network, Pencil, Plus, RefreshCw, Shuffle, Trash2, Upload } from "lucide-react";
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
import { Textarea } from "@/components/ui/textarea";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import {
  createEgressSource,
  deleteEgressSource,
  getEgressOperationsConfig,
  importEgressText,
  listEgressNodes,
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
import { formatDateTime } from "@/shared/lib/format";
import { ErrorState, LoadingState, TableLoadingRow } from "@/shared/components/data-state";

type SourceForm = EgressSourceInput & { url: string };
type ImportForm = { name: string; scope: EgressScope; accountCapacity: number; content: string };

const emptySource: SourceForm = {
  name: "", scope: "grok_build", enabled: true, url: "", refreshIntervalSeconds: 900, defaultAccountCapacity: 0,
};
const emptyImport: ImportForm = { name: "", scope: "grok_build", accountCapacity: 0, content: "" };
// Eight probes run concurrently and each can take up to 15 seconds. Keeping a
// request to 32 nodes leaves enough headroom for the admin HTTP timeout.
const egressProbeBatchSize = 32;
const fallbackScopes: EgressScope[] = ["grok_build", "grok_web", "grok_console", "grok_web_asset"];
const fallbackDescriptionKeys: Record<EgressScope, string> = {
  grok_build: "settings.egress.fallbackBuildHelp",
  grok_web: "settings.egress.fallbackWebHelp",
  grok_console: "settings.egress.fallbackConsoleHelp",
  grok_web_asset: "settings.egress.fallbackWebAssetHelp",
};

function defaultFallbacks(): Record<EgressScope, EgressFallbackConfigDTO> {
  return {
    grok_build: { mode: "none" }, grok_web: { mode: "none" },
    grok_console: { mode: "none" }, grok_web_asset: { mode: "none" },
  };
}

const defaultOperationsForm: Omit<EgressOperationsConfigDTO, "updatedAt"> = {
  probeIntervalSeconds: 900, autoAssignEnabled: false, autoBalanceEnabled: false, assignmentIntervalSeconds: 300, fallbacks: defaultFallbacks(),
};

function operationsFormFrom(value?: EgressOperationsConfigDTO): Omit<EgressOperationsConfigDTO, "updatedAt"> {
  if (!value) return { ...defaultOperationsForm, fallbacks: defaultFallbacks() };

  const defaults = defaultFallbacks();
  return {
    probeIntervalSeconds: value.probeIntervalSeconds,
    autoAssignEnabled: value.autoAssignEnabled,
    autoBalanceEnabled: value.autoBalanceEnabled,
    assignmentIntervalSeconds: value.assignmentIntervalSeconds,
    fallbacks: {
      grok_build: { ...defaults.grok_build, ...value.fallbacks.grok_build },
      grok_web: { ...defaults.grok_web, ...value.fallbacks.grok_web },
      grok_console: { ...defaults.grok_console, ...value.fallbacks.grok_console },
      grok_web_asset: { ...defaults.grok_web_asset, ...value.fallbacks.grok_web_asset },
    },
  };
}

async function testAllEgressNodes() {
  const nodes = await listEgressNodes();
  const ids = nodes.items.filter((node) => node.enabled && node.proxyConfigured).map((node) => node.id);
  const result = { requested: 0, healthy: 0, unhealthy: 0 };
  for (let index = 0; index < ids.length; index += egressProbeBatchSize) {
    const batch = await testEgressNodes(ids.slice(index, index + egressProbeBatchSize));
    result.requested += batch.requested;
    result.healthy += batch.healthy;
    result.unhealthy += batch.unhealthy;
  }
  return result;
}

export function EgressOperations({ scopeLabel }: { scopeLabel: (scope: EgressScope) => string }) {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const [sourceEditing, setSourceEditing] = useState<EgressSourceDTO | null | undefined>(undefined);
  const [sourceForm, setSourceForm] = useState<SourceForm>(emptySource);
  const [importOpen, setImportOpen] = useState(false);
  const [importForm, setImportForm] = useState<ImportForm>(emptyImport);
  const [operationsDraft, setOperationsDraft] = useState<Omit<EgressOperationsConfigDTO, "updatedAt"> | null>(null);
  const sourcesQuery = useQuery({ queryKey: ["egress-sources"], queryFn: listEgressSources });
  const operationsQuery = useQuery({ queryKey: ["egress-operations"], queryFn: getEgressOperationsConfig });
  const nodesQuery = useQuery({ queryKey: ["egress-nodes", "fallback-options"], queryFn: () => listEgressNodes() });
  const operationsForm = operationsDraft ?? operationsFormFrom(operationsQuery.data);

  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
    void queryClient.invalidateQueries({ queryKey: ["egress-sources"] });
    void queryClient.invalidateQueries({ queryKey: ["egress-operations"] });
  };
  const saveSource = useMutation({
    mutationFn: () => {
      const input: EgressSourceInput = { ...sourceForm, url: sourceForm.url.trim() || undefined };
      return sourceEditing ? updateEgressSource(sourceEditing.id, input) : createEgressSource(input);
    },
    onSuccess: () => { invalidate(); setSourceEditing(undefined); toast.success(t("settings.egress.sourceSaved")); },
    onError: showError,
  });
  const removeSource = useMutation({
    mutationFn: deleteEgressSource,
    onSuccess: () => { invalidate(); toast.success(t("settings.egress.sourceDeleted")); },
    onError: showError,
  });
  const syncSource = useMutation({
    mutationFn: syncEgressSource,
    onSuccess: (value) => { invalidate(); toast.success(t("settings.egress.sourceSynced", value)); },
    onError: showError,
  });
  const importText = useMutation({
    mutationFn: () => importEgressText(importForm),
    onSuccess: (value) => { invalidate(); setImportOpen(false); toast.success(t("settings.egress.imported", value)); },
    onError: showError,
  });
  const testAll = useMutation({
    mutationFn: testAllEgressNodes,
    onSuccess: (value) => { invalidate(); toast.success(t("settings.egress.tested", value)); },
    onError: showError,
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

  function openSource(value?: EgressSourceDTO) {
    if (!value) {
      setSourceForm(emptySource);
      setSourceEditing(null);
      return;
    }
    setSourceForm({
      name: value.name, scope: value.scope, enabled: value.enabled, url: "", refreshIntervalSeconds: value.refreshIntervalSeconds,
      defaultAccountCapacity: value.defaultAccountCapacity,
    });
    setSourceEditing(value);
  }

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

      <div className="space-y-3">
        <OperationSectionHeader title={t("settings.egress.subscriptions")} help={t("settings.egress.subscriptionsHelp")}>
          <ActionTooltip label={t("settings.egress.importTextHelp")}><Button type="button" size="sm" variant="secondary" onClick={() => { setImportForm(emptyImport); setImportOpen(true); }}><Upload />{t("settings.egress.importText")}</Button></ActionTooltip>
          <ActionTooltip label={t("settings.egress.addSourceHelp")}><Button type="button" size="sm" variant="secondary" onClick={() => openSource()}><Plus />{t("settings.egress.addSource")}</Button></ActionTooltip>
        </OperationSectionHeader>

        {sourcesQuery.isError ? <ErrorState message={sourcesQuery.error.message} onRetry={() => void sourcesQuery.refetch()} /> : (
          <div className="overflow-hidden rounded-md border">
            <Table className="min-w-[640px]">
              <TableHeader><TableRow><TableHead className="min-w-48">{t("settings.egress.source")}</TableHead><TableHead className="w-32 text-center">{t("settings.egress.scope")}</TableHead><TableHead className="min-w-44">{t("settings.egress.lastSync")}</TableHead><TableHead className="w-28 text-center">{t("settings.egress.capacity")}</TableHead><TableActionHead /></TableRow></TableHeader>
              <TableBody>
                {sourcesQuery.isPending ? <TableLoadingRow colSpan={5} /> : null}
                {!sourcesQuery.isPending && (sourcesQuery.data?.items.length ?? 0) === 0 ? <TableRow><TableCell colSpan={5} className="h-24 text-center text-xs text-muted-foreground">{t("settings.egress.noSources")}</TableCell></TableRow> : null}
                {sourcesQuery.data?.items.map((source) => (
                  <TableRow className="group h-12" key={source.id}>
                    <TableCell><div className="flex min-w-0 items-center gap-2"><span className={source.enabled ? "size-1.5 shrink-0 rounded-full bg-emerald-500" : "size-1.5 shrink-0 rounded-full bg-muted-foreground/35"} /><span className="truncate text-xs font-medium">{source.name}</span>{source.lastSyncError ? <SourceError message={source.lastSyncError} /> : null}</div></TableCell>
                    <TableCell className="text-center"><Badge variant="secondary" className="text-[10px]">{scopeLabel(source.scope)}</Badge></TableCell>
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
                ))}
              </TableBody>
            </Table>
          </div>
        )}
      </div>

      <Dialog open={sourceEditing !== undefined} onOpenChange={(open) => { if (!open) setSourceEditing(undefined); }}>
        <DialogContent className="max-h-[calc(100svh-2rem)] overflow-y-auto sm:max-w-[520px]">
          <DialogHeader className="pr-8"><DialogTitle>{sourceEditing ? t("settings.egress.editSource") : t("settings.egress.addSource")}</DialogTitle><DialogDescription>{t("settings.egress.sourceDialogDescription")}</DialogDescription></DialogHeader>
          <form className="space-y-3.5" onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); saveSource.mutate(); }}>
            <ToggleControl label={t("settings.egress.enabled")} checked={sourceForm.enabled} onChange={(enabled) => setSourceForm({ ...sourceForm, enabled })} />
            <Control label={t("settings.egress.name")}><Input value={sourceForm.name} onChange={(event) => setSourceForm({ ...sourceForm, name: event.target.value })} /></Control>
            <Control label={t("settings.egress.scope")}><ScopeSelect value={sourceForm.scope} onChange={(scope) => setSourceForm({ ...sourceForm, scope })} scopeLabel={scopeLabel} /></Control>
            <Control label={t("settings.egress.subscriptionURL")}><Input type="password" autoComplete="new-password" placeholder={sourceEditing?.urlConfigured ? t("settings.egress.keepConfigured") : "https://..."} value={sourceForm.url} onChange={(event) => setSourceForm({ ...sourceForm, url: event.target.value })} /></Control>
            <div className="grid gap-3 sm:grid-cols-2">
              <Control label={t("settings.egress.refreshInterval")}><Input type="number" min={60} max={86400} value={sourceForm.refreshIntervalSeconds} onChange={(event) => setSourceForm({ ...sourceForm, refreshIntervalSeconds: Number(event.target.value) })} /></Control>
              <Control label={t("settings.egress.capacity")}><Input type="number" min={0} max={100000} placeholder={t("settings.egress.unlimited")} value={sourceForm.defaultAccountCapacity || ""} onChange={(event) => setSourceForm({ ...sourceForm, defaultAccountCapacity: Number(event.target.value) })} /></Control>
            </div>
            <DialogFooter><Button type="button" size="sm" variant="secondary" onClick={() => setSourceEditing(undefined)}>{t("common.cancel")}</Button><Button type="submit" size="sm" disabled={!sourceForm.name.trim() || (!sourceEditing && !sourceForm.url.trim()) || saveSource.isPending}>{saveSource.isPending ? <Spinner /> : null}{t("common.save")}</Button></DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      <Dialog open={importOpen} onOpenChange={setImportOpen}>
        <DialogContent className="max-h-[calc(100svh-2rem)] overflow-y-auto sm:max-w-[620px]">
          <DialogHeader className="pr-8"><DialogTitle>{t("settings.egress.importText")}</DialogTitle><DialogDescription>{t("settings.egress.importDialogDescription")}</DialogDescription></DialogHeader>
          <form className="space-y-3.5" onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); importText.mutate(); }}>
            <div className="grid gap-3 sm:grid-cols-2"><Control label={t("settings.egress.name")}><Input value={importForm.name} onChange={(event) => setImportForm({ ...importForm, name: event.target.value })} /></Control><Control label={t("settings.egress.scope")}><ScopeSelect value={importForm.scope} onChange={(scope) => setImportForm({ ...importForm, scope })} scopeLabel={scopeLabel} /></Control></div>
            <Control label={t("settings.egress.capacity")}><Input type="number" min={0} max={100000} placeholder={t("settings.egress.unlimited")} value={importForm.accountCapacity || ""} onChange={(event) => setImportForm({ ...importForm, accountCapacity: Number(event.target.value) })} /></Control>
            <Control label={t("settings.egress.proxyList")}><Textarea className="min-h-52 font-mono text-xs" value={importForm.content} onChange={(event) => setImportForm({ ...importForm, content: event.target.value })} /></Control>
            <DialogFooter><Button type="button" size="sm" variant="secondary" onClick={() => setImportOpen(false)}>{t("common.cancel")}</Button><Button type="submit" size="sm" disabled={!importForm.name.trim() || !importForm.content.trim() || importText.isPending}>{importText.isPending ? <Spinner /> : null}{t("settings.egress.importText")}</Button></DialogFooter>
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
  return nodeScope === requestScope || ((requestScope === "grok_console" || requestScope === "grok_web_asset") && nodeScope === "grok_web");
}

function ScopeSelect({ value, onChange, scopeLabel }: { value: EgressScope; onChange: (value: EgressScope) => void; scopeLabel: (scope: EgressScope) => string }) {
  return <Select value={value} onValueChange={(next) => onChange(next as EgressScope)}><SelectTrigger><SelectValue /></SelectTrigger><SelectContent>{(["grok_build", "grok_web", "grok_console", "grok_web_asset"] as EgressScope[]).map((scope) => <SelectItem key={scope} value={scope}>{scopeLabel(scope)}</SelectItem>)}</SelectContent></Select>;
}

function OperationSectionHeader({ title, help, children }: { title: string; help: string; children: ReactNode }) {
  return (
    <div className="flex min-h-8 flex-wrap items-center justify-between gap-3 px-1">
      <div className="flex items-center gap-1.5">
        <h3 className="text-sm font-medium tracking-tight">{title}</h3>
        <Tooltip>
          <TooltipTrigger asChild><button type="button" className="text-muted-foreground transition-colors hover:text-foreground" aria-label={help}><CircleHelp className="size-3.5" /></button></TooltipTrigger>
          <TooltipContent className="max-w-80">{help}</TooltipContent>
        </Tooltip>
      </div>
      <div className="flex flex-wrap items-center gap-1.5">{children}</div>
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

function AutomationRow({ controlId, label, description, children }: { controlId: string; label: string; description: string; children: ReactNode }) {
  return (
    <div className="min-w-0 py-4">
      <div className="grid min-w-0 gap-2.5 sm:grid-cols-[minmax(0,2fr)_minmax(0,1fr)] sm:items-center sm:gap-8">
        <div className="min-w-0">
          <div className="flex min-h-5 items-center">
            <Label htmlFor={controlId} className="text-xs font-medium">{label}</Label>
          </div>
          <p className="mt-1 max-w-xl text-xs leading-5 text-muted-foreground">{description}</p>
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
