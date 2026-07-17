import { zodResolver } from "@hookform/resolvers/zod";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronLeft, ChevronRight, Copy, MoreHorizontal, Pencil, Plus, Search, Trash2 } from "lucide-react";
import { useState } from "react";
import { Controller, useForm, useWatch } from "react-hook-form";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import { z } from "zod";

import { CopyButton } from "@/shared/components/copy-button";

import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Spinner } from "@/components/ui/spinner";
import { Table, TableActionCell, TableActionHead, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { listModels } from "@/entities/model/model-api";
import { createClientKey, deleteClientKey, deleteClientKeys, getClientKeySecret, listClientKeys, updateClientKey, updateClientKeysEnabled, type ClientKeyDTO, type CreateKeyResponseDTO } from "@/features/client-keys/client-keys-api";
import { EmptyState, ErrorState, LoadingState, TableLoadingRow } from "@/shared/components/data-state";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { DataTableFilters } from "@/shared/components/data-table-filters";
import { DateTimePicker } from "@/shared/components/date-time-picker";
import { Pagination } from "@/shared/components/pagination";
import { SortableTableHead } from "@/shared/components/sortable-table-head";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { formatDateTime, toDateTimeLocal } from "@/shared/lib/format";
import { nextTableSort, type SortOrder, type TableSort } from "@/shared/lib/table-sort";

const USD_TICKS = 10_000_000_000;
const MAX_BILLING_LIMIT_USD = 900_000;

type SecretDialogState = {
  secret: string;
  source: "created" | "retrieved";
};

export function ClientKeysPage() {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [search, setSearch] = useState("");
  const [statusFilter, setStatusFilter] = useState("");
  const [modelScopeFilter, setModelScopeFilter] = useState("");
  const [sort, setSort] = useState<TableSort>({ field: "", order: "asc" });
  const [selected, setSelected] = useState<Set<string>>(() => new Set());
  const [batchDeleteOpen, setBatchDeleteOpen] = useState(false);
  const [editing, setEditing] = useState<ClientKeyDTO | "new" | null>(null);
  const [deleting, setDeleting] = useState<ClientKeyDTO | null>(null);
  const [secretDialog, setSecretDialog] = useState<SecretDialogState | null>(null);
  const [modelOptionsPage, setModelOptionsPage] = useState(1);
  const [modelOptionsSearch, setModelOptionsSearch] = useState("");
  const [statusReferenceTime] = useState(() => Date.now());
  const debouncedSearch = useDebouncedValue(search);
  const debouncedModelOptionsSearch = useDebouncedValue(modelOptionsSearch);
  const schema = z.object({
    name: z.string().min(1, t("errors.required")),
    enabled: z.boolean(),
    expiresAt: z.string(),
    rpmLimit: z.number().int().min(1, t("errors.positive")).max(100_000),
    maxConcurrent: z.number().int().min(1, t("errors.positive")).max(1_024),
    billingUnlimited: z.boolean(),
    billingLimitUsd: z.number().min(0.01, t("errors.positive")).max(MAX_BILLING_LIMIT_USD),
    allowedModelIds: z.array(z.string()),
  });
  type KeyForm = z.infer<typeof schema>;
  const form = useForm<KeyForm>({
    resolver: zodResolver(schema),
    defaultValues: { name: "", enabled: true, expiresAt: "", rpmLimit: 120, maxConcurrent: 8, billingUnlimited: true, billingLimitUsd: 10, allowedModelIds: [] },
  });
  const keyEnabled = useWatch({ control: form.control, name: "enabled" });
  const selectedModels = useWatch({ control: form.control, name: "allowedModelIds" });
  const billingUnlimited = useWatch({ control: form.control, name: "billingUnlimited" });

  const keysQuery = useQuery({
    queryKey: ["client-keys", page, pageSize, debouncedSearch, statusFilter, modelScopeFilter, sort.field, sort.order],
    queryFn: () => listClientKeys({ page, pageSize, search: debouncedSearch, status: statusFilter, modelScope: modelScopeFilter, sortBy: sort.field || undefined, sortOrder: sort.field ? sort.order : undefined }),
  });
  const modelsQuery = useQuery({
    queryKey: ["models", "options", modelOptionsPage, debouncedModelOptionsSearch],
    queryFn: () => listModels({ page: modelOptionsPage, pageSize: 50, search: debouncedModelOptionsSearch }),
    enabled: editing !== null,
  });

  const saveMutation = useMutation<CreateKeyResponseDTO | ClientKeyDTO, Error, KeyForm>({
    mutationFn: (values: KeyForm) => {
      const body = {
        name: values.name,
        enabled: values.enabled,
        rpmLimit: values.rpmLimit,
        maxConcurrent: values.maxConcurrent,
        billingLimitUsdTicks: values.billingUnlimited ? 0 : Math.round(values.billingLimitUsd * USD_TICKS),
        allowedModelIds: values.allowedModelIds,
        expiresAt: values.expiresAt ? new Date(values.expiresAt).toISOString() : "",
      };
      if (editing === "new") {
        return createClientKey(body);
      }
      if (!editing) throw new Error(t("errors.generic"));
      return updateClientKey(editing.id, body);
    },
    onSuccess: (result) => {
      void queryClient.invalidateQueries({ queryKey: ["client-keys"] });
      if ("secret" in result) {
        setSecretDialog({ secret: result.secret, source: "created" });
        toast.success(t("keys.created"));
      } else {
        toast.success(t("keys.updated"));
      }
      setEditing(null);
    },
    onError: showError,
  });

  const deleteMutation = useMutation({
    mutationFn: deleteClientKey,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["client-keys"] });
      setDeleting(null);
      toast.success(t("keys.deleted"));
    },
    onError: showError,
  });

  const copyMutation = useMutation({
    mutationFn: getClientKeySecret,
    onSuccess: (result) => setSecretDialog({ secret: result.secret, source: "retrieved" }),
    onError: showError,
  });

  const batchUpdateMutation = useMutation({
    mutationFn: (enabled: boolean) => updateClientKeysEnabled([...selected], enabled),
    onSuccess: () => {
      setSelected(new Set());
      void queryClient.invalidateQueries({ queryKey: ["client-keys"] });
      toast.success(t("keys.batchUpdated"));
    },
    onError: showError,
  });

  const batchDeleteMutation = useMutation({
    mutationFn: () => deleteClientKeys([...selected]),
    onSuccess: () => {
      setSelected(new Set());
      setBatchDeleteOpen(false);
      void queryClient.invalidateQueries({ queryKey: ["client-keys"] });
      toast.success(t("keys.deleted"));
    },
    onError: showError,
  });

  function showError(error: unknown): void {
    toast.error(error instanceof Error ? error.message : t("errors.generic"));
  }

  function beginCreate(): void {
    setEditing("new");
    setModelOptionsPage(1);
    setModelOptionsSearch("");
    form.reset({ name: "", enabled: true, expiresAt: "", rpmLimit: 120, maxConcurrent: 8, billingUnlimited: true, billingLimitUsd: 10, allowedModelIds: [] });
  }

  function beginEdit(key: ClientKeyDTO): void {
    setEditing(key);
    setModelOptionsPage(1);
    setModelOptionsSearch("");
    form.reset({
      name: key.name,
      enabled: key.enabled,
      expiresAt: toDateTimeLocal(key.expiresAt),
      rpmLimit: key.rpmLimit,
      maxConcurrent: key.maxConcurrent,
      billingUnlimited: key.billingLimitUsdTicks === 0,
      billingLimitUsd: key.billingLimitUsdTicks > 0 ? key.billingLimitUsdTicks / USD_TICKS : 10,
      allowedModelIds: key.allowedModelIds,
    });
  }

  function toggleModel(id: string): void {
    const current = form.getValues("allowedModelIds");
    form.setValue("allowedModelIds", current.includes(id) ? current.filter((value) => value !== id) : [...current, id], { shouldDirty: true });
  }

  const result = keysQuery.data;
  const pageIDs = result?.items.map((key) => key.id) ?? [];
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

  function toggleKey(id: string, checked: boolean): void {
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
        <h1 className="text-xl font-medium">{t("keys.title")}</h1>
        <p className="sr-only">{t("keys.description")}</p>
      </header>

      <DataTableShell
        toolbar={(
          <>
            <div className="flex w-full items-center gap-2 sm:w-auto">
              <div className="relative min-w-0 flex-1 sm:w-64 sm:flex-none">
                <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                <Input className="h-8 pl-9 text-xs" value={search} onChange={(event) => { setSearch(event.target.value); setPage(1); }} placeholder={t("keys.search")} aria-label={t("keys.search")} />
              </div>
              <DataTableFilters filters={[
                { id: "status", label: t("keys.status"), value: statusFilter, onChange: (value) => { setStatusFilter(value); setPage(1); }, options: [
                  { value: "active", label: t("keys.statusActive") },
                  { value: "disabled", label: t("common.disabled") },
                  { value: "expired", label: t("keys.statusExpired") },
                ] },
                { id: "modelScope", label: t("keys.models"), value: modelScopeFilter, onChange: (value) => { setModelScopeFilter(value); setPage(1); }, options: [
                  { value: "all", label: t("keys.allModels") },
                  { value: "restricted", label: t("keys.restrictedModels") },
                ] },
              ]} />
            </div>
            {selected.size > 0 ? (
              <div className="flex flex-wrap items-center gap-1.5">
                <span className="mr-1 text-xs text-muted-foreground">{t("common.selectedCount", { count: selected.size })}</span>
                <Button variant="secondary" size="sm" onClick={() => batchUpdateMutation.mutate(true)}>{t("common.enable")}</Button>
                <Button variant="secondary" size="sm" onClick={() => batchUpdateMutation.mutate(false)}>{t("common.disable")}</Button>
                <Button variant="ghost" size="sm" className="text-destructive hover:text-destructive" onClick={() => setBatchDeleteOpen(true)}>{t("common.delete")}</Button>
              </div>
            ) : <Button size="sm" onClick={beginCreate}><Plus />{t("keys.create")}</Button>}
          </>
        )}
        footer={result && result.total > 0 ? <Pagination page={result.page} pageSize={result.pageSize} total={result.total} onPageChange={setPage} onPageSizeChange={(value) => { setPageSize(value); setPage(1); }} /> : undefined}
      >
        {keysQuery.isError ? <ErrorState message={keysQuery.error.message} onRetry={() => void keysQuery.refetch()} /> : null}
        {result && result.items.length === 0 ? <EmptyState /> : null}
        {keysQuery.isPending || (result && result.items.length > 0) ? (
          <Table className="min-w-[1120px] table-fixed text-xs">
            <colgroup>
              <col className="w-10" />
              <col className="w-36" />
              <col className="w-56" />
              <col className="w-20" />
              <col className="w-18" />
              <col className="w-20" />
              <col className="w-40" />
              <col className="w-36" />
              <col className="w-36" />
              <col className="w-10" />
            </colgroup>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead><Checkbox checked={allPageSelected ? true : selectedOnPage.length > 0 ? "indeterminate" : false} onCheckedChange={(checked) => togglePage(checked === true)} aria-label={t("common.selectPage")} /></TableHead>
                <SortableTableHead field="name" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("keys.name")}</SortableTableHead>
                <SortableTableHead field="prefix" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("keys.prefix")}</SortableTableHead>
                <SortableTableHead field="status" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort}>{t("keys.status")}</SortableTableHead>
                <SortableTableHead field="rpmLimit" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort}>{t("keys.rpmShort")}</SortableTableHead>
                <SortableTableHead field="maxConcurrent" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort}>{t("keys.concurrencyShort")}</SortableTableHead>
                <SortableTableHead field="billingLimit" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("keys.billingLimit")}</SortableTableHead>
                <SortableTableHead field="expiresAt" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("keys.expires")}</SortableTableHead>
                <SortableTableHead field="lastUsedAt" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort}>{t("keys.lastUsed")}</SortableTableHead>
                <TableActionHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {keysQuery.isPending ? <TableLoadingRow colSpan={10} /> : result?.items.map((key) => (
                <TableRow className="group h-14" key={key.id} data-state={selected.has(key.id) ? "selected" : undefined}>
                  <TableCell><Checkbox checked={selected.has(key.id)} onCheckedChange={(checked) => toggleKey(key.id, checked === true)} aria-label={t("common.selectItem", { name: key.name })} /></TableCell>
                  <TableCell className="min-w-0">
                    <span className="block truncate font-medium" title={key.name}>{key.name}</span>
                    <span className="mt-0.5 block truncate text-[10px] text-muted-foreground">
                      {key.allowedModelIds.length === 0 ? t("keys.allModels") : t("keys.selectedModels", { count: key.allowedModelIds.length })}
                    </span>
                  </TableCell>
                  <TableCell className="overflow-hidden">
                    <div className="flex w-full min-w-0 items-center gap-1">
                      <code className="min-w-0 flex-1 truncate rounded bg-muted px-1.5 py-1 text-xs text-muted-foreground" title={`g2a_${key.prefix}_********`}>g2a_{key.prefix}_********</code>
                      <Tooltip>
                        <TooltipTrigger asChild>
                          <Button type="button" variant="ghost" size="icon" className="size-7 shrink-0" disabled={copyMutation.isPending} aria-label={t("keys.copySecret")} onClick={() => copyMutation.mutate(key.id)}>
                            {copyMutation.isPending && copyMutation.variables === key.id ? <Spinner className="size-3.5" /> : <Copy className="size-3.5" />}
                          </Button>
                        </TooltipTrigger>
                        <TooltipContent>{t("keys.copySecret")}</TooltipContent>
                      </Tooltip>
                    </div>
                  </TableCell>
                  <TableCell className="text-center"><ClientKeyStatus value={key} referenceTime={statusReferenceTime} /></TableCell>
                  <TableCell className="text-center text-xs tabular-nums">{key.rpmLimit}</TableCell>
                  <TableCell className="text-center text-xs tabular-nums">{key.maxConcurrent}</TableCell>
                  <TableCell><BillingUsage value={key} /></TableCell>
                  <TableCell className="overflow-hidden text-ellipsis whitespace-nowrap text-xs text-muted-foreground" title={key.expiresAt ? formatDateTime(key.expiresAt, i18n.language) : t("keys.neverExpires")}>{key.expiresAt ? formatDateTime(key.expiresAt, i18n.language) : t("keys.neverExpires")}</TableCell>
                  <TableCell className="overflow-hidden text-ellipsis whitespace-nowrap text-xs text-muted-foreground" title={formatDateTime(key.lastUsedAt, i18n.language)}>{formatDateTime(key.lastUsedAt, i18n.language)}</TableCell>
                  <TableActionCell>
                    <DropdownMenu>
                      <DropdownMenuTrigger asChild><Button variant="ghost" size="icon" className="size-8" aria-label={t("common.actions")}><MoreHorizontal /></Button></DropdownMenuTrigger>
                      <DropdownMenuContent align="end">
                        <DropdownMenuItem onClick={() => beginEdit(key)}><Pencil />{t("common.edit")}</DropdownMenuItem>
                        <DropdownMenuSeparator />
                        <DropdownMenuItem className="text-destructive focus:text-destructive" onClick={() => setDeleting(key)}><Trash2 />{t("common.delete")}</DropdownMenuItem>
                      </DropdownMenuContent>
                    </DropdownMenu>
                  </TableActionCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        ) : null}
      </DataTableShell>

      <Dialog open={editing !== null} onOpenChange={(open) => !open && setEditing(null)}>
        <DialogContent className="flex max-h-[calc(100svh-2rem)] min-h-0 flex-col gap-0 overflow-hidden p-0 sm:max-w-[520px]">
          <DialogHeader className="shrink-0 border-b px-5 py-4 pr-12">
            <DialogTitle>{editing === "new" ? t("keys.createTitle") : t("keys.editTitle")}</DialogTitle>
            <DialogDescription>{editing === "new" ? t("keys.description") : editing?.prefix}</DialogDescription>
          </DialogHeader>
          <form className="flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden" onSubmit={form.handleSubmit((values) => saveMutation.mutate(values))}>
            <div className="min-h-0 min-w-0 flex-1 space-y-4 overflow-y-auto overscroll-contain px-5 py-4">
              <div className="space-y-2"><Label htmlFor="key-name">{t("keys.name")}</Label><Input id="key-name" {...form.register("name")} />{form.formState.errors.name ? <p className="text-xs text-destructive">{form.formState.errors.name.message}</p> : null}</div>
              <div className="flex items-center justify-between border-b py-2"><Label htmlFor="key-enabled">{keyEnabled ? t("common.enabled") : t("common.disabled")}</Label><Switch id="key-enabled" checked={keyEnabled} onCheckedChange={(checked) => form.setValue("enabled", checked)} /></div>
              <div className="grid gap-4 sm:grid-cols-2">
                <div className="space-y-2"><Label htmlFor="key-rpm">{t("keys.rpm")}</Label><Input id="key-rpm" type="number" min="1" max="100000" {...form.register("rpmLimit", { valueAsNumber: true })} /></div>
                <div className="space-y-2"><Label htmlFor="key-concurrency">{t("keys.maxConcurrent")}</Label><Input id="key-concurrency" type="number" min="1" max="1024" {...form.register("maxConcurrent", { valueAsNumber: true })} /></div>
              </div>
              <div className="space-y-2">
                <div className="flex items-center justify-between gap-3">
                  <Label htmlFor="key-billing-unlimited">{t("keys.billingLimit")}</Label>
                  <div className="flex items-center gap-2">
                    <span className="text-xs text-muted-foreground">{t("keys.unlimited")}</span>
                    <Switch id="key-billing-unlimited" checked={billingUnlimited} onCheckedChange={(checked) => form.setValue("billingUnlimited", checked, { shouldDirty: true })} />
                  </div>
                </div>
                <div className="relative">
                  <span className="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-xs text-muted-foreground">$</span>
                  <Input className="pl-7" type="number" min="0.01" max={MAX_BILLING_LIMIT_USD} step="0.01" disabled={billingUnlimited} {...form.register("billingLimitUsd", { valueAsNumber: true })} />
                </div>
                <p className="text-xs text-muted-foreground">{t("keys.billingLimitDescription")}</p>
              </div>
              <div className="space-y-2">
                <Label>{t("keys.expires")}</Label>
                <Controller control={form.control} name="expiresAt" render={({ field }) => <DateTimePicker value={field.value} onChange={field.onChange} />} />
              </div>
              <fieldset className="min-w-0 space-y-2">
                <div className="flex items-center justify-between gap-3">
                  <legend className="text-xs font-medium">{t("keys.models")}</legend>
                  <span className="min-w-0 truncate text-xs text-muted-foreground">{selectedModels.length === 0 ? t("keys.allModels") : t("keys.selectedModels", { count: selectedModels.length })}</span>
                </div>
                <div className="min-w-0 overflow-hidden rounded-md border">
                  <div className="relative border-b bg-muted/25">
                    <Search className="pointer-events-none absolute left-3 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
                    <Input className="rounded-none border-0 bg-transparent pl-8 shadow-none focus-visible:bg-background focus-visible:ring-0" value={modelOptionsSearch} onChange={(event) => { setModelOptionsSearch(event.target.value); setModelOptionsPage(1); }} placeholder={t("keys.modelSearch")} aria-label={t("keys.modelSearch")} />
                  </div>
                  <div className="max-h-40 divide-y overflow-y-auto overscroll-contain sm:max-h-52">
                    {modelsQuery.isPending ? <LoadingState className="min-h-24" /> : modelsQuery.data?.items.map((model) => {
                      const checked = selectedModels.includes(model.id);
                      const controlId = `allowed-model-${model.id}`;
                      return (
                        <label key={model.id} htmlFor={controlId} className="flex h-9 cursor-pointer items-center gap-3 px-3 text-xs transition-colors hover:bg-accent/55">
                          <Checkbox id={controlId} checked={checked} onCheckedChange={() => toggleModel(model.id)} aria-label={t("common.selectItem", { name: model.publicId })} />
                          <span className="min-w-0 flex-1 truncate font-medium" title={model.publicId}>{model.publicId}</span>
                          <span className="hidden max-w-[42%] shrink-0 truncate text-muted-foreground sm:block" title={model.upstreamModel}>{model.upstreamModel}</span>
                          {!model.enabled ? <Badge variant="outline" className="shrink-0 text-muted-foreground">{t("common.disabled")}</Badge> : null}
                        </label>
                      );
                    })}
                    {modelsQuery.data?.items.length === 0 ? <p className="p-3 text-center text-xs text-muted-foreground">{t("common.noData")}</p> : null}
                  </div>
                  {modelsQuery.data && modelsQuery.data.total > modelsQuery.data.pageSize ? <ModelOptionPagination page={modelsQuery.data.page} pageSize={modelsQuery.data.pageSize} total={modelsQuery.data.total} onPageChange={setModelOptionsPage} /> : null}
                </div>
              </fieldset>
            </div>
            <DialogFooter className="shrink-0 gap-2 border-t bg-background px-5 py-4 sm:gap-0"><Button type="button" variant="secondary" size="sm" onClick={() => setEditing(null)}>{t("common.cancel")}</Button><Button type="submit" size="sm" disabled={saveMutation.isPending}>{saveMutation.isPending ? <Spinner /> : null}{editing === "new" ? t("common.create") : t("common.save")}</Button></DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      <Dialog open={secretDialog !== null} onOpenChange={(open) => !open && setSecretDialog(null)}>
        <DialogContent className="max-w-[440px]">
          <DialogHeader>
            <DialogTitle>{t(secretDialog?.source === "created" ? "keys.secretTitle" : "keys.copySecretTitle")}</DialogTitle>
            <DialogDescription>{t(secretDialog?.source === "created" ? "keys.secretDescription" : "keys.copySecretDescription")}</DialogDescription>
          </DialogHeader>
          <div className="min-w-0 space-y-1.5">
            <Label>{t("keys.secretLabel")}</Label>
            <div className="flex h-8 w-full min-w-0 overflow-hidden rounded-md border border-input bg-secondary/55">
              <code className="flex min-w-0 flex-1 select-all items-center overflow-x-auto whitespace-nowrap px-3 font-mono text-xs text-muted-foreground">{secretDialog?.source === "created" ? secretDialog.secret : t("keys.secretReady")}</code>
              <CopyButton value={secretDialog?.secret ?? ""} copyLabel={t("keys.copySecret")} disabled={!secretDialog?.secret} className="h-full w-8 shrink-0 rounded-none border-l" onCopied={() => toast.success(t("common.copied"))} />
            </div>
          </div>
          <DialogFooter><Button type="button" variant="secondary" size="sm" onClick={() => setSecretDialog(null)}>{t("common.close")}</Button></DialogFooter>
        </DialogContent>
      </Dialog>

      <AlertDialog open={Boolean(deleting)} onOpenChange={(open) => !open && setDeleting(null)}>
        <AlertDialogContent>
          <AlertDialogHeader><AlertDialogTitle>{t("keys.deleteTitle")}</AlertDialogTitle><AlertDialogDescription>{t("keys.deleteDescription")}</AlertDialogDescription></AlertDialogHeader>
          <AlertDialogFooter><AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel><AlertDialogAction className="bg-destructive text-white hover:bg-destructive/90" onClick={() => deleting && deleteMutation.mutate(deleting.id)}>{t("common.delete")}</AlertDialogAction></AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={batchDeleteOpen} onOpenChange={setBatchDeleteOpen}>
        <AlertDialogContent>
          <AlertDialogHeader><AlertDialogTitle>{t("keys.batchDeleteTitle", { count: selected.size })}</AlertDialogTitle><AlertDialogDescription>{t("keys.deleteDescription")}</AlertDialogDescription></AlertDialogHeader>
          <AlertDialogFooter><AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel><AlertDialogAction className="bg-destructive text-white hover:bg-destructive/90" onClick={() => batchDeleteMutation.mutate()}>{t("common.delete")}</AlertDialogAction></AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function BillingUsage({ value }: { value: ClientKeyDTO }) {
  const { t, i18n } = useTranslation();
  const used = value.billedUsageUsdTicks / USD_TICKS;
  if (value.billingLimitUsdTicks <= 0) {
    return (
      <div className="min-w-0">
        <div className="text-xs">{t("keys.unlimited")}</div>
        <div className="truncate text-xs text-muted-foreground">{t("keys.billedUsage", { value: formatUSD(used, i18n.language) })}</div>
      </div>
    );
  }
  const limit = value.billingLimitUsdTicks / USD_TICKS;
  const percent = Math.min(100, Math.max(0, (used / limit) * 100));
  return (
    <div className="min-w-0 space-y-1.5">
      <div className="truncate text-xs tabular-nums" title={`${formatUSD(used, i18n.language)} / ${formatUSD(limit, i18n.language)}`}>{formatUSD(used, i18n.language)} / {formatUSD(limit, i18n.language)}</div>
      <div className="h-1 overflow-hidden rounded-full bg-muted" aria-hidden="true">
        <div className="h-full rounded-full bg-emerald-500 transition-[width]" style={{ width: `${percent}%` }} />
      </div>
    </div>
  );
}

function formatUSD(value: number, locale: string): string {
  return new Intl.NumberFormat(locale, { style: "currency", currency: "USD", minimumFractionDigits: 2, maximumFractionDigits: 2 }).format(value);
}

function ModelOptionPagination({ page, pageSize, total, onPageChange }: { page: number; pageSize: number; total: number; onPageChange: (page: number) => void }) {
  const { t } = useTranslation();
  const pages = Math.max(1, Math.ceil(total / pageSize));
  return (
    <div className="flex h-9 items-center justify-between border-t bg-muted/20 px-2">
      <span className="px-1 text-xs text-muted-foreground">{t("common.pageOf", { page, pages })}</span>
      <div className="flex items-center gap-0.5">
        <Button type="button" variant="ghost" size="icon" className="size-7" disabled={page <= 1} onClick={() => onPageChange(page - 1)} aria-label={t("common.previousPage")}><ChevronLeft /></Button>
        <Button type="button" variant="ghost" size="icon" className="size-7" disabled={page >= pages} onClick={() => onPageChange(page + 1)} aria-label={t("common.nextPage")}><ChevronRight /></Button>
      </div>
    </div>
  );
}

function ClientKeyStatus({ value, referenceTime }: { value: ClientKeyDTO; referenceTime: number }) {
  const { t } = useTranslation();
  if (!value.enabled) {
    return <Badge variant="outline" className="text-muted-foreground">{t("common.disabled")}</Badge>;
  }
  if (value.expiresAt && new Date(value.expiresAt).getTime() <= referenceTime) {
    return <Badge variant="secondary" className="bg-amber-500/10 text-amber-700 dark:text-amber-300">{t("keys.statusExpired")}</Badge>;
  }
  return <Badge variant="secondary" className="bg-emerald-500/10 text-emerald-700 dark:text-emerald-300">{t("keys.statusActive")}</Badge>;
}
