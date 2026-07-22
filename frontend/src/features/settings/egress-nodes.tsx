import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { CircleAlert, CircleHelp, MoreHorizontal, Pencil, Plus, RefreshCw, Search, Trash2 } from "lucide-react";
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
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { Table, TableActionCell, TableActionHead, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { EgressOperations } from "@/features/settings/egress-operations";
import { createEgressNode, deleteEgressNode, deleteEgressNodes, listEgressNodes, refreshEgressClearance, testEgressNode, updateEgressNode, type EgressNodeDTO, type EgressNodeInput, type EgressScope } from "@/features/settings/settings-api";
import { ErrorState, TableLoadingRow } from "@/shared/components/data-state";
import { DataTableFilters } from "@/shared/components/data-table-filters";
import { SortableTableHead } from "@/shared/components/sortable-table-head";
import { cn } from "@/shared/lib/cn";
import { nextTableSort, type SortOrder, type TableSort } from "@/shared/lib/table-sort";

const emptyInput: EgressNodeInput = { name: "", scope: "grok_build", enabled: true, proxyPool: false, accountCapacity: 0, proxyURL: "", userAgent: "", cloudflareCookies: "" };

export function EgressNodes({ title, clearanceMode }: { title: string; clearanceMode: "manual" | "flaresolverr" }) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [editing, setEditing] = useState<EgressNodeDTO | null | undefined>(undefined);
  const [form, setForm] = useState<EgressNodeInput>(emptyInput);
  const [sort, setSort] = useState<TableSort>({ field: "", order: "asc" });
  const [search, setSearch] = useState("");
  const [scopeFilter, setScopeFilter] = useState("");
  const [enabledFilter, setEnabledFilter] = useState("");
  const [probeFilter, setProbeFilter] = useState("");
  const [assignmentFilter, setAssignmentFilter] = useState("");
  const [selected, setSelected] = useState<Set<string>>(() => new Set());
  const [batchDeleteOpen, setBatchDeleteOpen] = useState(false);
  const query = useQuery({ queryKey: ["egress-nodes", sort.field, sort.order], queryFn: () => listEgressNodes({ sortBy: sort.field || undefined, sortOrder: sort.field ? sort.order : undefined }) });
  const save = useMutation({
    mutationFn: () => {
      const input = {
        ...form,
        proxyURL: form.proxyURL?.trim() || undefined,
        userAgent: form.scope === "grok_build" ? "" : form.userAgent,
        cloudflareCookies: form.scope === "grok_build" ? undefined : form.cloudflareCookies?.trim() || undefined,
      };
      return editing ? updateEgressNode(editing.id, input) : createEgressNode(input);
    },
    onSuccess: () => { void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] }); setEditing(undefined); toast.success(t("settings.egress.saved")); },
    onError: (error) => showError(error, t("settings.egress.operationFailed")),
  });
  const remove = useMutation({
    mutationFn: deleteEgressNode,
    onSuccess: (_, id) => {
      setSelected((current) => {
        const next = new Set(current);
        next.delete(id);
        return next;
      });
      void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
      toast.success(t("settings.egress.deleted"));
    },
    onError: (error) => showError(error, t("settings.egress.operationFailed")),
  });
  const removeMany = useMutation({
    mutationFn: () => deleteEgressNodes([...selected]),
    onSuccess: (value) => {
      setSelected(new Set());
      setBatchDeleteOpen(false);
      void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
      toast.success(t("settings.egress.batchDeleted", value));
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
    setEditing(null);
  }

  function openEdit(node: EgressNodeDTO) {
    setForm({ name: node.name, scope: node.scope, enabled: node.enabled, proxyPool: node.proxyPool, accountCapacity: node.accountCapacity, userAgent: node.scope === "grok_build" ? "" : node.userAgent, proxyURL: "", cloudflareCookies: "" });
    setEditing(node);
  }

  function changeScope(scope: EgressScope) {
    const previousDefault = query.data?.defaultUserAgents[form.scope] ?? "";
    const nextDefault = query.data?.defaultUserAgents[scope] ?? "";
    setForm({
      ...form,
      scope,
      userAgent: scope === "grok_build" ? "" : (form.userAgent === "" || form.userAgent === previousDefault ? nextDefault : form.userAgent),
      cloudflareCookies: scope === "grok_build" ? "" : form.cloudflareCookies,
    });
  }

  function scopeLabel(scope: EgressScope) {
    if (scope === "grok_build") return t("settings.egress.scopeBuild");
    if (scope === "grok_console") return t("console.name");
    if (scope === "grok_web_asset") return t("settings.egress.scopeWebAsset");
    return t("settings.egress.scopeWeb");
  }

  function changeSort(field: string, initialOrder: SortOrder): void {
    setSort((current) => nextTableSort(current, field, initialOrder));
  }

  function toggleVisible(checked: boolean): void {
    setSelected((current) => {
      const next = new Set(current);
      for (const node of filteredNodes) {
        if (checked) next.add(node.id);
        else next.delete(node.id);
      }
      return next;
    });
  }

  function toggleNode(id: string, checked: boolean): void {
    setSelected((current) => {
      const next = new Set(current);
      if (checked) next.add(id);
      else next.delete(id);
      return next;
    });
  }

  const nodes = query.data?.items ?? [];
  const normalizedSearch = search.trim().toLocaleLowerCase();
  const filteredNodes = nodes.filter((node) => {
    if (normalizedSearch && !node.name.toLocaleLowerCase().includes(normalizedSearch)) return false;
    if (scopeFilter && node.scope !== scopeFilter) return false;
    if (enabledFilter === "enabled" && !node.enabled) return false;
    if (enabledFilter === "disabled" && node.enabled) return false;
    if (probeFilter && node.probeStatus !== probeFilter) return false;
    if (assignmentFilter === "bound" && node.assignedAccountCount === 0) return false;
    if (assignmentFilter === "unbound" && node.assignedAccountCount > 0) return false;
    return true;
  });
  const selectedVisible = filteredNodes.filter((node) => selected.has(node.id));
  const allVisibleSelected = filteredNodes.length > 0 && selectedVisible.length === filteredNodes.length;
  const selectedAssignedAccounts = nodes.filter((node) => selected.has(node.id)).reduce((total, node) => total + node.assignedAccountCount, 0);
  const selectedSourceNodes = nodes.filter((node) => selected.has(node.id) && node.sourceId).length;

  return (
    <div className="space-y-8">
      <section className="space-y-3">
        <div className="flex min-h-8 items-center justify-between gap-3 px-1">
          <h2 className="text-sm font-medium tracking-tight">{title}</h2>
          <Button type="button" size="sm" variant="secondary" onClick={openCreate}><Plus />{t("settings.egress.add")}</Button>
        </div>
        <div className="flex flex-wrap items-center justify-between gap-2 px-1">
          <div className="flex min-w-0 flex-1 items-center gap-2 sm:flex-none">
            <div className="relative min-w-0 flex-1 sm:w-64 sm:flex-none">
              <Search className="pointer-events-none absolute left-3 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
              <Input className="h-8 pl-9 text-xs" value={search} onChange={(event) => { setSearch(event.target.value); setSelected(new Set()); }} placeholder={t("settings.egress.search")} aria-label={t("settings.egress.search")} />
            </div>
            <DataTableFilters filters={[
              { id: "scope", label: t("settings.egress.scope"), value: scopeFilter, onChange: (value) => { setScopeFilter(value); setSelected(new Set()); }, options: [
                { value: "grok_build", label: scopeLabel("grok_build") },
                { value: "grok_web", label: scopeLabel("grok_web") },
                { value: "grok_console", label: scopeLabel("grok_console") },
                { value: "grok_web_asset", label: scopeLabel("grok_web_asset") },
              ] },
              { id: "enabled", label: t("settings.egress.enabled"), value: enabledFilter, onChange: (value) => { setEnabledFilter(value); setSelected(new Set()); }, options: [
                { value: "enabled", label: t("common.enable") },
                { value: "disabled", label: t("common.disable") },
              ] },
              { id: "probe", label: t("settings.egress.probe"), value: probeFilter, onChange: (value) => { setProbeFilter(value); setSelected(new Set()); }, options: [
                { value: "healthy", label: t("settings.egress.healthy") },
                { value: "unhealthy", label: t("settings.egress.unhealthy") },
                { value: "unknown", label: t("settings.egress.notTested") },
              ] },
              { id: "assignment", label: t("settings.egress.accounts"), value: assignmentFilter, onChange: (value) => { setAssignmentFilter(value); setSelected(new Set()); }, options: [
                { value: "bound", label: t("settings.egress.assigned") },
                { value: "unbound", label: t("settings.egress.unassigned") },
              ] },
            ]} />
          </div>
          {selected.size > 0 ? (
            <div className="flex items-center gap-1.5">
              <span className="text-xs text-muted-foreground">{t("common.selectedCount", { count: selected.size })}</span>
              <Button type="button" size="sm" variant="secondary" className="bg-destructive/10 text-destructive hover:bg-destructive/15 hover:text-destructive" disabled={removeMany.isPending} onClick={() => setBatchDeleteOpen(true)}><Trash2 />{t("common.delete")}</Button>
            </div>
          ) : null}
        </div>
        {query.isError ? <ErrorState message={query.error.message} onRetry={() => void query.refetch()} /> : <div className="overflow-hidden rounded-md border">
          <Table className="min-w-[920px]">
          <TableHeader><TableRow><TableHead className="w-10 px-2"><Checkbox checked={allVisibleSelected ? true : selectedVisible.length > 0 ? "indeterminate" : false} disabled={filteredNodes.length === 0} onCheckedChange={(checked) => toggleVisible(checked === true)} aria-label={t("settings.egress.selectVisible")} /></TableHead><SortableTableHead className="min-w-44" field="name" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("settings.egress.name")}</SortableTableHead><SortableTableHead className="w-28" field="scope" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort}>{t("settings.egress.scope")}</SortableTableHead><SortableTableHead className="w-24" field="proxy" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort}>{t("settings.egress.proxy")}</SortableTableHead><SortableTableHead className="min-w-32" field="clearance" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort}>{t("settings.egress.clearance")}</SortableTableHead><TableHead className="w-24 text-center">{t("settings.egress.accounts")}</TableHead><SortableTableHead className="w-32" field="health" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" align="center" onSort={changeSort}>{t("settings.egress.health")}</SortableTableHead><TableHead className="min-w-40 text-center">{t("settings.egress.probe")}</TableHead><TableActionHead /></TableRow></TableHeader>
          <TableBody>
            {query.isPending ? <TableLoadingRow colSpan={9} /> : null}
            {!query.isPending && filteredNodes.length === 0 ? <TableRow><TableCell colSpan={9} className="h-24 text-center text-xs text-muted-foreground">{nodes.length === 0 ? t("settings.egress.directFallback") : t("settings.egress.noMatches")}</TableCell></TableRow> : filteredNodes.map((node) => (
              <TableRow className="group h-12" key={node.id} data-state={selected.has(node.id) ? "selected" : undefined}>
                <TableCell className="px-2"><Checkbox checked={selected.has(node.id)} onCheckedChange={(checked) => toggleNode(node.id, checked === true)} aria-label={t("common.selectItem", { name: node.name })} /></TableCell>
                <TableCell>
                  <div className="flex min-w-0 items-center gap-2">
                    <span className={cn("size-1.5 shrink-0 rounded-full", node.enabled ? "bg-emerald-500" : "bg-muted-foreground/35")} />
                    <span className={cn("truncate text-xs font-medium", !node.enabled && "text-muted-foreground")}>{node.name}</span>
                    {node.lastError ? <ErrorTooltip message={node.lastError} /> : null}
                  </div>
                </TableCell>
                <TableCell className="text-center"><Badge variant="secondary" className="text-[10px]">{scopeLabel(node.scope)}</Badge></TableCell>
                <TableCell className="text-center"><Badge variant={node.proxyConfigured ? "secondary" : "outline"} className={cn("text-[10px]", node.proxyConfigured ? "bg-emerald-500/10 text-emerald-700 dark:text-emerald-300" : "text-muted-foreground")}>{node.proxyConfigured ? t("settings.egress.configured") : t("settings.egress.direct")}</Badge></TableCell>
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
                      {clearanceMode === "flaresolverr" && !node.accountBoundProxy && (node.scope === "grok_web" || node.scope === "grok_web_asset" || node.scope === "grok_console") ? <DropdownMenuItem disabled={refreshClearance.isPending} onClick={() => refreshClearance.mutate(node.id)}><RefreshCw />{t("settings.egress.refreshClearance")}</DropdownMenuItem> : null}
                      <DropdownMenuItem disabled={testNode.isPending || !node.proxyConfigured} onClick={() => testNode.mutate(node.id)}><RefreshCw />{t("settings.egress.test")}</DropdownMenuItem>
                      <DropdownMenuItem className="text-destructive focus:text-destructive" onClick={() => remove.mutate(node.id)}><Trash2 />{t("common.delete")}</DropdownMenuItem>
                    </DropdownMenuContent>
                  </DropdownMenu>
                </TableActionCell>
              </TableRow>
            ))}
          </TableBody>
          </Table>
        </div>}
      </section>

      <EgressOperations scopeLabel={scopeLabel} />

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
                </SelectContent>
              </Select>
            </Field>
            {form.scope !== "grok_build" ? (
              <div className="flex h-10 items-center justify-between gap-4 rounded-md bg-muted/45 px-3">
                <span className="text-xs font-medium">{t("settings.egress.clearance")}</span>
                <Badge variant="secondary" className="shrink-0 text-[10px]">
                  {clearanceMode === "flaresolverr" ? t("settings.web.clearanceFlareSolverr") : t("settings.web.clearanceManual")}
                </Badge>
              </div>
            ) : null}
            <Field label={t("settings.egress.proxyURL")} controlId="egress-proxy" help={t("settings.egress.proxyProtocols")}>
              <Input id="egress-proxy" type="password" autoComplete="new-password" placeholder={editing?.proxyConfigured ? t("settings.egress.keepConfigured") : "socks5h://user:pass@host:port"} value={form.proxyURL} onChange={(event) => {
                const proxyURL = event.target.value;
                setForm({ ...form, proxyURL, proxyPool: editing?.proxyConfigured || proxyURL.trim() ? form.proxyPool : false });
              }} />
            </Field>
            <div className="flex items-start justify-between gap-4 rounded-md bg-muted/45 px-3 py-2.5">
              <div className="space-y-1">
                <Label htmlFor="egress-proxy-pool">{t("settings.egress.proxyPool")}</Label>
                <p className="max-w-[390px] text-xs leading-5 text-muted-foreground">{t("settings.egress.proxyPoolHelp")}</p>
              </div>
              <Switch id="egress-proxy-pool" className="mt-0.5" checked={form.proxyPool} disabled={!editing?.proxyConfigured && !form.proxyURL?.trim()} onCheckedChange={(proxyPool) => setForm({ ...form, proxyPool })} />
            </div>
            {form.scope !== "grok_build" && clearanceMode === "manual" ? (
              <Field label={t("settings.egress.userAgent")} controlId="egress-user-agent">
                <Input id="egress-user-agent" value={form.userAgent} onChange={(event) => setForm({ ...form, userAgent: event.target.value })} />
              </Field>
            ) : null}
            {form.scope !== "grok_build" && clearanceMode === "manual" ? (
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

function ClearanceBadge({ node, clearanceMode }: { node: EgressNodeDTO; clearanceMode: "manual" | "flaresolverr" }) {
  const { t } = useTranslation();
  if (node.scope === "grok_build") return <span className="text-xs text-muted-foreground">—</span>;
  if (clearanceMode === "flaresolverr") {
    return <Badge variant="secondary" className="text-[10px]">{node.accountBoundProxy ? `${t("settings.web.clearanceFlareSolverr")} · Resin` : t("settings.web.clearanceFlareSolverr")}</Badge>;
  }
  return <Badge variant={node.cookieConfigured ? "secondary" : "outline"} className={cn("text-[10px]", !node.cookieConfigured && "text-muted-foreground")}>{node.cookieConfigured ? t("settings.egress.configured") : t("settings.egress.none")}</Badge>;
}

function HealthMeter({ value }: { value: number }) {
  const percent = Math.max(0, Math.min(100, Math.round(value * 100)));
  return (
    <div className="mx-auto flex w-24 items-center gap-2">
      <div className="h-1.5 min-w-0 flex-1 overflow-hidden rounded-full bg-muted">
        <div className={cn("h-full rounded-full transition-[width]", percent >= 70 ? "bg-emerald-500" : percent >= 35 ? "bg-amber-500" : "bg-destructive")} style={{ width: `${percent}%` }} />
      </div>
      <span className="w-8 text-right text-[11px] tabular-nums text-muted-foreground">{percent}%</span>
    </div>
  );
}

function ProbeSummary({ node }: { node: EgressNodeDTO }) {
  const { t } = useTranslation();
  const healthy = node.probeStatus === "healthy";
  const unhealthy = node.probeStatus === "unhealthy";
  return (
    <div className="flex min-w-0 items-center justify-center gap-2 text-xs">
      <span className={cn("size-1.5 shrink-0 rounded-full", healthy ? "bg-emerald-500" : unhealthy ? "bg-destructive" : "bg-muted-foreground/35")} />
      <span className={cn("truncate", healthy ? "text-foreground" : unhealthy ? "text-destructive" : "text-muted-foreground")}>
        {healthy ? node.exitIp || t("settings.egress.healthy") : unhealthy ? t("settings.egress.unhealthy") : t("settings.egress.notTested")}
      </span>
      {healthy ? <span className="shrink-0 tabular-nums text-muted-foreground">{node.probeLatencyMs} ms</span> : null}
      {unhealthy && node.probeError ? <ErrorTooltip message={node.probeError} /> : null}
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
