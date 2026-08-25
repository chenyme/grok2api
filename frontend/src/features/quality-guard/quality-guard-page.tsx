import { zodResolver } from "@hookform/resolvers/zod";
import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Activity, AlertTriangle, BarChart3, Bot, Coins, Eye, Gauge, MoreHorizontal, Pencil, Plus, Power, PowerOff, RefreshCw, RotateCcw, RotateCw, Search, Shield, ShieldCheck, ShieldX, TimerReset, Trash2, Zap } from "lucide-react";
import { memo, useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { useForm, useWatch } from "react-hook-form";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import { z } from "zod";

import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { DegradeAccountsPanel } from "@/features/quality-guard/degrade-accounts-panel";
import { ProbeProfilesPanel } from "@/features/quality-guard/probe-profiles-panel";
import { getQualityGuardStatus, runQualityTest, updateQualityGuardPolicy, type QualityGuardEvent, type QualityGuardNodeState, type QualityGuardPolicy, type QualityGuardStatistics, type QualityGuardStatus, type QualityTestResult } from "@/features/quality-guard/quality-guard-api";
import { createEgressNode, deleteEgressNodes, listEgressNodes, updateEgressNode, updateEgressNodesEnabled, type EgressNodeDTO, type EgressNodeInput } from "@/features/settings/settings-api";
import { ErrorState, TableLoadingRow } from "@/shared/components/data-state";
import { DataTableFilters } from "@/shared/components/data-table-filters";
import { PageHeader } from "@/shared/components/page-header";
import { Pagination } from "@/shared/components/pagination";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { cn } from "@/shared/lib/cn";

const NODE_ACTION_TOAST_ID = "quality-guard-node-action";
const QUALITY_GUARD_PAGE_SIZES = [20, 50, 100] as const;
type QualityGuardTab = "nodes" | "profiles" | "accounts";

export function QualityGuardPage() {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const [manualResults, setManualResults] = useState<Record<string, QualityGuardNodeState>>({});
  const [policyOpen, setPolicyOpen] = useState(false);
  const [editingNode, setEditingNode] = useState<EgressNodeDTO | null | undefined>(undefined);
  const [nodeForm, setNodeForm] = useState<EgressNodeInput>(emptyNodeInput());
  const [deletingNodes, setDeletingNodes] = useState<EgressNodeDTO[]>([]);
  const [selectedNodeIDs, setSelectedNodeIDs] = useState<Set<string>>(() => new Set());
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [search, setSearch] = useState("");
  const [enabledFilter, setEnabledFilter] = useState("");
  const [probeFilter, setProbeFilter] = useState("");
  const [assignmentFilter, setAssignmentFilter] = useState("");
  const [activeTab, setActiveTab] = useState<QualityGuardTab>("nodes");
  const nodesTabActive = activeTab === "nodes";
  const debouncedSearch = useDebouncedValue(search);
  const nodesQuery = useQuery({
    queryKey: ["quality-guard-egress-nodes", "page", page, pageSize, debouncedSearch, enabledFilter, probeFilter, assignmentFilter],
    queryFn: () => listEgressNodes({
      page, pageSize, search: debouncedSearch, scope: "grok_build", enabled: enabledFilter,
      probe: probeFilter, assignment: assignmentFilter,
    }),
    placeholderData: keepPreviousData,
    enabled: nodesTabActive,
    refetchInterval: nodesTabActive ? 15_000 : false,
    staleTime: 10_000,
  });
  const nodeIDs = nodesTabActive ? nodesQuery.data?.items.map((node) => node.id) ?? [] : [];
  const statusQuery = useQuery({
    queryKey: ["quality-guard", "nodes", nodeIDs],
    queryFn: () => getQualityGuardStatus(nodeIDs),
    placeholderData: keepPreviousData,
    refetchInterval: 5_000,
  });
  const statusRef = useRef<QualityGuardStatus | undefined>(statusQuery.data);
  useEffect(() => {
    statusRef.current = statusQuery.data;
  }, [statusQuery.data]);
  const nodeCountsQuery = useQuery({
    queryKey: ["quality-guard-egress-nodes", "counts"],
    queryFn: async () => {
      const [allNodes, enabledNodes] = await Promise.all([
        listEgressNodes({ page: 1, pageSize: 1, scope: "grok_build" }),
        listEgressNodes({ page: 1, pageSize: 1, scope: "grok_build", enabled: "enabled" }),
      ]);
      return { total: allNodes.total, enabled: enabledNodes.total };
    },
    enabled: nodesTabActive,
    refetchInterval: nodesTabActive ? 30_000 : false,
    staleTime: 20_000,
  });
  const testMutation = useMutation({
    mutationFn: ({ nodeId, status }: { nodeId: string; status: QualityGuardStatus }) => runQualityTest(nodeId, status),
    onMutate: () => toast.loading(t("qualityGuard.testing"), { id: NODE_ACTION_TOAST_ID }),
    onSuccess: (result, variables) => {
      setManualResults((current) => ({ ...current, [variables.nodeId]: qualityTestState(result, variables.status) }));
      toast.success(t("qualityGuard.testComplete", { speed: formatTPS(result.outputTokensPerSecond) }), { id: NODE_ACTION_TOAST_ID });
    },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("qualityGuard.testFailed"), { id: NODE_ACTION_TOAST_ID }),
  });
  const { mutate: mutateTestNode } = testMutation;

  const refreshNodeQueries = () => Promise.all([
    queryClient.invalidateQueries({ queryKey: ["quality-guard"] }),
    queryClient.invalidateQueries({ queryKey: ["quality-guard-egress-nodes"] }),
    queryClient.invalidateQueries({ queryKey: ["egress-nodes"] }),
  ]);
  const saveNodeMutation = useMutation({
    mutationFn: () => {
      const input: EgressNodeInput = {
        ...nodeForm,
        name: nodeForm.name.trim(),
        scope: "grok_build",
        proxyURL: nodeForm.proxyURL?.trim() || undefined,
        userAgent: "",
        cloudflareCookies: undefined,
      };
      return editingNode ? updateEgressNode(editingNode.id, input) : createEgressNode(input);
    },
    onSuccess: () => {
      setEditingNode(undefined);
      void refreshNodeQueries();
      toast.success(t("settings.egress.saved"), { id: NODE_ACTION_TOAST_ID });
    },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("settings.egress.operationFailed"), { id: NODE_ACTION_TOAST_ID }),
  });
  const toggleNodeMutation = useMutation({
    mutationFn: ({ node, enabled }: { node: EgressNodeDTO; enabled: boolean }) => updateEgressNodesEnabled([node.id], enabled),
    onSuccess: (_, { enabled }) => {
      void refreshNodeQueries();
      toast.success(t(enabled ? "qualityGuard.nodeEnabled" : "qualityGuard.nodeDisabled"), { id: NODE_ACTION_TOAST_ID });
    },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("settings.egress.operationFailed"), { id: NODE_ACTION_TOAST_ID }),
  });
  const { mutate: mutateToggleNode } = toggleNodeMutation;
  const batchToggleMutation = useMutation({
    mutationFn: ({ nodes, enabled }: { nodes: EgressNodeDTO[]; enabled: boolean }) => updateEgressNodesEnabled(nodes.map((node) => node.id), enabled),
    onSuccess: (_, { enabled }) => {
      setSelectedNodeIDs(new Set());
      void refreshNodeQueries();
      toast.success(t(enabled ? "qualityGuard.nodesEnabled" : "qualityGuard.nodesDisabled"), { id: NODE_ACTION_TOAST_ID });
    },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("settings.egress.operationFailed"), { id: NODE_ACTION_TOAST_ID }),
  });
  const deleteNodeMutation = useMutation({
    mutationFn: (nodes: EgressNodeDTO[]) => deleteEgressNodes(nodes.map((node) => node.id)),
    onSuccess: () => {
      setDeletingNodes([]);
      setSelectedNodeIDs(new Set());
      void refreshNodeQueries();
      toast.success(t("settings.egress.deleted"), { id: NODE_ACTION_TOAST_ID });
    },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("settings.egress.operationFailed"), { id: NODE_ACTION_TOAST_ID }),
  });

  const openCreateNode = () => {
    setNodeForm(emptyNodeInput());
    setEditingNode(null);
  };
  const openEditNode = useCallback((node: EgressNodeDTO) => {
    setNodeForm({
      name: node.name,
      scope: "grok_build",
      enabled: node.enabled,
      proxyPool: node.proxyPool,
      accountCapacity: node.accountCapacity,
      proxyURL: "",
      userAgent: "",
      cloudflareCookies: "",
    });
    setEditingNode(node);
  }, []);
  const openDeleteNode = useCallback((node: EgressNodeDTO) => setDeletingNodes([node]), []);
  const testNode = useCallback((nodeId: string) => {
    const currentStatus = statusRef.current;
    if (currentStatus?.config) mutateTestNode({ nodeId, status: currentStatus });
  }, [mutateTestNode]);
  const toggleNode = useCallback((node: EgressNodeDTO, enabled: boolean) => {
    mutateToggleNode({ node, enabled });
  }, [mutateToggleNode]);
  const toggleSelectedNode = useCallback((nodeId: string, checked: boolean) => setSelectedNodeIDs((current) => {
    const next = new Set(current);
    if (checked) next.add(nodeId);
    else next.delete(nodeId);
    return next;
  }), []);

  const refresh = () => void (nodesTabActive
    ? Promise.all([statusQuery.refetch(), nodesQuery.refetch(), nodeCountsQuery.refetch()])
    : statusQuery.refetch());
  if (nodesTabActive && nodesQuery.isError && !nodesQuery.data) return <ErrorState message={nodesQuery.error.message} onRetry={refresh} />;
  if (statusQuery.isError && !statusQuery.data) return <ErrorState message={statusQuery.error.message} onRetry={refresh} />;

  const status = statusQuery.data;
  const nodes = nodesQuery.data?.items ?? [];
  const protectedNodeIDs = new Set(status?.protectedNodeIds ?? []);
  const selectableNodes = nodes.filter((node) => !protectedNodeIDs.has(node.id));
  const selectedNodes = selectableNodes.filter((node) => selectedNodeIDs.has(node.id));
  const allNodesSelected = selectableNodes.length > 0 && selectedNodes.length === selectableNodes.length;
  const toggleAllNodes = (checked: boolean) => setSelectedNodeIDs(checked ? new Set(selectableNodes.map((node) => node.id)) : new Set());
  const fresh = isFresh(status);
  const guardedNodes = status?.nodes ?? {};
  const quarantined = status?.nodeSummary?.quarantined ?? Object.values(guardedNodes).filter((node) => node.disabled_by_guard).length;
  const quarantinedLeases = status?.nodeSummary?.quarantinedLeases ?? Object.values(guardedNodes).reduce((total, node) => total + (node.quarantined_lease_count ?? 0), 0);
  const totalNodes = nodeCountsQuery.data?.total ?? nodesQuery.data?.total ?? nodes.length;
  const enabledNodes = nodeCountsQuery.data?.enabled;
  const hasNodeFilters = Boolean(debouncedSearch || enabledFilter || probeFilter || assignmentFilter);
  const resetNodePage = () => {
    setPage(1);
    setSelectedNodeIDs(new Set());
  };
  const changeNodePage = (value: number) => {
    setPage(value);
    setSelectedNodeIDs(new Set());
  };
  const refreshing = statusQuery.isFetching || (nodesTabActive && (nodesQuery.isFetching || nodeCountsQuery.isFetching));
  const testingNodeID = testMutation.isPending ? testMutation.variables?.nodeId : undefined;
  const togglingNodeID = toggleNodeMutation.isPending ? toggleNodeMutation.variables?.node.id : undefined;

  return (
    <div className="space-y-6">
      <PageHeader
        title={t("qualityGuard.title")}
        description={t("qualityGuard.description")}
        actions={(
          <Button variant="secondary" size="sm" onClick={refresh} disabled={refreshing}>
            <RefreshCw className={cn(refreshing && "animate-spin")} />
            {t("common.refresh")}
          </Button>
        )}
      />

      <Tabs value={activeTab} onValueChange={(value) => setActiveTab(value as QualityGuardTab)}>
        <TabsList>
          <TabsTrigger value="nodes">{t("qualityGuard.nodesTab")}</TabsTrigger>
          <TabsTrigger value="profiles">{t("qualityGuard.profilesTab")}</TabsTrigger>
          <TabsTrigger value="accounts">{t("qualityGuard.degrade.tab")}</TabsTrigger>
        </TabsList>
        <TabsContent value="profiles" className="mt-6">
          <ProbeProfilesPanel />
        </TabsContent>
        <TabsContent value="accounts" className="mt-6">
          <DegradeAccountsPanel
            softTPS={status?.config?.soft_tps}
            hardTPS={status?.config?.hard_tps}
            failClosed={status?.config?.fail_closed}
            minGenMs={status?.config?.min_generation_ms}
          />
        </TabsContent>
        <TabsContent value="nodes" className="mt-6 space-y-6">
      {!status?.available ? <UnavailableState /> : (
        <>
          <section className="grid overflow-hidden rounded-lg bg-card sm:grid-cols-2 xl:grid-cols-4" aria-label={t("qualityGuard.overview")}>
            <Metric icon={fresh ? ShieldCheck : ShieldX} label={t("qualityGuard.serviceStatus")} value={fresh ? t("qualityGuard.running") : t("qualityGuard.stale")} tone={fresh ? "good" : "bad"} />
            <Metric icon={Activity} label={t("qualityGuard.mode")} value={t(`qualityGuard.modes.${status.config?.mode ?? "hybrid"}`)} />
            <Metric icon={Gauge} label={t("qualityGuard.availableNodes")} value={`${enabledNodes ?? "-"} / ${totalNodes}`} />
            <Metric icon={TimerReset} label={t("qualityGuard.quarantinedTargets")} value={String(quarantined + quarantinedLeases)} tone={quarantined || quarantinedLeases ? "bad" : "good"} />
          </section>

          {status.statistics ? <StatisticsPanel statistics={status.statistics} locale={i18n.language} /> : null}

          <section className="overflow-hidden rounded-lg bg-card" aria-labelledby="guard-nodes-title">
            <div className="flex flex-col gap-2 border-b px-4 py-4 sm:flex-row sm:items-center sm:justify-between sm:px-5">
              <div>
                <h2 id="guard-nodes-title" className="text-sm font-medium">{t("qualityGuard.nodes")}</h2>
                <p className="mt-1 text-xs text-muted-foreground">{t("qualityGuard.nodesHelp")}</p>
              </div>
              <div className="flex flex-wrap items-center gap-1.5 self-start sm:self-auto sm:justify-end">
                <span className="mr-1 hidden text-xs text-muted-foreground lg:inline">{t("qualityGuard.updatedAt", { time: formatTime(status.updatedAt, i18n.language) })}</span>
                {selectedNodes.length > 0 ? <>
                  <span className="mr-1 text-xs text-muted-foreground">{t("common.selectedCount", { count: selectedNodes.length })}</span>
                  <Button type="button" variant="secondary" size="sm" disabled={batchToggleMutation.isPending || selectedNodes.every((node) => node.enabled)} onClick={() => batchToggleMutation.mutate({ nodes: selectedNodes, enabled: true })}><Power />{t("common.enable")}</Button>
                  <Button type="button" variant="secondary" size="sm" disabled={batchToggleMutation.isPending || selectedNodes.every((node) => !node.enabled)} onClick={() => batchToggleMutation.mutate({ nodes: selectedNodes, enabled: false })}><PowerOff />{t("common.disable")}</Button>
                  <Button type="button" variant="secondary" size="sm" className="bg-destructive/10 text-destructive hover:bg-destructive/15 hover:text-destructive" disabled={deleteNodeMutation.isPending} onClick={() => setDeletingNodes(selectedNodes)}><Trash2 />{t("common.delete")}</Button>
                </> : null}
                <Button type="button" variant="ghost" size="icon" className="size-8" onClick={() => void nodesQuery.refetch()} disabled={nodesQuery.isFetching} aria-label={t("qualityGuard.refreshNodes")} title={t("qualityGuard.refreshNodes")}><RefreshCw className={cn("size-4", nodesQuery.isFetching && "animate-spin")} /></Button>
                <Button type="button" size="sm" onClick={openCreateNode}><Plus />{t("settings.egress.add")}</Button>
              </div>
            </div>
            <div className="flex flex-col gap-2 border-b px-4 py-3 sm:flex-row sm:items-center sm:px-5">
              <div className="relative min-w-0 flex-1 sm:max-w-72">
                <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                <Input className="h-8 pl-9 text-xs" value={search} onChange={(event) => { setSearch(event.target.value); resetNodePage(); }} placeholder={t("settings.egress.search")} aria-label={t("settings.egress.search")} />
              </div>
              <DataTableFilters filters={[
                { id: "enabled", label: t("settings.egress.enabled"), value: enabledFilter, onChange: (value) => { setEnabledFilter(value); resetNodePage(); }, options: [
                  { value: "enabled", label: t("common.enable") },
                  { value: "disabled", label: t("common.disable") },
                ] },
                { id: "probe", label: t("settings.egress.probe"), value: probeFilter, onChange: (value) => { setProbeFilter(value); resetNodePage(); }, options: [
                  { value: "healthy", label: t("settings.egress.healthy") },
                  { value: "unhealthy", label: t("settings.egress.unhealthy") },
                  { value: "unknown", label: t("settings.egress.notTested") },
                ] },
                { id: "assignment", label: t("settings.egress.accounts"), value: assignmentFilter, onChange: (value) => { setAssignmentFilter(value); resetNodePage(); }, options: [
                  { value: "bound", label: t("settings.egress.assigned") },
                  { value: "unbound", label: t("settings.egress.unassigned") },
                ] },
              ]} />
            </div>
            <div className="overflow-x-auto">
              <Table className="min-w-[1040px]">
                <TableHeader><TableRow>
                  <TableHead className="w-10 px-3"><Checkbox checked={allNodesSelected ? true : selectedNodes.length > 0 ? "indeterminate" : false} disabled={selectableNodes.length === 0} onCheckedChange={(checked) => toggleAllNodes(checked === true)} aria-label={t("common.selectPage")} /></TableHead>
                  <TableHead>{t("qualityGuard.node")}</TableHead><TableHead>{t("qualityGuard.state")}</TableHead><TableHead className="text-right">{t("settings.egress.accounts")}</TableHead>
                  <TableHead className="text-right">{t("qualityGuard.outputTPS")}</TableHead><TableHead className="text-right">{t("qualityGuard.firstToken")}</TableHead>
                  <TableHead>{t("qualityGuard.source")}</TableHead><TableHead>{t("qualityGuard.strikes")}</TableHead>
                  <TableHead>{t("qualityGuard.lastObserved")}</TableHead><TableHead className="w-48 text-right">{t("common.actions")}</TableHead>
                </TableRow></TableHeader>
                {nodesQuery.isPending ? <TableBody><TableLoadingRow colSpan={10} /></TableBody> : null}
                {!nodesQuery.isPending && nodes.length === 0 ? <TableBody><TableRow><TableCell colSpan={10} className="h-24 text-center text-xs text-muted-foreground">{hasNodeFilters ? t("settings.egress.noMatches") : t("common.noData")}</TableCell></TableRow></TableBody> : null}
                {!nodesQuery.isPending && nodes.length > 0 ? <TableBody>
                  {nodes.map((node) => <NodeRow key={node.id} node={node} protectedNode={protectedNodeIDs.has(node.id)} selected={selectedNodeIDs.has(node.id)} onSelect={toggleSelectedNode} state={manualResults[node.id] ?? guardedNodes[node.id]} locale={i18n.language} testEnabled={Boolean(status.config)} testing={testingNodeID === node.id} toggling={togglingNodeID === node.id} onTest={testNode} onToggle={toggleNode} onEdit={openEditNode} onDelete={openDeleteNode} />)}
                </TableBody> : null}
              </Table>
            </div>
            {nodesQuery.data && nodesQuery.data.total > 0 ? <div className="border-t px-4 py-3 sm:px-5"><Pagination page={nodesQuery.data.page} pageSize={nodesQuery.data.pageSize} total={nodesQuery.data.total} pageSizeOptions={QUALITY_GUARD_PAGE_SIZES} onPageChange={changeNodePage} onPageSizeChange={(value) => { setPageSize(value); resetNodePage(); }} /></div> : null}
          </section>

          <div className="grid gap-3 xl:grid-cols-[minmax(0,3fr)_minmax(300px,2fr)]">
            <EventList events={status.recentEvents ?? []} locale={i18n.language} />
            <Policy status={status} onEdit={() => setPolicyOpen(true)} />
          </div>
          {policyOpen ? <PolicyEditor open onOpenChange={setPolicyOpen} status={status} /> : null}
          <NodeEditor open={editingNode !== undefined} editingNode={editingNode} form={nodeForm} onFormChange={setNodeForm} onOpenChange={(open) => { if (!open && !saveNodeMutation.isPending) setEditingNode(undefined); }} onSave={() => saveNodeMutation.mutate()} saving={saveNodeMutation.isPending} />
          <AlertDialog open={deletingNodes.length > 0} onOpenChange={(open) => { if (!open && !deleteNodeMutation.isPending) setDeletingNodes([]); }}>
            <AlertDialogContent>
              <AlertDialogHeader>
                <AlertDialogTitle>{deletingNodes.length > 1 ? t("qualityGuard.deleteNodesTitle", { count: deletingNodes.length }) : t("qualityGuard.deleteNodeTitle")}</AlertDialogTitle>
                <AlertDialogDescription>{deletingNodes.length > 1 ? t("qualityGuard.deleteNodesDescription", { count: deletingNodes.length }) : t("qualityGuard.deleteNodeDescription", { name: deletingNodes[0]?.name ?? "" })}</AlertDialogDescription>
              </AlertDialogHeader>
              <AlertDialogFooter>
                <AlertDialogCancel disabled={deleteNodeMutation.isPending}>{t("common.cancel")}</AlertDialogCancel>
                <AlertDialogAction className="bg-destructive text-white hover:bg-destructive/90" disabled={deleteNodeMutation.isPending || deletingNodes.length === 0} onClick={(event) => { event.preventDefault(); if (deletingNodes.length > 0) deleteNodeMutation.mutate(deletingNodes); }}>{deleteNodeMutation.isPending ? <Spinner /> : null}{t("common.delete")}</AlertDialogAction>
              </AlertDialogFooter>
            </AlertDialogContent>
          </AlertDialog>
        </>
      )}
        </TabsContent>
      </Tabs>
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
    { icon: Coins, label: t("qualityGuard.statisticsTokens"), value: formatCount(statistics.active.output_tokens, locale), detail: t("qualityGuard.statisticsTokensHelp") },
    { icon: AlertTriangle, label: t("qualityGuard.statisticsAnomalies"), value: formatCount(anomalies, locale), detail: t("qualityGuard.statisticsAnomalyDetail", { soft: formatCount(statistics.active.soft + statistics.passive.soft, locale), hard: formatCount(statistics.active.hard + statistics.passive.hard, locale) }) },
    { icon: Shield, label: t("qualityGuard.statisticsQuarantines"), value: formatCount(statistics.actions.quarantined, locale), detail: t("qualityGuard.statisticsSuppressedActionDetail", { restored: formatCount(statistics.actions.restored, locale), suppressed: formatCount(statistics.actions.suppressed, locale) }) },
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

const NodeRow = memo(function NodeRow({ node, protectedNode, selected, onSelect, state, locale, testEnabled, testing, toggling, onTest, onToggle, onEdit, onDelete }: { node: EgressNodeDTO; protectedNode: boolean; selected: boolean; onSelect: (nodeId: string, checked: boolean) => void; state?: QualityGuardNodeState; locale: string; testEnabled: boolean; testing: boolean; toggling: boolean; onTest: (nodeId: string) => void; onToggle: (node: EgressNodeDTO, enabled: boolean) => void; onEdit: (node: EgressNodeDTO) => void; onDelete: (node: EgressNodeDTO) => void }) {
  const { t } = useTranslation();
  const classification = state?.last_classification || "unknown";
  return <TableRow>
    <TableCell className="px-3"><Checkbox checked={selected} disabled={protectedNode} onCheckedChange={(checked) => onSelect(node.id, checked === true)} aria-label={t("common.selectItem", { name: node.name })} /></TableCell>
    <TableCell><div className="font-medium">{node.name}</div><div className="mt-0.5 text-[11px] text-muted-foreground">ID {node.id}</div></TableCell>
    <TableCell><StateBadge node={node} state={state} protectedNode={protectedNode} /></TableCell>
    <TableCell className="text-right text-xs tabular-nums"><span className="font-medium">{node.assignedAccountCount}</span>{node.accountCapacity > 0 ? <span className="text-muted-foreground"> / {node.accountCapacity}</span> : null}</TableCell>
    <TableCell className={cn("text-right font-mono text-xs tabular-nums", classification === "hard" && "font-medium text-destructive", classification === "soft" && "text-amber-600 dark:text-amber-400")}>{state?.last_observed_at ? formatTPS(state.last_output_tps) : "-"}</TableCell>
    <TableCell className="text-right font-mono text-xs tabular-nums">{state?.last_first_token_ms ? `${state.last_first_token_ms} ms` : "-"}</TableCell>
    <TableCell className="text-xs text-muted-foreground">{state?.last_source ? t(`qualityGuard.sources.${state.last_source}`) : "-"}</TableCell>
    <TableCell className="text-xs tabular-nums">{state ? `${state.passive_soft_strikes} / ${state.active_soft_strikes} / ${state.error_strikes}` : "-"}</TableCell>
    <TableCell className="text-xs text-muted-foreground">{formatTime(state?.last_observed_at, locale)}</TableCell>
    <TableCell className="text-right"><div className="flex items-center justify-end gap-1">
      <Switch checked={node.enabled} disabled={toggling || protectedNode} onCheckedChange={(enabled) => onToggle(node, enabled)} aria-label={t(node.enabled ? "qualityGuard.disableNode" : "qualityGuard.enableNode", { name: node.name })} />
      <Button variant="ghost" size="sm" disabled={testing || !testEnabled || (!node.enabled && !state?.disabled_by_guard)} onClick={() => onTest(node.id)}><RotateCw className={cn(testing && "animate-spin")} />{t("qualityGuard.test")}</Button>
      <DropdownMenu>
        <DropdownMenuTrigger asChild><Button type="button" variant="ghost" size="icon" className="size-8" aria-label={t("common.actions")}><MoreHorizontal /></Button></DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuItem onClick={() => onEdit(node)}><Pencil />{t("common.edit")}</DropdownMenuItem>
          <DropdownMenuItem disabled={toggling || protectedNode} onClick={() => onToggle(node, !node.enabled)}>{node.enabled ? <PowerOff /> : <Power />}{t(node.enabled ? "common.disable" : "common.enable")}</DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem disabled={protectedNode} className="text-destructive focus:text-destructive" onClick={() => onDelete(node)}><Trash2 />{t("common.delete")}</DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    </div></TableCell>
  </TableRow>;
});

function NodeEditor({ open, editingNode, form, onFormChange, onOpenChange, onSave, saving }: { open: boolean; editingNode: EgressNodeDTO | null | undefined; form: EgressNodeInput; onFormChange: (form: EgressNodeInput) => void; onOpenChange: (open: boolean) => void; onSave: () => void; saving: boolean }) {
  const { t } = useTranslation();
  const proxyConfigured = Boolean(editingNode?.proxyConfigured || form.proxyURL?.trim());
  return <Dialog open={open} onOpenChange={onOpenChange}>
    <DialogContent className="max-h-[calc(100svh-2rem)] overflow-y-auto sm:max-w-[520px]">
      <DialogHeader className="pr-8">
        <DialogTitle>{editingNode ? t("settings.egress.editTitle") : t("settings.egress.addTitle")}</DialogTitle>
        <DialogDescription>{t("qualityGuard.nodeEditorDescription")}</DialogDescription>
      </DialogHeader>
      <form className="space-y-3.5" onSubmit={(event) => { event.preventDefault(); onSave(); }}>
        <div className="flex items-center justify-between gap-4 rounded-md bg-muted/45 px-3 py-2.5">
          <Label htmlFor="quality-node-enabled">{t("settings.egress.enabled")}</Label>
          <Switch id="quality-node-enabled" checked={form.enabled} onCheckedChange={(enabled) => onFormChange({ ...form, enabled })} />
        </div>
        <NodeField label={t("settings.egress.name")} controlId="quality-node-name">
          <Input id="quality-node-name" autoFocus value={form.name} onChange={(event) => onFormChange({ ...form, name: event.target.value })} />
        </NodeField>
        <NodeField label={t("settings.egress.scope")} controlId="quality-node-scope">
          <div id="quality-node-scope" className="flex h-9 items-center rounded-md border bg-muted/30 px-3 text-sm text-muted-foreground">{t("settings.egress.scopeBuild")}</div>
        </NodeField>
        <NodeField label={t("settings.egress.capacity")} controlId="quality-node-capacity" help={t("qualityGuard.nodeCapacityHelp")}>
          <Input id="quality-node-capacity" type="number" min={0} max={100000} placeholder={t("settings.egress.unlimited")} value={form.accountCapacity || ""} onChange={(event) => onFormChange({ ...form, accountCapacity: Number(event.target.value) })} />
        </NodeField>
        <NodeField label={t("settings.egress.proxyURL")} controlId="quality-node-proxy" help={t("settings.egress.proxyProtocols")}>
          <Input id="quality-node-proxy" type="password" autoComplete="new-password" placeholder={editingNode?.proxyConfigured ? t("settings.egress.keepConfigured") : "socks5h://user:pass@host:port"} value={form.proxyURL ?? ""} onChange={(event) => {
            const proxyURL = event.target.value;
            onFormChange({ ...form, proxyURL, proxyPool: editingNode?.proxyConfigured || proxyURL.trim() ? form.proxyPool : false });
          }} />
        </NodeField>
        <div className="flex items-start justify-between gap-4 rounded-md bg-muted/45 px-3 py-2.5">
          <div className="space-y-1">
            <Label htmlFor="quality-node-proxy-pool">{t("settings.egress.proxyPool")}</Label>
            <p className="max-w-[390px] text-xs leading-5 text-muted-foreground">{t("settings.egress.proxyPoolHelp")}</p>
          </div>
          <Switch id="quality-node-proxy-pool" className="mt-0.5" checked={form.proxyPool} disabled={!proxyConfigured} onCheckedChange={(proxyPool) => onFormChange({ ...form, proxyPool })} />
        </div>
        <DialogFooter>
          <Button type="button" variant="secondary" size="sm" disabled={saving} onClick={() => onOpenChange(false)}>{t("common.cancel")}</Button>
          <Button type="submit" size="sm" disabled={!form.name.trim() || saving}>{saving ? <Spinner /> : null}{t("common.save")}</Button>
        </DialogFooter>
      </form>
    </DialogContent>
  </Dialog>;
}

function NodeField({ label, controlId, help, children }: { label: string; controlId: string; help?: string; children: ReactNode }) {
  return <div className="space-y-2">
    <Label htmlFor={controlId}>{label}</Label>
    {children}
    {help ? <p className="whitespace-pre-line text-xs leading-5 text-muted-foreground">{help}</p> : null}
  </div>;
}

function emptyNodeInput(): EgressNodeInput {
  return { name: "", scope: "grok_build", enabled: true, proxyPool: false, accountCapacity: 0, proxyURL: "", userAgent: "", cloudflareCookies: "" };
}

function StateBadge({ node, state, protectedNode }: { node: EgressNodeDTO; state?: QualityGuardNodeState; protectedNode: boolean }) {
  const { t } = useTranslation();
  if (state?.disabled_by_guard) return <Badge variant="destructive">{t("qualityGuard.quarantined")}</Badge>;
  if (protectedNode) return <Badge variant="secondary">{t("qualityGuard.fixedFallback")}</Badge>;
  if (!node.enabled) return <Badge variant="secondary">{t("common.disabled")}</Badge>;
  if (state?.quarantined_lease_count) return <Badge variant="outline" className="border-amber-500/40 text-amber-700 dark:text-amber-400" title={t("qualityGuard.leaseScopedHelp")}>{t("qualityGuard.leaseQuarantined", { count: state.quarantined_lease_count })}</Badge>;
  if (state?.observe_only) return <Badge variant="outline" className="border-sky-500/40 text-sky-700 dark:text-sky-400" title={t("qualityGuard.leaseScopedObserveOnlyHelp")}>{t("qualityGuard.leaseScopedObserveOnly")}</Badge>;
  if (node.accountBoundProxy) return <Badge variant="outline" className="border-sky-500/40 text-sky-700 dark:text-sky-400" title={t("qualityGuard.leaseScopedHelp")}>{t("qualityGuard.leaseScoped")}</Badge>;
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
        <div className="min-w-0"><p className="truncate text-xs font-medium">{event.node_name || `ID ${event.node_id}`} · {t(eventLabelKey(event.event))}</p><p className="mt-1 truncate text-[11px] text-muted-foreground">{t(reasonLabelKey(event.reason))}{event.account_id ? ` · ${t("qualityGuard.accountLease", { id: event.account_id })}` : ""}{event.request_id ? ` · ${event.request_id}` : ""}{event.cooldown_until ? ` · ${t("qualityGuard.leaseUntil", { time: formatTime(event.cooldown_until, locale) })}` : ""}{event.output_tps ? ` · ${formatTPS(event.output_tps)}` : ""}</p></div>
        <time className="text-[11px] text-muted-foreground">{formatTime(event.ts, locale)}</time>
      </div>)}
    </div>}
  </section>;
}

function eventLabelKey(event: string): string {
  if (event === "lease_scoped_quarantine_suppressed") return "qualityGuard.leaseScopedQuarantineSuppressedEvent";
  if (event === "lease_scoped_guard_released") return "qualityGuard.leaseScopedGuardReleasedEvent";
  if (event === "lease_quarantined") return "qualityGuard.leaseQuarantinedEvent";
  if (event === "lease_restored") return "qualityGuard.leaseRestoredEvent";
  if (event === "lease_quarantine_extended") return "qualityGuard.leaseQuarantineExtendedEvent";
  if (event === "lease_quarantine_failed" || event === "lease_quarantine_suppressed") return "qualityGuard.leaseQuarantineFailedEvent";
  return `qualityGuard.eventTypes.${event}`;
}

function reasonLabelKey(reason: string): string {
  if (reason === "lease_scoped_node") return "qualityGuard.leaseScopedNodeReason";
  if (reason === "fixed_fallback_node") return "qualityGuard.fixedFallback";
  return `qualityGuard.reasons.${reason || "unknown"}`;
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
  const expectedUpdateSeconds = status.config.mode === "active"
    ? status.config.active_interval_seconds
    : status.config.passive_poll_seconds;
  return Date.now() / 1000 - status.updatedAt < Math.max(60, expectedUpdateSeconds * 3);
}
function qualityTestState(result: QualityTestResult, status: QualityGuardStatus): QualityGuardNodeState {
  const softTPS = status.config?.soft_tps ?? 500;
  const hardTPS = status.config?.hard_tps ?? 1000;
  const profile = status.profiles?.find((item) => item.id === status.activeProfileId);
  const hasExpected = profile?.has_expected ?? true;
  const requireThinking = result.thinkingRequired;
  const failClosed = status.config?.fail_closed ?? false;
  const minimumGenerationMS = status.config?.min_generation_ms ?? 0;
  let classification = "healthy";
  let reason = "within_threshold";
  if (hasExpected && !result.expectedMatched) { classification = "hard"; reason = "expected_marker_missing"; }
  else if (result.outputTokens < 32) { classification = "soft"; reason = "insufficient_output_tokens"; }
  else if (requireThinking && result.outputTokens >= 64 && result.reasoningTokens <= 0) { classification = "hard"; reason = "missing_thinking"; }
  else if (failClosed && result.generationMs < minimumGenerationMS && result.outputTokensPerSecond >= softTPS) { classification = "hard"; reason = "buffered_burst"; }
  else if (failClosed && result.generationMs < minimumGenerationMS) { classification = "soft"; reason = "insufficient_generation_window"; }
  else if (result.outputTokensPerSecond >= hardTPS) { classification = "hard"; reason = "hard_tps"; }
  else if (result.outputTokensPerSecond >= softTPS) { classification = "soft"; reason = "soft_tps"; }
  const now = Date.now() / 1000;
  return {
    observe_only: false, observe_only_reason: "", quarantined_lease_count: 0,
    active_soft_strikes: classification === "soft" ? 1 : classification === "hard" ? (status.config?.consecutive_soft ?? 2) : 0,
    passive_soft_strikes: 0, error_strikes: 0, quarantined_until: 0, disabled_by_guard: false,
    last_reason: reason, last_probe_at: now, last_observed_at: now, last_source: "active",
    last_classification: classification, last_output_tps: result.outputTokensPerSecond,
    last_output_tokens: result.outputTokens, last_first_token_ms: result.firstTokenMs, last_duration_ms: result.durationMs,
  };
}
function formatTPS(value: number): string { return `${new Intl.NumberFormat(undefined, { maximumFractionDigits: 1 }).format(value)} Token/s`; }
function formatCount(value: number, locale: string): string { return new Intl.NumberFormat(locale, { maximumFractionDigits: 0 }).format(value); }
function formatDuration(seconds: number): string { if (seconds < 60) return `${seconds}s`; if (seconds % 3600 === 0) return `${seconds / 3600}h`; return `${seconds / 60}m`; }
function formatTime(value: number | undefined, locale: string): string { return value ? new Intl.DateTimeFormat(locale, { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", second: "2-digit" }).format(new Date(value * 1000)) : "-"; }
