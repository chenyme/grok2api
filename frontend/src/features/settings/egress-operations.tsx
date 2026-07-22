import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Download, Network, Pencil, RefreshCw, Shuffle, Trash2, Upload } from "lucide-react";
import { useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { Table, TableActionCell, TableActionHead, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Textarea } from "@/components/ui/textarea";
import {
  createEgressSource,
  deleteEgressSource,
  getEgressOperationsConfig,
  importEgressText,
  listEgressSources,
  rebalanceEgressAccounts,
  syncEgressSource,
  testEgressNodes,
  updateEgressOperationsConfig,
  updateEgressSource,
  type EgressOperationsConfigDTO,
  type EgressScope,
  type EgressSourceDTO,
  type EgressSourceInput,
} from "@/features/settings/settings-api";
import { formatDateTime } from "@/shared/lib/format";

type SourceForm = EgressSourceInput & { url: string };
type ImportForm = { name: string; scope: EgressScope; accountCapacity: number; content: string };

const emptySource: SourceForm = {
  name: "", scope: "grok_build", enabled: true, url: "", refreshIntervalSeconds: 900, defaultAccountCapacity: 0,
};
const emptyImport: ImportForm = { name: "", scope: "grok_build", accountCapacity: 0, content: "" };
const defaultOperationsForm: Omit<EgressOperationsConfigDTO, "updatedAt"> = {
  probeIntervalSeconds: 900, autoAssignEnabled: false, autoBalanceEnabled: false, assignmentIntervalSeconds: 300,
};

function operationsFormFrom(value?: EgressOperationsConfigDTO): Omit<EgressOperationsConfigDTO, "updatedAt"> {
  if (!value) return defaultOperationsForm;
  return {
    probeIntervalSeconds: value.probeIntervalSeconds,
    autoAssignEnabled: value.autoAssignEnabled,
    autoBalanceEnabled: value.autoBalanceEnabled,
    assignmentIntervalSeconds: value.assignmentIntervalSeconds,
  };
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
    mutationFn: () => testEgressNodes(),
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
    onSuccess: (value) => { setOperationsDraft(operationsFormFrom(value)); invalidate(); toast.success(t("settings.egress.automationSaved")); },
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

  return (
    <section className="space-y-3 border-t pt-5">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="text-sm font-medium">{t("settings.egress.operations")}</h3>
        <div className="flex flex-wrap items-center gap-1.5">
          <Button type="button" size="sm" variant="secondary" disabled={testAll.isPending} onClick={() => testAll.mutate()}>{testAll.isPending ? <Spinner /> : <Network />}{t("settings.egress.testAll")}</Button>
          <Button type="button" size="sm" variant="secondary" disabled={rebalance.isPending} onClick={() => rebalance.mutate()}>{rebalance.isPending ? <Spinner /> : <Shuffle />}{t("settings.egress.rebalance")}</Button>
          <Button type="button" size="sm" variant="secondary" onClick={() => { setImportForm(emptyImport); setImportOpen(true); }}><Upload />{t("settings.egress.importText")}</Button>
          <Button type="button" size="sm" variant="secondary" onClick={() => openSource()}><Download />{t("settings.egress.addSource")}</Button>
        </div>
      </div>

      <div className="grid gap-3 border px-3 py-3 sm:grid-cols-2 xl:grid-cols-4">
        <Control label={t("settings.egress.probeInterval")}>
          <Input type="number" min={60} max={86400} value={operationsForm.probeIntervalSeconds} onChange={(event) => setOperationsDraft({ ...operationsForm, probeIntervalSeconds: Number(event.target.value) })} />
        </Control>
        <Control label={t("settings.egress.assignmentInterval")}>
          <Input type="number" min={60} max={86400} value={operationsForm.assignmentIntervalSeconds} onChange={(event) => setOperationsDraft({ ...operationsForm, assignmentIntervalSeconds: Number(event.target.value) })} />
        </Control>
        <ToggleControl label={t("settings.egress.autoAssign")} checked={operationsForm.autoAssignEnabled} onChange={(autoAssignEnabled) => setOperationsDraft({ ...operationsForm, autoAssignEnabled })} />
        <div className="flex items-center justify-between gap-3">
          <ToggleControl label={t("settings.egress.autoBalance")} checked={operationsForm.autoBalanceEnabled} onChange={(autoBalanceEnabled) => setOperationsDraft({ ...operationsForm, autoBalanceEnabled })} />
          <Button type="button" size="sm" disabled={saveOperations.isPending} onClick={() => saveOperations.mutate()}>{saveOperations.isPending ? <Spinner /> : null}{t("common.save")}</Button>
        </div>
      </div>

      <div className="overflow-hidden border">
        <Table>
          <TableHeader><TableRow><TableHead>{t("settings.egress.source")}</TableHead><TableHead>{t("settings.egress.scope")}</TableHead><TableHead>{t("settings.egress.sync")}</TableHead><TableHead>{t("settings.egress.capacity")}</TableHead><TableActionHead /></TableRow></TableHeader>
          <TableBody>
            {sourcesQuery.isPending ? <TableRow><TableCell colSpan={5} className="h-16 text-center"><Spinner /></TableCell></TableRow> : null}
            {!sourcesQuery.isPending && (sourcesQuery.data?.items.length ?? 0) === 0 ? <TableRow><TableCell colSpan={5} className="h-16 text-center text-xs text-muted-foreground">{t("settings.egress.noSources")}</TableCell></TableRow> : null}
            {sourcesQuery.data?.items.map((source) => (
              <TableRow key={source.id}>
                <TableCell><div className="text-xs font-medium">{source.name}</div>{source.lastSyncError ? <div className="mt-0.5 max-w-72 truncate text-[11px] text-destructive">{source.lastSyncError}</div> : null}</TableCell>
                <TableCell><Badge variant="secondary" className="text-[10px]">{scopeLabel(source.scope)}</Badge></TableCell>
                <TableCell className="text-xs text-muted-foreground">{source.lastSyncedAt ? formatDateTime(source.lastSyncedAt, i18n.language) : t("settings.egress.never")}</TableCell>
                <TableCell className="text-xs tabular-nums">{source.defaultAccountCapacity || t("settings.egress.unlimited")}</TableCell>
                <TableActionCell className="whitespace-nowrap">
                  <Button type="button" size="icon" variant="ghost" className="size-8" disabled={syncSource.isPending} onClick={() => syncSource.mutate(source.id)} aria-label={t("settings.egress.sync")} title={t("settings.egress.sync")}><RefreshCw /></Button>
                  <Button type="button" size="icon" variant="ghost" className="size-8" onClick={() => openSource(source)} aria-label={t("common.edit")} title={t("common.edit")}><Pencil /></Button>
                  <Button type="button" size="icon" variant="ghost" className="size-8 text-destructive hover:text-destructive" disabled={removeSource.isPending} onClick={() => removeSource.mutate(source.id)} aria-label={t("common.delete")} title={t("common.delete")}><Trash2 /></Button>
                </TableActionCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>

      <Dialog open={sourceEditing !== undefined} onOpenChange={(open) => { if (!open) setSourceEditing(undefined); }}>
        <DialogContent className="sm:max-w-[520px]">
          <DialogHeader><DialogTitle>{sourceEditing ? t("settings.egress.editSource") : t("settings.egress.addSource")}</DialogTitle></DialogHeader>
          <form className="space-y-3.5" onSubmit={(event) => { event.preventDefault(); saveSource.mutate(); }}>
            <ToggleControl label={t("settings.egress.enabled")} checked={sourceForm.enabled} onChange={(enabled) => setSourceForm({ ...sourceForm, enabled })} />
            <Control label={t("settings.egress.name")}><Input value={sourceForm.name} onChange={(event) => setSourceForm({ ...sourceForm, name: event.target.value })} /></Control>
            <Control label={t("settings.egress.scope")}><ScopeSelect value={sourceForm.scope} onChange={(scope) => setSourceForm({ ...sourceForm, scope })} scopeLabel={scopeLabel} /></Control>
            <Control label={t("settings.egress.subscriptionURL")}><Input type="password" autoComplete="new-password" placeholder={sourceEditing?.urlConfigured ? t("settings.egress.keepConfigured") : "https://..."} value={sourceForm.url} onChange={(event) => setSourceForm({ ...sourceForm, url: event.target.value })} /></Control>
            <div className="grid grid-cols-2 gap-3">
              <Control label={t("settings.egress.refreshInterval")}><Input type="number" min={60} max={86400} value={sourceForm.refreshIntervalSeconds} onChange={(event) => setSourceForm({ ...sourceForm, refreshIntervalSeconds: Number(event.target.value) })} /></Control>
              <Control label={t("settings.egress.capacity")}><Input type="number" min={0} max={100000} value={sourceForm.defaultAccountCapacity} onChange={(event) => setSourceForm({ ...sourceForm, defaultAccountCapacity: Number(event.target.value) })} /></Control>
            </div>
            <DialogFooter><Button type="button" size="sm" variant="secondary" onClick={() => setSourceEditing(undefined)}>{t("common.cancel")}</Button><Button type="submit" size="sm" disabled={!sourceForm.name.trim() || (!sourceEditing && !sourceForm.url.trim()) || saveSource.isPending}>{saveSource.isPending ? <Spinner /> : null}{t("common.save")}</Button></DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      <Dialog open={importOpen} onOpenChange={setImportOpen}>
        <DialogContent className="sm:max-w-[620px]">
          <DialogHeader><DialogTitle>{t("settings.egress.importText")}</DialogTitle></DialogHeader>
          <form className="space-y-3.5" onSubmit={(event) => { event.preventDefault(); importText.mutate(); }}>
            <div className="grid grid-cols-2 gap-3"><Control label={t("settings.egress.name")}><Input value={importForm.name} onChange={(event) => setImportForm({ ...importForm, name: event.target.value })} /></Control><Control label={t("settings.egress.scope")}><ScopeSelect value={importForm.scope} onChange={(scope) => setImportForm({ ...importForm, scope })} scopeLabel={scopeLabel} /></Control></div>
            <Control label={t("settings.egress.capacity")}><Input type="number" min={0} max={100000} value={importForm.accountCapacity} onChange={(event) => setImportForm({ ...importForm, accountCapacity: Number(event.target.value) })} /></Control>
            <Control label={t("settings.egress.proxyList")}><Textarea className="min-h-52 font-mono text-xs" value={importForm.content} onChange={(event) => setImportForm({ ...importForm, content: event.target.value })} /></Control>
            <DialogFooter><Button type="button" size="sm" variant="secondary" onClick={() => setImportOpen(false)}>{t("common.cancel")}</Button><Button type="submit" size="sm" disabled={!importForm.name.trim() || !importForm.content.trim() || importText.isPending}>{importText.isPending ? <Spinner /> : null}{t("settings.egress.importText")}</Button></DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </section>
  );
}

function ScopeSelect({ value, onChange, scopeLabel }: { value: EgressScope; onChange: (value: EgressScope) => void; scopeLabel: (scope: EgressScope) => string }) {
  return <Select value={value} onValueChange={(next) => onChange(next as EgressScope)}><SelectTrigger><SelectValue /></SelectTrigger><SelectContent>{(["grok_build", "grok_web", "grok_console", "grok_web_asset"] as EgressScope[]).map((scope) => <SelectItem key={scope} value={scope}>{scopeLabel(scope)}</SelectItem>)}</SelectContent></Select>;
}

function Control({ label, children }: { label: string; children: ReactNode }) {
  return <div className="space-y-1.5"><Label>{label}</Label>{children}</div>;
}

function ToggleControl({ label, checked, onChange }: { label: string; checked: boolean; onChange: (value: boolean) => void }) {
  return <div className="flex min-h-10 items-center justify-between gap-3"><Label>{label}</Label><Switch checked={checked} onCheckedChange={onChange} /></div>;
}

function showError(error: unknown) {
  toast.error(error instanceof Error ? error.message : "Operation failed");
}
