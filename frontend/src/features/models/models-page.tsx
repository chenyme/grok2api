import { zodResolver } from "@hookform/resolvers/zod";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { MoreHorizontal, Pencil, Plus, RefreshCw, Search, Trash2 } from "lucide-react";
import { useState } from "react";
import { useForm, useWatch } from "react-hook-form";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import { z } from "zod";

import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Spinner } from "@/components/ui/spinner";
import { Table, TableActionCell, TableActionHead, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { createModel, deleteModel, deleteModels, listModelAccountOptions, listModels, syncModels, updateModel, updateModelsEnabled } from "@/entities/model/model-api";
import type { ModelRouteDTO } from "@/entities/model/types";
import { EmptyState, ErrorState, TableLoadingRow } from "@/shared/components/data-state";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { DataTableFilters } from "@/shared/components/data-table-filters";
import { Pagination } from "@/shared/components/pagination";
import { SortableTableHead } from "@/shared/components/sortable-table-head";
import { VirtualTableBody } from "@/shared/components/virtual-table-body";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { cn } from "@/shared/lib/cn";
import { formatDateTime } from "@/shared/lib/format";
import { nextTableSort, type SortOrder, type TableSort } from "@/shared/lib/table-sort";

export function ModelsPage() {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [search, setSearch] = useState("");
  const [statusFilter, setStatusFilter] = useState("");
  const [providerFilter, setProviderFilter] = useState<ModelRouteDTO["provider"] | "">("");
  const [sort, setSort] = useState<TableSort>({ field: "", order: "asc" });
  const [selected, setSelected] = useState<Set<string>>(() => new Set());
  const [editing, setEditing] = useState<ModelRouteDTO | "new" | null>(null);
  const [deleting, setDeleting] = useState<ModelRouteDTO | null>(null);
  const [batchDeleteOpen, setBatchDeleteOpen] = useState(false);
  const [accountSearch, setAccountSearch] = useState("");
  const debouncedSearch = useDebouncedValue(search);
  const schema = z.object({
    publicId: z.string().min(1, t("errors.required")),
    provider: z.enum(["grok_build", "grok_web", "grok_console"]),
    upstreamModel: z.string().min(1, t("errors.required")),
    capability: z.enum(["responses", "chat", "image", "image_edit", "video"]),
    enabled: z.boolean(),
    bindingMode: z.boolean(),
    accountIds: z.array(z.string()),
  }).refine((value) => !value.bindingMode || value.accountIds.length > 0, { path: ["accountIds"], message: t("models.selectAccountRequired") });
  type ModelForm = z.infer<typeof schema>;
  const form = useForm<ModelForm>({
    resolver: zodResolver(schema),
    defaultValues: { publicId: "", provider: "grok_build", upstreamModel: "", capability: "responses", enabled: true, bindingMode: false, accountIds: [] },
  });
  const modelEnabled = useWatch({ control: form.control, name: "enabled" });
  const selectedProvider = useWatch({ control: form.control, name: "provider" });
  const selectedCapability = useWatch({ control: form.control, name: "capability" });
  const bindingMode = useWatch({ control: form.control, name: "bindingMode" });
  const selectedAccountIDs = useWatch({ control: form.control, name: "accountIds" });

  const modelsQuery = useQuery({
    queryKey: ["models", page, pageSize, debouncedSearch, statusFilter, providerFilter, sort.field, sort.order],
    queryFn: () => listModels({ page, pageSize, search: debouncedSearch, status: statusFilter, provider: providerFilter, sortBy: sort.field || undefined, sortOrder: sort.field ? sort.order : undefined }),
  });

  const accountOptionsQuery = useQuery({
    queryKey: ["models", "account-options", selectedProvider],
    queryFn: () => listModelAccountOptions(selectedProvider),
    enabled: editing !== null,
  });

  const updateMutation = useMutation({
    mutationFn: (values: ModelForm) => {
      if (!editing) throw new Error(t("errors.generic"));
      const input = { ...values, accountIds: values.bindingMode ? values.accountIds : [] };
      if (editing === "new") return createModel(input);
      return updateModel(editing.id, { publicId: input.publicId, enabled: input.enabled, accountIds: input.accountIds });
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["models"] });
      setEditing(null);
      toast.success(t(editing === "new" ? "models.created" : "models.updated"));
    },
    onError: showError,
  });

  const deleteMutation = useMutation({
    mutationFn: (id: string) => deleteModel(id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["models"] });
      setDeleting(null);
      toast.success(t("models.deleted"));
    },
    onError: showError,
  });

  const batchDeleteMutation = useMutation({
    mutationFn: () => deleteModels([...selected]),
    onSuccess: (result) => {
      setSelected(new Set());
      setBatchDeleteOpen(false);
      void queryClient.invalidateQueries({ queryKey: ["models"] });
      toast.success(t("models.batchDeleted", { count: result.deleted }));
    },
    onError: showError,
  });

  const batchUpdateMutation = useMutation({
    mutationFn: (enabled: boolean) => updateModelsEnabled([...selected], enabled),
    onSuccess: () => {
      setSelected(new Set());
      void queryClient.invalidateQueries({ queryKey: ["models"] });
      toast.success(t("models.batchUpdated"));
    },
    onError: showError,
  });

  const syncMutation = useMutation({
    mutationFn: syncModels,
    onSuccess: (result) => {
      void queryClient.invalidateQueries({ queryKey: ["models"] });
      toast.success(t("models.synced", { count: result.synced }));
    },
    onError: showError,
  });

  function showError(error: unknown): void {
    toast.error(error instanceof Error ? error.message : t("errors.generic"));
  }

  function beginEdit(model: ModelRouteDTO): void {
    setEditing(model);
    setAccountSearch("");
    form.reset({
      publicId: model.publicId,
      provider: model.provider,
      upstreamModel: model.upstreamModel,
      capability: model.capability,
      enabled: model.enabled,
      bindingMode: model.bindingMode,
      accountIds: model.accountIds,
    });
  }

  function beginCreate(): void {
    setEditing("new");
    setAccountSearch("");
    form.reset({ publicId: "", provider: "grok_build", upstreamModel: "", capability: "responses", enabled: true, bindingMode: false, accountIds: [] });
  }

  function toggleBoundAccount(id: string, checked: boolean): void {
    const current = form.getValues("accountIds");
    form.setValue("accountIds", checked ? [...new Set([...current, id])] : current.filter((value) => value !== id), { shouldValidate: true });
  }

  const accountOptions = accountOptionsQuery.data?.items ?? [];
  const normalizedAccountSearch = accountSearch.trim().toLocaleLowerCase();
  const visibleAccountOptions = normalizedAccountSearch
    ? accountOptions.filter((account) => account.name.toLocaleLowerCase().includes(normalizedAccountSearch) || account.id.includes(normalizedAccountSearch))
    : accountOptions;

  const result = modelsQuery.data;
  const pageIDs = result?.items.map((model) => model.id) ?? [];
  const selectedOnPage = pageIDs.filter((id) => selected.has(id));
  const allPageSelected = pageIDs.length > 0 && selectedOnPage.length === pageIDs.length;

  function togglePage(checked: boolean): void {
    setSelected((current) => {
      const next = new Set(current);
      for (const id of pageIDs) {
        if (checked) next.add(id);
        else next.delete(id);
      }
      return next;
    });
  }

  function toggleModel(id: string, checked: boolean): void {
    setSelected((current) => {
      const next = new Set(current);
      if (checked) next.add(id);
      else next.delete(id);
      return next;
    });
  }

  function changeSort(field: string, initialOrder: SortOrder): void {
    setSort((current) => nextTableSort(current, field, initialOrder));
    setPage(1);
  }

  return (
    <div className="space-y-5">
      <header className="flex min-h-8 items-center">
        <h1 className="text-xl font-medium">{t("models.title")}</h1>
        <p className="sr-only">{t("models.description")}</p>
      </header>

      <DataTableShell
        toolbar={(
          <>
            <div className="flex w-full items-center gap-2 sm:w-auto">
              <div className="relative min-w-0 flex-1 sm:w-64 sm:flex-none">
                <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                <Input className="h-8 pl-9 text-xs" value={search} onChange={(event) => { setSearch(event.target.value); setPage(1); }} placeholder={t("models.search")} aria-label={t("models.search")} />
              </div>
              <DataTableFilters filters={[
                { id: "provider", label: t("models.provider"), value: providerFilter, onChange: (value) => { setProviderFilter(value as ModelRouteDTO["provider"] | ""); setPage(1); }, options: [
                  { value: "grok_build", label: t("models.providerGrokBuild") },
                  { value: "grok_web", label: t("models.providerGrokWeb") },
                  { value: "grok_console", label: t("console.name") },
                ] },
                { id: "status", label: t("models.status"), value: statusFilter, onChange: (value) => { setStatusFilter(value); setPage(1); }, options: [
                  { value: "enabled", label: t("common.enabled") },
                  { value: "disabled", label: t("common.disabled") },
                ] },
              ]} />
            </div>
            <div className="flex flex-wrap items-center gap-1.5">
              {selected.size > 0 ? (
                <>
                  <span className="mr-1 text-xs text-muted-foreground">{t("common.selectedCount", { count: selected.size })}</span>
                  <Button variant="secondary" size="sm" onClick={() => batchUpdateMutation.mutate(true)}>{t("common.enable")}</Button>
                  <Button variant="secondary" size="sm" onClick={() => batchUpdateMutation.mutate(false)}>{t("common.disable")}</Button>
                  <Button variant="secondary" size="sm" className="text-destructive hover:text-destructive" onClick={() => setBatchDeleteOpen(true)}>{t("common.delete")}</Button>
                </>
              ) : null}
              <Button variant="secondary" size="sm" disabled={syncMutation.isPending} onClick={() => syncMutation.mutate()}>
                {syncMutation.isPending ? <Spinner /> : <RefreshCw />}
                {t("models.sync")}
              </Button>
              <Button size="sm" onClick={beginCreate}><Plus />{t("models.create")}</Button>
            </div>
          </>
        )}
        footer={result && result.total > 0 ? <Pagination page={result.page} pageSize={result.pageSize} total={result.total} onPageChange={setPage} onPageSizeChange={(value) => { setPageSize(value); setPage(1); }} /> : undefined}
      >
        {modelsQuery.isError ? <ErrorState message={modelsQuery.error.message} onRetry={() => void modelsQuery.refetch()} /> : null}
        {result && result.items.length === 0 ? <EmptyState /> : null}
        {modelsQuery.isPending || (result && result.items.length > 0) ? (
          <Table viewportRows={20} rowHeight={56} className="min-w-[1000px] table-fixed text-xs">
            <colgroup>
              <col className="w-10" />
              <col className="w-56" />
              <col className="w-52" />
              <col className="w-24" />
              <col className="w-32" />
              <col className="w-40" />
              <col className="w-44" />
              <col className="w-10" />
            </colgroup>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="px-2 text-center"><Checkbox checked={allPageSelected ? true : selectedOnPage.length > 0 ? "indeterminate" : false} onCheckedChange={(checked) => togglePage(checked === true)} aria-label={t("common.selectPage")} /></TableHead>
                <SortableTableHead field="publicId" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("models.model")}</SortableTableHead>
                <SortableTableHead field="upstreamModel" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("models.upstream")}</SortableTableHead>
                <SortableTableHead field="status" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort}>{t("models.status")}</SortableTableHead>
                <SortableTableHead field="provider" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort}>{t("models.provider")}</SortableTableHead>
                <SortableTableHead field="accountSupport" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" align="center" onSort={changeSort}>{t("models.accountSupport")}</SortableTableHead>
                <SortableTableHead field="lastSyncedAt" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("models.lastSyncedAt")}</SortableTableHead>
                <TableActionHead />
              </TableRow>
            </TableHeader>
            {modelsQuery.isPending ? (
              <TableBody><TableLoadingRow colSpan={8} /></TableBody>
            ) : (
              <VirtualTableBody items={result?.items ?? []} colSpan={8} rowHeight={56} renderRow={(model) => (
                <TableRow className="group h-14" key={model.id} data-state={selected.has(model.id) ? "selected" : undefined}>
                  <TableCell className="px-2 text-center"><Checkbox checked={selected.has(model.id)} onCheckedChange={(checked) => toggleModel(model.id, checked === true)} aria-label={t("common.selectItem", { name: model.publicId })} /></TableCell>
                  <TableCell className="min-w-0">
                    <span className="block truncate text-xs font-medium" title={model.publicId}>{model.publicId}</span>
                  </TableCell>
                  <TableCell className="min-w-0">
                    <span className="block truncate text-xs text-muted-foreground" title={model.upstreamModel}>{model.upstreamModel}</span>
                  </TableCell>
                  <TableCell className="text-center">{model.enabled ? <Badge variant="secondary" className="bg-emerald-500/10 text-emerald-700 dark:text-emerald-300">{t("common.enabled")}</Badge> : <Badge variant="outline" className="text-muted-foreground">{t("common.disabled")}</Badge>}</TableCell>
                  <TableCell className="text-center"><ModelProvider provider={model.provider} /></TableCell>
                  <TableCell className="text-center text-xs">
                    <div title={t("models.supportSummary", { supported: model.supportedAccounts, total: model.totalAccounts })}>
                      <span className="inline-flex items-baseline gap-1 tabular-nums"><span className={cn("font-medium", model.supportedAccounts > 0 ? "text-emerald-600 dark:text-emerald-400" : "text-muted-foreground")}>{model.supportedAccounts}</span><span className="text-muted-foreground">/ {model.totalAccounts}</span></span>
                      {model.bindingMode ? <span className="mt-0.5 block text-[10px] text-muted-foreground">{t("models.boundAccounts")}</span> : null}
                    </div>
                  </TableCell>
                  <TableCell className="whitespace-nowrap text-xs text-muted-foreground">{formatDateTime(model.lastSyncedAt, i18n.language)}</TableCell>
                  <TableActionCell>
                    <DropdownMenu>
                      <DropdownMenuTrigger asChild><Button type="button" variant="ghost" size="icon" className="size-8" aria-label={t("common.actions")}><MoreHorizontal /></Button></DropdownMenuTrigger>
                      <DropdownMenuContent align="end">
                        <DropdownMenuItem onClick={() => beginEdit(model)}><Pencil />{t("common.edit")}</DropdownMenuItem>
                        <DropdownMenuItem className="text-destructive focus:text-destructive" onClick={() => setDeleting(model)}><Trash2 />{t("common.delete")}</DropdownMenuItem>
                      </DropdownMenuContent>
                    </DropdownMenu>
                  </TableActionCell>
                </TableRow>
              )} />
            )}
          </Table>
        ) : null}
      </DataTableShell>

      <Dialog open={Boolean(editing)} onOpenChange={(open) => !open && setEditing(null)}>
        <DialogContent className="max-w-2xl">
          <DialogHeader>
            <DialogTitle>{t(editing === "new" ? "models.createTitle" : "models.editTitle")}</DialogTitle>
            <DialogDescription className={editing === "new" ? undefined : "font-mono"}>{editing === "new" ? t("models.createDescription") : editing?.upstreamModel}</DialogDescription>
          </DialogHeader>
          <form className="space-y-4" onSubmit={form.handleSubmit((values) => updateMutation.mutate(values))}>
            <div className="space-y-2"><Label htmlFor="model-public-id">{t("models.publicId")}</Label><Input id="model-public-id" className="font-mono" {...form.register("publicId")} />{form.formState.errors.publicId ? <p className="text-xs text-destructive">{form.formState.errors.publicId.message}</p> : null}</div>
            {editing === "new" ? (
              <div className="grid gap-4 sm:grid-cols-2">
                <div className="space-y-2">
                  <Label>{t("models.provider")}</Label>
                  <Select value={selectedProvider} disabled>
                    <SelectTrigger><SelectValue /></SelectTrigger>
                    <SelectContent><SelectItem value="grok_build">{t("models.providerGrokBuild")}</SelectItem></SelectContent>
                  </Select>
                </div>
                <div className="space-y-2">
                  <Label>{t("models.capability")}</Label>
                  <Select value={selectedCapability} disabled>
                    <SelectTrigger><SelectValue /></SelectTrigger>
                    <SelectContent><SelectItem value="responses">Responses</SelectItem></SelectContent>
                  </Select>
                </div>
                <div className="space-y-2 sm:col-span-2"><Label htmlFor="model-upstream-id">{t("models.upstream")}</Label><Input id="model-upstream-id" className="font-mono" {...form.register("upstreamModel")} />{form.formState.errors.upstreamModel ? <p className="text-xs text-destructive">{form.formState.errors.upstreamModel.message}</p> : null}</div>
              </div>
            ) : null}
            <div className="rounded-md border">
              <div className="flex items-center justify-between gap-4 p-3">
                <div><Label htmlFor="model-binding-mode">{t("models.bindAccounts")}</Label><p className="mt-1 text-xs text-muted-foreground">{t("models.bindAccountsDescription")}</p></div>
                <Switch id="model-binding-mode" checked={bindingMode} onCheckedChange={(checked) => { form.setValue("bindingMode", checked); if (!checked) form.clearErrors("accountIds"); }} />
              </div>
              {bindingMode ? (
                <div className="border-t p-3">
                  <Input className="mb-2" value={accountSearch} onChange={(event) => setAccountSearch(event.target.value)} placeholder={t("models.searchAccounts")} />
                  <div className="max-h-56 overflow-y-auto rounded-md border">
                    {accountOptionsQuery.isPending ? <div className="flex items-center justify-center p-6"><Spinner /></div> : null}
                    {accountOptionsQuery.isError ? <p className="p-3 text-xs text-destructive">{accountOptionsQuery.error.message}</p> : null}
                    {!accountOptionsQuery.isPending && visibleAccountOptions.length === 0 ? <p className="p-3 text-xs text-muted-foreground">{t("models.noBindableAccounts")}</p> : null}
                    {visibleAccountOptions.map((account) => (
                      <label key={account.id} className="flex cursor-pointer items-center gap-3 border-b px-3 py-2 last:border-b-0 hover:bg-muted/40">
                        <Checkbox checked={selectedAccountIDs.includes(account.id)} onCheckedChange={(checked) => toggleBoundAccount(account.id, checked === true)} />
                        <span className="min-w-0 flex-1 truncate text-xs">{account.name}</span>
                        <span className="font-mono text-[11px] text-muted-foreground">#{account.id}</span>
                      </label>
                    ))}
                  </div>
                  <div className="mt-2 flex items-center justify-between text-xs text-muted-foreground"><span>{t("models.selectedAccounts", { count: selectedAccountIDs.length })}</span>{form.formState.errors.accountIds ? <span className="text-destructive">{form.formState.errors.accountIds.message}</span> : null}</div>
                </div>
              ) : null}
            </div>
            <div className="flex items-center justify-between border-b py-2"><Label htmlFor="model-enabled">{modelEnabled ? t("common.enabled") : t("common.disabled")}</Label><Switch id="model-enabled" checked={modelEnabled} onCheckedChange={(checked) => form.setValue("enabled", checked)} /></div>
            <DialogFooter><Button type="button" variant="secondary" size="sm" onClick={() => setEditing(null)}>{t("common.cancel")}</Button><Button type="submit" size="sm" disabled={updateMutation.isPending}>{updateMutation.isPending ? <Spinner /> : null}{t("common.save")}</Button></DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      <AlertDialog open={Boolean(deleting)} onOpenChange={(open) => !open && setDeleting(null)}>
        <AlertDialogContent>
          <AlertDialogHeader><AlertDialogTitle>{t("models.deleteTitle")}</AlertDialogTitle><AlertDialogDescription>{t("models.deleteDescription", { name: deleting?.publicId ?? "" })}</AlertDialogDescription></AlertDialogHeader>
          <AlertDialogFooter><AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel><AlertDialogAction className="bg-destructive text-white hover:bg-destructive/90" disabled={deleteMutation.isPending} onClick={() => deleting && deleteMutation.mutate(deleting.id)}>{deleteMutation.isPending ? <Spinner /> : null}{t("common.delete")}</AlertDialogAction></AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={batchDeleteOpen} onOpenChange={setBatchDeleteOpen}>
        <AlertDialogContent>
          <AlertDialogHeader><AlertDialogTitle>{t("models.batchDeleteTitle", { count: selected.size })}</AlertDialogTitle><AlertDialogDescription>{t("models.batchDeleteDescription")}</AlertDialogDescription></AlertDialogHeader>
          <AlertDialogFooter><AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel><AlertDialogAction className="bg-destructive text-white hover:bg-destructive/90" disabled={batchDeleteMutation.isPending} onClick={() => batchDeleteMutation.mutate()}>{batchDeleteMutation.isPending ? <Spinner /> : null}{t("common.delete")}</AlertDialogAction></AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function ModelProvider({ provider }: { provider: ModelRouteDTO["provider"] }) {
  const { t } = useTranslation();
  const label = provider === "grok_web" ? t("models.providerGrokWeb") : provider === "grok_console" ? t("console.name") : t("models.providerGrokBuild");
  const color = provider === "grok_web" ? "bg-quota-product-2" : provider === "grok_console" ? "bg-quota-product-4" : "bg-quota-product-1";
  return (
    <span className="inline-flex items-center gap-1.5 whitespace-nowrap text-xs text-muted-foreground">
      <span className={cn("size-2 rounded-full", color)} />
      {label}
    </span>
  );
}
