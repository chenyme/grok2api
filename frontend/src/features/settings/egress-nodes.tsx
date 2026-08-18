import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Check, ChevronLeft, ChevronRight, ChevronsUpDown, CircleAlert, CircleHelp, Eye, EyeOff, MoreHorizontal, Network, Pencil, Plus, Power, PowerOff, RefreshCw, Search, Trash2, Upload } from "lucide-react";
import { type ReactNode, useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { Table, TableActionCell, TableActionHead, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Textarea } from "@/components/ui/textarea";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { EgressAutomation, EgressSources } from "@/features/settings/egress-operations";
import { EgressProxyProfiles } from "@/features/settings/egress-proxy-profiles";
import { cleanupUnhealthyEgressNodes, createEgressNode, deleteEgressNode, deleteEgressNodes, getEgressNodeProxyURL, getEgressProxyProfile, importEgressText, listEgressProxyProfiles, listEgressNodes, previewUnhealthyEgressNodes, refreshEgressClearance, testEgressNode, updateEgressNode, updateEgressNodesEnabled, type ClearanceMode, type EgressIPProbeDTO, type EgressNodeDTO, type EgressNodeInput, type EgressScope } from "@/features/settings/settings-api";
import { ErrorState, TableLoadingRow } from "@/shared/components/data-state";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { DataTableFilters } from "@/shared/components/data-table-filters";
import { Pagination } from "@/shared/components/pagination";
import { SortableTableHead } from "@/shared/components/sortable-table-head";
import { VirtualTableBody } from "@/shared/components/virtual-table-body";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { cn } from "@/shared/lib/cn";
import { nextTableSort, type SortOrder, type TableSort } from "@/shared/lib/table-sort";

const emptyInput: EgressNodeInput = { name: "", scope: "grok_build", enabled: true, proxyPool: false, accountCapacity: 0, proxyURL: "", userAgent: "", cloudflareCookies: "" };
type ImportForm = { name: string; scope: EgressScope; accountCapacity: number; content: string };
const emptyImport: ImportForm = { name: "", scope: "grok_build", accountCapacity: 0, content: "" };

export function EgressNodes({ title, clearanceMode }: { title: string; clearanceMode: ClearanceMode }) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [editing, setEditing] = useState<EgressNodeDTO | null | undefined>(undefined);
  const [proxyVisible, setProxyVisible] = useState(false);
  const [revealedProxyURL, setRevealedProxyURL] = useState("");
  const [importOpen, setImportOpen] = useState(false);
  const [importForm, setImportForm] = useState<ImportForm>(emptyImport);
  const [form, setForm] = useState<EgressNodeInput>(emptyInput);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [sort, setSort] = useState<TableSort>({ field: "", order: "asc" });
  const [search, setSearch] = useState("");
  const [scopeFilter, setScopeFilter] = useState("");
  const [enabledFilter, setEnabledFilter] = useState("");
  const [probeFilter, setProbeFilter] = useState("");
  const [assignmentFilter, setAssignmentFilter] = useState("");
  const [selected, setSelected] = useState<Map<string, EgressNodeDTO>>(() => new Map());
  const [batchDeleteOpen, setBatchDeleteOpen] = useState(false);
  const [cleanupOpen, setCleanupOpen] = useState(false);
  const [profileLibraryOpen, setProfileLibraryOpen] = useState(false);
  const [profileLibraryCreate, setProfileLibraryCreate] = useState(false);
  const debouncedSearch = useDebouncedValue(search);
  const query = useQuery({
    queryKey: ["egress-nodes", "page", page, pageSize, debouncedSearch, scopeFilter, enabledFilter, probeFilter, assignmentFilter, sort.field, sort.order],
    queryFn: () => listEgressNodes({
      page, pageSize, search: debouncedSearch, scope: scopeFilter as EgressScope | "", enabled: enabledFilter,
      probe: probeFilter, assignment: assignmentFilter, sortBy: sort.field || undefined, sortOrder: sort.field ? sort.order : undefined,
    }),
    staleTime: 15_000,
    refetchOnWindowFocus: true,
  });
  const save = useMutation({
    mutationFn: () => {
      const normalizedProxyURL = form.proxyURL?.trim() || "";
      const input = {
        ...form,
        proxyURL: normalizedProxyURL && (!editing || normalizedProxyURL !== revealedProxyURL) ? normalizedProxyURL : undefined,
        userAgent: form.scope === "grok_build" ? "" : form.userAgent,
        cloudflareCookies: form.scope === "grok_build" ? undefined : form.cloudflareCookies?.trim() || undefined,
      };
      return editing ? updateEgressNode(editing.id, input) : createEgressNode(input);
    },
    onSuccess: () => { void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] }); void queryClient.invalidateQueries({ queryKey: ["egress-proxy-profiles"] }); setEditing(undefined); toast.success(t("settings.egress.saved")); },
    onError: (error) => showError(error, t("settings.egress.operationFailed")),
  });
  const revealProxy = useMutation({
    mutationFn: () => {
      if (!editing) throw new Error(t("egressProxyProfiles.revealUnavailable"));
      return getEgressNodeProxyURL(editing.id);
    },
    onSuccess: ({ proxyURL }) => {
      setRevealedProxyURL(proxyURL);
      setProxyVisible(true);
      setForm((current) => ({ ...current, proxyURL }));
    },
    onError: (error) => showError(error, t("settings.egress.operationFailed")),
  });
  const importText = useMutation({
    mutationFn: () => importEgressText(importForm),
    onSuccess: (value) => { void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] }); setImportOpen(false); toast.success(t("settings.egress.imported", value)); },
    onError: (error) => showError(error, t("settings.egress.operationFailed")),
  });
  const remove = useMutation({
    mutationFn: deleteEgressNode,
    onSuccess: (_, id) => {
      if (page > 1 && query.data?.items.length === 1) setPage(page - 1);
      setSelected((current) => {
        const next = new Map(current);
        next.delete(id);
        return next;
      });
      void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
      void queryClient.invalidateQueries({ queryKey: ["egress-proxy-profiles"] });
      toast.success(t("settings.egress.deleted"));
    },
    onError: (error) => showError(error, t("settings.egress.operationFailed")),
  });
  const removeMany = useMutation({
    mutationFn: () => deleteEgressNodes([...selected.keys()]),
    onSuccess: (value) => {
      const selectedOnCurrentPage = query.data?.items.filter((node) => selected.has(node.id)).length ?? 0;
      if (page > 1 && query.data && query.data.items.length > 0 && selectedOnCurrentPage === query.data.items.length) setPage(page - 1);
      setSelected(new Map());
      setBatchDeleteOpen(false);
      void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
      void queryClient.invalidateQueries({ queryKey: ["egress-proxy-profiles"] });
      toast.success(t("settings.egress.batchDeleted", value));
    },
    onError: (error) => showError(error, t("settings.egress.operationFailed")),
  });
  const updateManyEnabled = useMutation({
    mutationFn: (enabled: boolean) => updateEgressNodesEnabled([...selected.keys()], enabled),
    onSuccess: (value, enabled) => {
      setSelected(new Map());
      void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
      toast.success(t(enabled ? "settings.egress.batchEnabled" : "settings.egress.batchDisabled", value));
    },
    onError: (error) => showError(error, t("settings.egress.operationFailed")),
  });
  const cleanupPreview = useMutation({
    mutationFn: previewUnhealthyEgressNodes,
  });
  const cleanupUnhealthy = useMutation({
    mutationFn: cleanupUnhealthyEgressNodes,
    onSuccess: (value) => {
      setPage(1);
      setSelected(new Map());
      setCleanupOpen(false);
      cleanupPreview.reset();
      void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
      void queryClient.invalidateQueries({ queryKey: ["egress-proxy-profiles"] });
      toast.success(t("settings.egress.cleanupUnavailableComplete", value));
    },
    onError: (error) => showError(error, t("settings.egress.operationFailed")),
  });
  const refreshClearance = useMutation({
    mutationFn: (id: string) => refreshEgressClearance(id),
    onSuccess: () => { void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] }); toast.success(t("settings.egress.clearanceRefreshed")); },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("settings.egress.operationFailed")),
  });
  const testNode = useMutation({
    mutationFn: testEgressNode,
    onSuccess: (result) => {
      if (result.status === "healthy") toast.success(t("settings.egress.testedOne"));
      else toast.error(result.error || t("settings.egress.operationFailed"));
    },
    onError: (error) => showError(error, t("settings.egress.operationFailed")),
    onSettled: () => { void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] }); },
  });

  function openCreate() {
    setForm(emptyInput);
    setProxyVisible(false);
    setRevealedProxyURL("");
    setEditing(null);
  }

  function openCleanup() {
    cleanupPreview.reset();
    setCleanupOpen(true);
    cleanupPreview.mutate();
  }

  function openEdit(node: EgressNodeDTO) {
    setForm({ name: node.name, scope: node.scope, enabled: node.enabled, proxyPool: node.proxyPool, accountCapacity: node.accountCapacity, proxyProfileId: node.proxyProfileId, userAgent: node.scope === "grok_build" ? "" : node.userAgent, proxyURL: "", cloudflareCookies: "" });
    setProxyVisible(false);
    setRevealedProxyURL("");
    setEditing(node);
  }

  function changeScope(scope: EgressScope) {
    const previousDefault = query.data?.defaultUserAgents[form.scope] ?? "";
    const nextDefault = query.data?.defaultUserAgents[scope] ?? "";
    setForm({
      ...form,
      scope,
      userAgent: scope === "grok_build" ? "" : (form.userAgent === "" || form.userAgent === previousDefault ? nextDefault : form.userAgent),
      cloudflareCookies: scope === "grok_build" || scope === "grok_console_asset" ? "" : form.cloudflareCookies,
    });
  }

  function scopeLabel(scope: EgressScope) {
    if (scope === "grok_build") return t("settings.egress.scopeBuild");
    if (scope === "grok_console") return t("console.name");
    if (scope === "grok_web_asset") return t("settings.egress.scopeWebAsset");
    if (scope === "grok_console_asset") return t("settings.egress.scopeConsoleAsset");
    return t("settings.egress.scopeWeb");
  }

  function changeSort(field: string, initialOrder: SortOrder): void {
    setSort((current) => nextTableSort(current, field, initialOrder));
    setPage(1);
  }

  function togglePage(checked: boolean): void {
    setSelected((current) => {
      const next = new Map(current);
      for (const node of nodes) {
        if (checked) next.set(node.id, node);
        else next.delete(node.id);
      }
      return next;
    });
  }

  function toggleNode(node: EgressNodeDTO, checked: boolean): void {
    setSelected((current) => {
      const next = new Map(current);
      if (checked) next.set(node.id, node);
      else next.delete(node.id);
      return next;
    });
  }

  const nodes = query.data?.items ?? [];
  const selectedOnPage = nodes.filter((node) => selected.has(node.id));
  const allPageSelected = nodes.length > 0 && selectedOnPage.length === nodes.length;
  const selectedNodes = [...selected.values()];
  const selectedAssignedAccounts = selectedNodes.reduce((total, node) => total + node.assignedAccountCount, 0);
  const selectedSourceNodes = selectedNodes.filter((node) => node.sourceId).length;
  const batchPending = removeMany.isPending || updateManyEnabled.isPending;
  const hasActiveFilters = Boolean(debouncedSearch || scopeFilter || enabledFilter || probeFilter || assignmentFilter);

  return (
    <div className="space-y-8">
      <EgressSources scopeLabel={scopeLabel} />

      <section className="space-y-3">
        <div className="flex min-h-8 items-center px-1">
          <h2 className="text-sm font-medium tracking-tight">{title}</h2>
        </div>
        <DataTableShell
          toolbar={(
            <>
              <div className="flex w-full min-w-0 items-center gap-2 sm:w-auto">
                <div className="relative min-w-0 flex-1 sm:w-64 sm:flex-none">
                  <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                  <Input className="h-8 pl-9 text-xs" value={search} onChange={(event) => { setSearch(event.target.value); setPage(1); setSelected(new Map()); }} placeholder={t("settings.egress.search")} aria-label={t("settings.egress.search")} />
                </div>
                <DataTableFilters filters={[
                  { id: "scope", label: t("settings.egress.scope"), value: scopeFilter, onChange: (value) => { setScopeFilter(value); setPage(1); setSelected(new Map()); }, options: [
                    { value: "grok_build", label: scopeLabel("grok_build") },
                    { value: "grok_web", label: scopeLabel("grok_web") },
                    { value: "grok_console", label: scopeLabel("grok_console") },
                    { value: "grok_web_asset", label: scopeLabel("grok_web_asset") },
                    { value: "grok_console_asset", label: scopeLabel("grok_console_asset") },
                  ] },
                  { id: "enabled", label: t("settings.egress.enabled"), value: enabledFilter, onChange: (value) => { setEnabledFilter(value); setPage(1); setSelected(new Map()); }, options: [
                    { value: "enabled", label: t("common.enable") },
                    { value: "disabled", label: t("common.disable") },
                  ] },
                  { id: "probe", label: t("settings.egress.probe"), value: probeFilter, onChange: (value) => { setProbeFilter(value); setPage(1); setSelected(new Map()); }, options: [
                    { value: "healthy", label: t("settings.egress.healthy") },
                    { value: "unhealthy", label: t("settings.egress.unhealthy") },
                    { value: "unknown", label: t("settings.egress.notTested") },
                  ] },
                  { id: "assignment", label: t("settings.egress.accounts"), value: assignmentFilter, onChange: (value) => { setAssignmentFilter(value); setPage(1); setSelected(new Map()); }, options: [
                    { value: "bound", label: t("settings.egress.assigned") },
                    { value: "unbound", label: t("settings.egress.unassigned") },
                  ] },
                ]} />
              </div>
              <div className="flex flex-wrap items-center gap-1.5">
                {selected.size > 0 ? (
                  <>
                    <span className="mr-1 text-xs text-muted-foreground">{t("common.selectedCount", { count: selected.size })}</span>
                    <Button type="button" size="sm" variant="secondary" disabled={batchPending || selectedNodes.every((node) => node.enabled)} onClick={() => updateManyEnabled.mutate(true)}><Power />{t("common.enable")}</Button>
                    <Button type="button" size="sm" variant="secondary" disabled={batchPending || selectedNodes.every((node) => !node.enabled)} onClick={() => updateManyEnabled.mutate(false)}><PowerOff />{t("common.disable")}</Button>
                    <Button type="button" size="sm" variant="secondary" className="bg-destructive/10 text-destructive hover:bg-destructive/15 hover:text-destructive" disabled={batchPending} onClick={() => setBatchDeleteOpen(true)}><Trash2 />{t("common.delete")}</Button>
                  </>
                ) : null}
                <Button type="button" size="sm" variant="secondary" onClick={() => { setProfileLibraryCreate(false); setProfileLibraryOpen(true); }}><Network />{t("egressProxyProfiles.libraryTitle")}</Button>
                <Button type="button" size="icon" variant="secondary" className="size-8" disabled={query.isFetching} onClick={() => void query.refetch()} aria-label={t("egressProxyProfiles.refreshNodes")} title={t("egressProxyProfiles.refreshNodes")}><RefreshCw className={cn(query.isFetching && "animate-spin")} /></Button>
                <Button type="button" size="sm" variant="secondary" disabled={cleanupUnhealthy.isPending} onClick={openCleanup}><Trash2 />{t("settings.egress.cleanupUnavailable")}</Button>
                <DropdownMenu>
                  <DropdownMenuTrigger asChild><Button type="button" size="sm" variant="secondary"><Plus />{t("settings.egress.add")}</Button></DropdownMenuTrigger>
                  <DropdownMenuContent align="end">
                    <DropdownMenuItem onClick={openCreate}><Plus />{t("settings.egress.addManually")}</DropdownMenuItem>
                    <DropdownMenuItem onClick={() => { setImportForm(emptyImport); setImportOpen(true); }}><Upload />{t("settings.egress.importText")}</DropdownMenuItem>
                  </DropdownMenuContent>
                </DropdownMenu>
              </div>
            </>
          )}
          footer={query.data && query.data.total > 0 ? <Pagination page={query.data.page} pageSize={query.data.pageSize} total={query.data.total} onPageChange={setPage} onPageSizeChange={(value) => { setPageSize(value); setPage(1); }} /> : undefined}
        >
          {query.isError ? <ErrorState message={query.error.message} onRetry={() => void query.refetch()} /> : null}
          {!query.isError ? <Table viewportRows={10} rowHeight={48} className="min-w-[920px] table-fixed">
          <TableHeader><TableRow className="hover:bg-transparent"><TableHead className="w-10 px-2"><Checkbox checked={allPageSelected ? true : selectedOnPage.length > 0 ? "indeterminate" : false} disabled={nodes.length === 0} onCheckedChange={(checked) => togglePage(checked === true)} aria-label={t("common.selectPage")} /></TableHead><SortableTableHead className="w-18" field="name" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("settings.egress.name")}</SortableTableHead><SortableTableHead className="w-24" field="scope" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort}>{t("settings.egress.scope")}</SortableTableHead><SortableTableHead className="w-44" field="proxy" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("settings.egress.proxy")}</SortableTableHead><SortableTableHead className="w-28" field="clearance" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort}>{t("settings.egress.clearance")}</SortableTableHead><TableHead className="w-14 text-center">{t("settings.egress.accounts")}</TableHead><SortableTableHead className="w-24" field="health" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" align="center" title={t("settings.egress.healthHelp")} onSort={changeSort}>{t("settings.egress.health")}</SortableTableHead><TableHead className="w-52"><div className="flex items-center justify-center gap-1"><span>{t("settings.egress.probe")}</span><Tooltip><TooltipTrigger asChild><button type="button" className="text-muted-foreground transition-colors hover:text-foreground" aria-label={t("settings.egress.probeHelp")}><CircleHelp className="size-3.5" /></button></TooltipTrigger><TooltipContent className="max-w-80">{t("settings.egress.probeHelp")}</TooltipContent></Tooltip></div></TableHead><TableActionHead /></TableRow></TableHeader>
          {query.isPending ? <TableBody><TableLoadingRow colSpan={9} /></TableBody> : null}
          {!query.isPending && nodes.length === 0 ? <TableBody><TableRow><TableCell colSpan={9} className="h-24 text-center text-xs text-muted-foreground">{hasActiveFilters ? t("settings.egress.noMatches") : t("settings.egress.directFallback")}</TableCell></TableRow></TableBody> : null}
          {!query.isPending && nodes.length > 0 ? <VirtualTableBody items={nodes} colSpan={9} rowHeight={48} renderRow={(node) => (
              <TableRow className="group h-12" key={node.id} data-state={selected.has(node.id) ? "selected" : undefined}>
                <TableCell className="px-2"><Checkbox checked={selected.has(node.id)} onCheckedChange={(checked) => toggleNode(node, checked === true)} aria-label={t("common.selectItem", { name: node.name })} /></TableCell>
                <TableCell>
                  <div className="flex min-w-0 items-center gap-2">
                    <span className={cn("size-1.5 shrink-0 rounded-full", node.enabled ? "bg-emerald-500" : "bg-muted-foreground/35")} />
                    <span className={cn("truncate text-xs font-medium", !node.enabled && "text-muted-foreground")} title={node.name}>{node.name}</span>
                    {node.lastError ? <ErrorTooltip message={node.lastError} /> : null}
                  </div>
                </TableCell>
                <TableCell className="text-center"><Badge variant="secondary" className="text-[10px]">{scopeLabel(node.scope)}</Badge></TableCell>
                <TableCell>
                  {node.proxyConfigured ? <div className="min-w-0" title={`${node.proxyDisplay || t("settings.egress.configured")} · ${node.proxyProfileName || node.proxyFingerprint || ""}`}><p className="truncate text-xs font-medium">{node.proxyDisplay || t("settings.egress.configured")}</p>{node.proxyProfileId ? <p className="truncate text-[10px] text-muted-foreground">{node.proxyProfileName || `#${node.proxyFingerprint}`}</p> : node.proxyFingerprint ? <p className="font-mono text-[10px] text-muted-foreground">#{node.proxyFingerprint}</p> : null}</div> : <Badge variant="outline" className="text-[10px] text-muted-foreground">{t("settings.egress.direct")}</Badge>}
                </TableCell>
                <TableCell className="text-center"><ClearanceBadge node={node} clearanceMode={clearanceMode} /></TableCell>
                <TableCell className="text-center text-xs tabular-nums"><span className="font-medium">{node.assignedAccountCount}</span>{node.accountCapacity > 0 ? <span className="text-muted-foreground"> / {node.accountCapacity}</span> : null}</TableCell>
                <TableCell><HealthMeter value={node.health} /></TableCell>
                <TableCell><ProbeSummary node={node} /></TableCell>
                <TableActionCell>
                  <DropdownMenu>
                    <DropdownMenuTrigger asChild><Button type="button" variant="ghost" size="icon" className="size-8" aria-label={t("common.actions")}><MoreHorizontal /></Button></DropdownMenuTrigger>
                    <DropdownMenuContent align="end">
                      <DropdownMenuItem onClick={() => openEdit(node)}><Pencil />{t("common.edit")}</DropdownMenuItem>
                      <DropdownMenuSeparator />
                      {clearanceMode !== "manual" && !node.accountBoundProxy && (node.scope === "grok_web" || node.scope === "grok_web_asset" || node.scope === "grok_console") ? <DropdownMenuItem disabled={refreshClearance.isPending} onClick={() => refreshClearance.mutate(node.id)}><RefreshCw />{t("settings.egress.refreshClearance")}</DropdownMenuItem> : null}
                      <DropdownMenuItem disabled={testNode.isPending || !node.proxyConfigured} onClick={() => testNode.mutate(node.id)}><RefreshCw />{t("settings.egress.test")}</DropdownMenuItem>
                      <DropdownMenuItem className="text-destructive focus:text-destructive" onClick={() => remove.mutate(node.id)}><Trash2 />{t("common.delete")}</DropdownMenuItem>
                    </DropdownMenuContent>
                  </DropdownMenu>
                </TableActionCell>
              </TableRow>
            )} /> : null}
          </Table>
          : null}
        </DataTableShell>
      </section>

      <EgressAutomation scopeLabel={scopeLabel} />

      <AlertDialog open={batchDeleteOpen} onOpenChange={setBatchDeleteOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("settings.egress.batchDeleteTitle", { count: selected.size })}</AlertDialogTitle>
            <AlertDialogDescription className="space-y-1">
              <span className="block">{t("settings.egress.batchDeleteDescription", { count: selected.size, accounts: selectedAssignedAccounts })}</span>
              {selectedSourceNodes > 0 ? <span className="block">{t("settings.egress.batchDeleteSourceHint", { count: selectedSourceNodes })}</span> : null}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter><AlertDialogCancel disabled={removeMany.isPending}>{t("common.cancel")}</AlertDialogCancel><AlertDialogAction className="bg-destructive text-white hover:bg-destructive/90" disabled={removeMany.isPending} onClick={(event) => { event.preventDefault(); removeMany.mutate(); }}>{removeMany.isPending ? <Spinner /> : null}{t("common.delete")}</AlertDialogAction></AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={cleanupOpen} onOpenChange={(open) => {
        if (!open && cleanupUnhealthy.isPending) return;
        if (!open) cleanupPreview.reset();
        setCleanupOpen(open);
      }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("settings.egress.cleanupUnavailableTitle")}</AlertDialogTitle>
            <AlertDialogDescription className="space-y-2">
              <span className="block">{t("settings.egress.cleanupUnavailableDescription")}</span>
              <span className="block">{t("settings.egress.cleanupUnavailableImpact")}</span>
            </AlertDialogDescription>
          </AlertDialogHeader>
          <div className="min-h-20 rounded-md bg-muted/45 p-3 text-xs">
            {cleanupPreview.isPending ? <div className="flex h-14 items-center justify-center gap-2 text-muted-foreground"><Spinner />{t("settings.egress.cleanupPreviewLoading")}</div> : null}
            {cleanupPreview.isError ? <div className="flex h-14 items-center justify-center text-center text-destructive">{t("settings.egress.cleanupPreviewFailed")}</div> : null}
            {cleanupPreview.data ? (
              <div className="grid grid-cols-3 gap-3 text-center">
                <CleanupPreviewValue label={t("settings.egress.cleanupNodeCount")} value={cleanupPreview.data.nodes} />
                <CleanupPreviewValue label={t("settings.egress.cleanupAccountCount")} value={cleanupPreview.data.boundAccounts} />
                <CleanupPreviewValue label={t("settings.egress.cleanupSubscriptionCount")} value={cleanupPreview.data.subscriptionManaged} />
              </div>
            ) : null}
          </div>
          {cleanupPreview.data && cleanupPreview.data.subscriptionManaged > 0 ? <p className="text-xs leading-5 text-amber-700 dark:text-amber-300">{t("settings.egress.cleanupSubscriptionHint")}</p> : null}
          <AlertDialogFooter>
            <AlertDialogCancel disabled={cleanupUnhealthy.isPending}>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-white hover:bg-destructive/90"
              disabled={cleanupUnhealthy.isPending || !cleanupPreview.data || cleanupPreview.data.nodes === 0}
              onClick={(event) => { event.preventDefault(); cleanupUnhealthy.mutate(); }}
            >
              {cleanupUnhealthy.isPending ? <Spinner /> : null}{t("settings.egress.cleanupUnavailableConfirm")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <Dialog open={editing !== undefined} onOpenChange={(open) => { if (!open) setEditing(undefined); }}>
        <DialogContent className="max-h-[calc(100svh-2rem)] overflow-y-auto sm:max-w-[520px]">
          <DialogHeader className="pr-8">
            <DialogTitle>{editing ? t("settings.egress.editTitle") : t("settings.egress.addTitle")}</DialogTitle>
            <DialogDescription>{t("console.egressDialogDescription")}</DialogDescription>
          </DialogHeader>
          <form className="space-y-3.5" onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); save.mutate(); }}>
            <div className="flex items-center justify-between gap-4 rounded-md bg-muted/45 px-3 py-2.5">
              <Label htmlFor="egress-enabled">{t("settings.egress.enabled")}</Label>
              <Switch id="egress-enabled" checked={form.enabled} onCheckedChange={(enabled) => setForm({ ...form, enabled })} />
            </div>
            <Field label={t("settings.egress.name")} controlId="egress-name">
              <Input id="egress-name" value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} />
            </Field>
            <Field label={t("settings.egress.capacity")} controlId="egress-capacity">
              <Input id="egress-capacity" type="number" min={0} max={100000} placeholder={t("settings.egress.unlimited")} value={form.accountCapacity || ""} onChange={(event) => setForm({ ...form, accountCapacity: Number(event.target.value) })} />
            </Field>
            <Field label={t("settings.egress.scope")} controlId="egress-scope">
              <Select value={form.scope} onValueChange={(value) => changeScope(value as EgressScope)}>
                <SelectTrigger id="egress-scope"><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="grok_build">{t("settings.egress.scopeBuild")}</SelectItem>
                  <SelectItem value="grok_web">{t("settings.egress.scopeWeb")}</SelectItem>
                  <SelectItem value="grok_console">{t("console.name")}</SelectItem>
                  <SelectItem value="grok_web_asset">{t("settings.egress.scopeWebAsset")}</SelectItem>
                  <SelectItem value="grok_console_asset">{t("settings.egress.scopeConsoleAsset")}</SelectItem>
                </SelectContent>
              </Select>
            </Field>
            {form.scope !== "grok_build" && form.scope !== "grok_console_asset" ? (
              <div className="flex h-10 items-center justify-between gap-4 rounded-md bg-muted/45 px-3">
                <span className="text-xs font-medium">{t("settings.egress.clearance")}</span>
                <Badge variant="secondary" className="shrink-0 text-[10px]">
                  {clearanceMode === "flaresolverr" ? t("settings.web.clearanceFlareSolverr") : clearanceMode === "on_demand" ? t("settings.web.clearanceOnDemand") : t("settings.web.clearanceManual")}
                </Badge>
              </div>
            ) : null}
            {!editing?.sourceId ? <Field label={t("egressProxyProfiles.assignment")} controlId="egress-proxy-profile" help={t("egressProxyProfiles.assignmentHelp")}>
              <ProxyProfilePicker value={form.proxyProfileId && form.proxyProfileId !== "0" ? form.proxyProfileId : "manual"} onChange={(value) => {
                setProxyVisible(false);
                setRevealedProxyURL("");
                setForm({ ...form, proxyProfileId: value === "manual" ? (editing?.proxyProfileId ? "0" : undefined) : value, proxyURL: "" });
              }} onCreate={() => { setProfileLibraryCreate(true); setProfileLibraryOpen(true); }} />
            </Field> : null}
            <Field label={t("settings.egress.proxyURL")} controlId="egress-proxy" help={t("settings.egress.proxyProtocols")}>
              <div className="flex gap-2">
              <Input id="egress-proxy" type={proxyVisible ? "text" : "password"} autoComplete="new-password" disabled={Boolean(form.proxyProfileId && form.proxyProfileId !== "0")} placeholder={form.proxyProfileId && form.proxyProfileId !== "0" ? t("egressProxyProfiles.managedByProfile") : editing?.proxyConfigured ? t("settings.egress.keepConfigured") : "socks5h://user:pass@host:port"} value={form.proxyURL} onChange={(event) => {
                const proxyURL = event.target.value;
                setForm({ ...form, proxyURL, proxyProfileId: editing?.proxyProfileId ? "0" : undefined, proxyPool: editing?.proxyConfigured || proxyURL.trim() ? form.proxyPool : false });
              }} />
              {editing?.proxyConfigured ? <Button type="button" variant="outline" size="icon" className="shrink-0" disabled={revealProxy.isPending || Boolean(form.proxyProfileId && form.proxyProfileId !== "0")} aria-label={t(proxyVisible ? "egressProxyProfiles.hide" : "egressProxyProfiles.reveal")} onClick={() => {
                if (revealedProxyURL) setProxyVisible((visible) => !visible);
                else revealProxy.mutate();
              }}>{revealProxy.isPending ? <Spinner /> : proxyVisible ? <EyeOff /> : <Eye />}</Button> : null}
              </div>
            </Field>
            <div className="flex items-start justify-between gap-4 rounded-md bg-muted/45 px-3 py-2.5">
              <div className="space-y-1">
                <Label htmlFor="egress-proxy-pool">{t("settings.egress.proxyPool")}</Label>
                <p className="max-w-[390px] text-xs leading-5 text-muted-foreground">{t("settings.egress.proxyPoolHelp")}</p>
              </div>
              <Switch id="egress-proxy-pool" className="mt-0.5" checked={form.proxyPool} disabled={!editing?.proxyConfigured && !form.proxyURL?.trim() && !form.proxyProfileId} onCheckedChange={(proxyPool) => setForm({ ...form, proxyPool })} />
            </div>
            {form.scope !== "grok_build" && (clearanceMode === "manual" || form.scope === "grok_console_asset") ? (
              <Field label={t("settings.egress.userAgent")} controlId="egress-user-agent">
                <Input id="egress-user-agent" value={form.userAgent} onChange={(event) => setForm({ ...form, userAgent: event.target.value })} />
              </Field>
            ) : null}
            {form.scope !== "grok_build" && form.scope !== "grok_console_asset" && clearanceMode === "manual" ? (
              <Field label={t("settings.egress.cloudflareCookie")} controlId="egress-cookie">
                <Input id="egress-cookie" type="password" autoComplete="new-password" placeholder={editing?.cookieConfigured ? t("settings.egress.keepConfigured") : "cf_clearance=...; __cf_bm=..."} value={form.cloudflareCookies} onChange={(event) => setForm({ ...form, cloudflareCookies: event.target.value })} />
              </Field>
            ) : null}
            <DialogFooter>
              <Button type="button" variant="secondary" size="sm" onClick={() => setEditing(undefined)}>{t("common.cancel")}</Button>
              <Button type="submit" size="sm" disabled={!form.name.trim() || save.isPending}>{save.isPending ? <Spinner /> : null}{t("common.save")}</Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      <Dialog open={importOpen} onOpenChange={setImportOpen}>
        <DialogContent className="max-h-[calc(100svh-2rem)] overflow-y-auto sm:max-w-[620px]">
          <DialogHeader className="pr-8"><DialogTitle>{t("settings.egress.importText")}</DialogTitle><DialogDescription>{t("settings.egress.importDialogDescription")}</DialogDescription></DialogHeader>
          <form className="space-y-3.5" onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); importText.mutate(); }}>
            <div className="grid gap-3 sm:grid-cols-2">
              <Field label={t("settings.egress.name")} controlId="egress-import-name"><Input id="egress-import-name" value={importForm.name} onChange={(event) => setImportForm({ ...importForm, name: event.target.value })} /></Field>
              <Field label={t("settings.egress.scope")} controlId="egress-import-scope">
                <Select value={importForm.scope} onValueChange={(value) => setImportForm({ ...importForm, scope: value as EgressScope })}>
                  <SelectTrigger id="egress-import-scope"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="grok_build">{t("settings.egress.scopeBuild")}</SelectItem>
                    <SelectItem value="grok_web">{t("settings.egress.scopeWeb")}</SelectItem>
                    <SelectItem value="grok_console">{t("console.name")}</SelectItem>
                    <SelectItem value="grok_web_asset">{t("settings.egress.scopeWebAsset")}</SelectItem>
                    <SelectItem value="grok_console_asset">{t("settings.egress.scopeConsoleAsset")}</SelectItem>
                  </SelectContent>
                </Select>
              </Field>
            </div>
            <Field label={t("settings.egress.capacity")} controlId="egress-import-capacity"><Input id="egress-import-capacity" type="number" min={0} max={100000} placeholder={t("settings.egress.unlimited")} value={importForm.accountCapacity || ""} onChange={(event) => setImportForm({ ...importForm, accountCapacity: Number(event.target.value) })} /></Field>
            <Field label={t("settings.egress.proxyList")} controlId="egress-import-list"><Textarea className="min-h-52 font-mono text-xs" id="egress-import-list" value={importForm.content} onChange={(event) => setImportForm({ ...importForm, content: event.target.value })} /></Field>
            <DialogFooter><Button type="button" size="sm" variant="secondary" onClick={() => setImportOpen(false)}>{t("common.cancel")}</Button><Button type="submit" size="sm" disabled={!importForm.name.trim() || !importForm.content.trim() || importText.isPending}>{importText.isPending ? <Spinner /> : null}{t("settings.egress.importText")}</Button></DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      {profileLibraryOpen ? <EgressProxyProfiles
        key={profileLibraryCreate ? "create" : "manage"}
        open={profileLibraryOpen}
        startCreating={profileLibraryCreate}
        onOpenChange={(open) => { setProfileLibraryOpen(open); if (!open) setProfileLibraryCreate(false); }}
        onCreated={(profile) => {
          if (!profileLibraryCreate || editing === undefined) return;
          setProxyVisible(false);
          setRevealedProxyURL("");
          setForm((current) => ({ ...current, proxyProfileId: profile.id, proxyURL: "" }));
          setProfileLibraryOpen(false);
          setProfileLibraryCreate(false);
        }}
      /> : null}
    </div>
  );
}

function ProxyProfilePicker({ value, onChange, onCreate }: { value: string; onChange: (value: string) => void; onCreate: () => void }) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const [search, setSearch] = useState("");
  const [page, setPage] = useState(1);
  const debouncedSearch = useDebouncedValue(search);
  const pageSize = 20;
  const profiles = useQuery({
    queryKey: ["egress-proxy-profiles", "picker", page, debouncedSearch],
    queryFn: () => listEgressProxyProfiles({ page, pageSize, search: debouncedSearch }),
    enabled: open,
    staleTime: 30_000,
  });
  const selected = useQuery({
    queryKey: ["egress-proxy-profiles", "selected", value],
    queryFn: () => getEgressProxyProfile(value),
    enabled: value !== "manual",
    staleTime: 30_000,
  });
  const selectedLabel = value === "manual"
    ? t("egressProxyProfiles.independent")
    : selected.data
      ? `${selected.data.name} · ${selected.data.proxyDisplay || "—"}`
      : selected.isError
        ? t("egressProxyProfiles.selectionUnavailable", { id: value })
        : t("egressProxyProfiles.loadingSelection");
  const pages = Math.max(1, Math.ceil((profiles.data?.total ?? 0) / pageSize));

  function choose(next: string) {
    onChange(next);
    setOpen(false);
  }

  return (
    <Popover open={open} onOpenChange={(next) => { setOpen(next); if (next) setPage(1); }}>
      <PopoverTrigger asChild>
        <Button id="egress-proxy-profile" type="button" variant="outline" role="combobox" aria-expanded={open} className="w-full justify-between px-3 font-normal">
          <span className="truncate text-left">{selectedLabel}</span>
          <ChevronsUpDown className="ml-2 size-4 shrink-0 text-muted-foreground" />
        </Button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-[var(--radix-popover-trigger-width)] p-2">
        <div className="relative">
          <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
          <Input type="search" autoComplete="off" data-1p-ignore data-lpignore="true" data-form-type="other" className="h-8 pl-8 text-xs" value={search} onChange={(event) => { setSearch(event.target.value); setPage(1); }} placeholder={t("egressProxyProfiles.search")} aria-label={t("egressProxyProfiles.search")} />
        </div>
        <div className="mt-2 max-h-56 space-y-0.5 overflow-y-auto overscroll-contain" role="listbox" aria-label={t("egressProxyProfiles.assignment")}>
          <button type="button" role="option" aria-selected={value === "manual"} className={cn("flex min-h-9 w-full items-center gap-2 rounded-md px-2 text-left text-xs hover:bg-accent", value === "manual" && "bg-accent/60")} onClick={() => choose("manual")}>
            <Check className={cn("size-3.5 shrink-0", value !== "manual" && "invisible")} />
            <span className="truncate">{t("egressProxyProfiles.independent")}</span>
          </button>
          {profiles.isPending ? <div className="flex min-h-16 items-center justify-center"><Spinner /></div> : null}
          {profiles.isError ? <p className="p-3 text-center text-xs text-destructive">{profiles.error.message}</p> : null}
          {!profiles.isPending && !profiles.isError && profiles.data?.items.length === 0 ? <p className="p-3 text-center text-xs text-muted-foreground">{t("egressProxyProfiles.noMatches")}</p> : null}
          {profiles.data?.items.map((profile) => <button key={profile.id} type="button" role="option" aria-selected={value === profile.id} className={cn("flex min-h-11 w-full items-center gap-2 rounded-md px-2 text-left hover:bg-accent", value === profile.id && "bg-accent/60")} onClick={() => choose(profile.id)}>
            <Check className={cn("size-3.5 shrink-0", value !== profile.id && "invisible")} />
            <span className="min-w-0 flex-1"><span className="block truncate text-xs font-medium">{profile.name}</span><span className="block truncate text-[11px] text-muted-foreground">{profile.proxyDisplay || "—"} · {t("egressProxyProfiles.nodeCount", { count: profile.boundNodeCount })}</span></span>
          </button>)}
        </div>
        {profiles.data && profiles.data.total > pageSize ? <div className="mt-2 flex h-8 items-center justify-between border-t px-1 pt-2"><span className="text-[11px] text-muted-foreground">{t("common.pageOf", { page, pages })}</span><div className="flex gap-0.5"><Button type="button" variant="ghost" size="icon" className="size-7" disabled={page <= 1} onClick={() => setPage(page - 1)} aria-label={t("common.previousPage")}><ChevronLeft /></Button><Button type="button" variant="ghost" size="icon" className="size-7" disabled={page >= pages} onClick={() => setPage(page + 1)} aria-label={t("common.nextPage")}><ChevronRight /></Button></div></div> : null}
        <div className="mt-2 border-t pt-2">
          <Button type="button" variant="ghost" size="sm" className="w-full justify-start text-muted-foreground" onClick={() => { setOpen(false); onCreate(); }}><Plus />{t("egressProxyProfiles.createFromPicker")}</Button>
        </div>
      </PopoverContent>
    </Popover>
  );
}

function CleanupPreviewValue({ label, value }: { label: string; value: number }) {
  return (
    <div className="space-y-1">
      <div className="text-base font-medium tabular-nums text-foreground">{value}</div>
      <div className="text-muted-foreground">{label}</div>
    </div>
  );
}

function ErrorTooltip({ message }: { message: string }) {
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span className="inline-flex shrink-0 cursor-help text-destructive" tabIndex={0} aria-label={message}><CircleAlert className="size-3.5" /></span>
      </TooltipTrigger>
      <TooltipContent className="max-w-80">{message}</TooltipContent>
    </Tooltip>
  );
}

function ClearanceBadge({ node, clearanceMode }: { node: EgressNodeDTO; clearanceMode: ClearanceMode }) {
  const { t } = useTranslation();
  if (node.scope === "grok_build") return <span className="text-xs text-muted-foreground">—</span>;
  if (clearanceMode === "flaresolverr") {
    return <Badge variant="secondary" className="text-[10px]">{node.accountBoundProxy ? `${t("settings.web.clearanceFlareSolverr")} · Resin` : t("settings.web.clearanceFlareSolverr")}</Badge>;
  }
  if (clearanceMode === "on_demand") {
    return <Badge variant="secondary" className="text-[10px]">{node.accountBoundProxy ? `${t("settings.web.clearanceOnDemand")} · Resin` : t("settings.web.clearanceOnDemand")}</Badge>;
  }
  return <Badge variant={node.cookieConfigured ? "secondary" : "outline"} className={cn("text-[10px]", !node.cookieConfigured && "text-muted-foreground")}>{node.cookieConfigured ? t("settings.egress.configured") : t("settings.egress.none")}</Badge>;
}

function HealthMeter({ value }: { value: number }) {
  const percent = Math.max(0, Math.min(100, Math.round(value * 100)));
  return (
    <div className="mx-auto flex w-20 items-center gap-1.5">
      <div className="h-1.5 min-w-0 flex-1 overflow-hidden rounded-full bg-muted">
        <div className={cn("h-full rounded-full transition-[width]", percent >= 70 ? "bg-emerald-500" : percent >= 35 ? "bg-amber-500" : "bg-destructive")} style={{ width: `${percent}%` }} />
      </div>
      <span className="w-8 text-right text-[11px] tabular-nums text-muted-foreground">{percent}%</span>
    </div>
  );
}

function ProbeSummary({ node }: { node: EgressNodeDTO }) {
  return (
    <div className="flex w-full justify-center">
      <div className="grid w-fit max-w-full grid-cols-[2rem_auto] grid-rows-2 items-center gap-x-2 gap-y-1 py-1 text-left text-xs">
        <ProbeFamilySummary family="IPv4" probe={node.ipv4Probe} row={1} />
        <ProbeFamilySummary family="IPv6" probe={node.ipv6Probe} row={2} />
      </div>
    </div>
  );
}

function ProbeFamilySummary({ family, probe, row }: { family: "IPv4" | "IPv6"; probe: EgressIPProbeDTO; row: 1 | 2 }) {
  const { t } = useTranslation();
  const healthy = probe.status === "healthy";
  const unhealthy = probe.status === "unhealthy";
  const rowClass = row === 1 ? "row-start-1" : "row-start-2";
  const stateContent = <span className="flex min-w-0 items-center gap-1.5">
    <span className={cn("size-1.5 shrink-0 rounded-full", healthy ? "bg-emerald-500" : unhealthy ? "bg-destructive" : "bg-muted-foreground/35")} />
    <span className={cn("truncate text-[10px]", healthy ? "text-foreground" : unhealthy ? "text-destructive" : "text-muted-foreground")} title={healthy ? probe.exitIp : undefined}>
      {healthy ? probe.exitIp || t("settings.egress.healthy") : unhealthy ? t("settings.egress.unhealthy") : t("settings.egress.notTested")}
    </span>
  </span>;
  return (
    <div className="contents">
      <span className={cn("col-start-1 text-[10px] font-medium text-muted-foreground", rowClass)}>{family}</span>
      <span className={cn("col-start-2 flex min-w-0 max-w-[10rem] items-center gap-1.5", rowClass)}>
        {probe.status === "unknown" ? stateContent : (
          <Tooltip>
            <TooltipTrigger asChild><span className="min-w-0 cursor-help">{stateContent}</span></TooltipTrigger>
            <TooltipContent>{t("settings.egress.probeLatency", { latency: probe.latencyMs })}</TooltipContent>
          </Tooltip>
        )}
        {unhealthy && probe.error ? <ErrorTooltip message={probe.error} /> : null}
      </span>
    </div>
  );
}

function Field({ label, controlId, description, help, children }: { label: string; controlId: string; description?: string; help?: string; children: ReactNode }) {
  return (
    <div className="space-y-2">
      <div className="flex items-center gap-1.5">
        <Label htmlFor={controlId}>{label}</Label>
        {help ? (
          <Tooltip>
            <TooltipTrigger asChild><button type="button" className="text-muted-foreground transition-colors hover:text-foreground" aria-label={help}><CircleHelp className="size-3.5" /></button></TooltipTrigger>
            <TooltipContent className="max-w-80 whitespace-pre-line">{help}</TooltipContent>
          </Tooltip>
        ) : null}
      </div>
      {children}
      {description ? <p className="whitespace-pre-line text-xs leading-5 text-muted-foreground">{description}</p> : null}
    </div>
  );
}

function showError(error: unknown, fallback: string) {
  toast.error(error instanceof Error ? error.message : fallback);
}
