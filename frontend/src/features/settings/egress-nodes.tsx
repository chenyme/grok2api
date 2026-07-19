import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { MoreHorizontal, Pencil, Plus, Trash2 } from "lucide-react";
import { type ReactNode, useState } from "react";
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
import { Table, TableActionCell, TableActionHead, TableBody, TableCell, TableHeader, TableRow } from "@/components/ui/table";
import { createEgressNode, deleteEgressNode, listEgressNodes, updateEgressNode, type EgressNodeDTO, type EgressNodeInput, type EgressScope } from "@/features/settings/settings-api";
import { SortableTableHead } from "@/shared/components/sortable-table-head";
import { ErrorState } from "@/shared/components/data-state";
import { nextTableSort, type SortOrder, type TableSort } from "@/shared/lib/table-sort";

const emptyInput: EgressNodeInput = { name: "", scope: "grok_build", enabled: true, proxyURL: "", userAgent: "", cloudflareCookies: "" };

export function EgressNodes() {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [editing, setEditing] = useState<EgressNodeDTO | null | undefined>(undefined);
  const [form, setForm] = useState<EgressNodeInput>(emptyInput);
  const [sort, setSort] = useState<TableSort>({ field: "", order: "asc" });
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
    onSuccess: () => { void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] }); toast.success(t("settings.egress.deleted")); },
    onError: (error) => showError(error, t("settings.egress.operationFailed")),
  });

  function openCreate() {
    setForm(emptyInput);
    setEditing(null);
  }

  function openEdit(node: EgressNodeDTO) {
    setForm({ name: node.name, scope: node.scope, enabled: node.enabled, userAgent: node.scope === "grok_build" ? "" : node.userAgent, proxyURL: "", cloudflareCookies: "" });
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

  const nodes = query.data?.items ?? [];
  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between gap-3">
        <p className="text-xs text-muted-foreground">{t("console.egressDescription")}</p>
        <Button type="button" size="sm" variant="secondary" onClick={openCreate}><Plus />{t("settings.egress.add")}</Button>
      </div>
      {query.isError ? <ErrorState message={query.error.message} onRetry={() => void query.refetch()} /> : <div className="overflow-hidden rounded-md border">
        <Table>
          <TableHeader><TableRow><SortableTableHead field="name" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("settings.egress.name")}</SortableTableHead><SortableTableHead field="scope" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort}>{t("settings.egress.scope")}</SortableTableHead><SortableTableHead field="proxy" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort}>{t("settings.egress.proxy")}</SortableTableHead><SortableTableHead field="clearance" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort}>{t("settings.egress.clearance")}</SortableTableHead><SortableTableHead field="health" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" align="center" onSort={changeSort}>{t("settings.egress.health")}</SortableTableHead><TableActionHead /></TableRow></TableHeader>
          <TableBody>
            {nodes.length === 0 ? <TableRow><TableCell colSpan={6} className="h-20 text-center text-xs text-muted-foreground">{t("settings.egress.directFallback")}</TableCell></TableRow> : nodes.map((node) => (
              <TableRow className="group" key={node.id}>
                <TableCell><div className="text-xs font-medium">{node.name}</div>{node.lastError ? <div className="mt-0.5 max-w-72 truncate text-[11px] text-destructive">{node.lastError}</div> : null}</TableCell>
                <TableCell className="text-center"><Badge variant="secondary" className="text-[10px]">{scopeLabel(node.scope)}</Badge></TableCell>
                <TableCell className="text-center text-xs text-muted-foreground">{node.proxyConfigured ? t("settings.egress.configured") : t("settings.egress.direct")}</TableCell>
                <TableCell className="text-center text-xs text-muted-foreground">{node.cookieConfigured ? t("settings.egress.configured") : t("settings.egress.none")}</TableCell>
                <TableCell className="text-center text-xs tabular-nums">{Math.round(node.health * 100)}%</TableCell>
                <TableActionCell>
                  <DropdownMenu><DropdownMenuTrigger asChild><Button type="button" variant="ghost" size="icon" className="size-8" aria-label={t("common.actions")}><MoreHorizontal /></Button></DropdownMenuTrigger><DropdownMenuContent align="end">
                    <DropdownMenuItem onClick={() => openEdit(node)}><Pencil />{t("common.edit")}</DropdownMenuItem><DropdownMenuSeparator /><DropdownMenuItem className="text-destructive focus:text-destructive" onClick={() => remove.mutate(node.id)}><Trash2 />{t("common.delete")}</DropdownMenuItem>
                  </DropdownMenuContent></DropdownMenu>
                </TableActionCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>}

      <Dialog open={editing !== undefined} onOpenChange={(open) => { if (!open) setEditing(undefined); }}>
        <DialogContent className="max-h-[calc(100svh-2rem)] overflow-y-auto sm:max-w-[520px]">
          <DialogHeader className="pr-8">
            <DialogTitle>{editing ? t("settings.egress.editTitle") : t("settings.egress.addTitle")}</DialogTitle>
            <DialogDescription>{t("console.egressDialogDescription")}</DialogDescription>
          </DialogHeader>
          <form className="space-y-4" onSubmit={(event) => { event.preventDefault(); save.mutate(); }}>
            <div className="flex items-center justify-between gap-4 rounded-md bg-muted/45 px-3 py-2.5">
              <Label htmlFor="egress-enabled">{t("settings.egress.enabled")}</Label>
              <Switch id="egress-enabled" checked={form.enabled} onCheckedChange={(enabled) => setForm({ ...form, enabled })} />
            </div>
            <Field label={t("settings.egress.name")} controlId="egress-name">
              <Input id="egress-name" value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} />
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
            <Field label={t("settings.egress.proxyURL")} controlId="egress-proxy" description={t("settings.egress.proxyProtocols")}>
              <Input id="egress-proxy" type="password" autoComplete="new-password" placeholder={editing?.proxyConfigured ? t("settings.egress.keepConfigured") : "socks5h://user:pass@host:port"} value={form.proxyURL} onChange={(event) => setForm({ ...form, proxyURL: event.target.value })} />
            </Field>
            {form.scope !== "grok_build" ? (
              <Field label={t("settings.egress.userAgent")} controlId="egress-user-agent">
                <Input id="egress-user-agent" value={form.userAgent} onChange={(event) => setForm({ ...form, userAgent: event.target.value })} />
              </Field>
            ) : null}
            {form.scope !== "grok_build" ? (
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

function Field({ label, controlId, description, children }: { label: string; controlId: string; description?: string; children: ReactNode }) {
  return <div className="space-y-2"><Label htmlFor={controlId}>{label}</Label>{children}{description ? <p className="whitespace-pre-line text-xs leading-5 text-muted-foreground">{description}</p> : null}</div>;
}

function showError(error: unknown, fallback: string) {
  toast.error(error instanceof Error ? error.message : fallback);
}
