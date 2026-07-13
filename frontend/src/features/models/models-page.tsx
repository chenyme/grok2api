import { zodResolver } from "@hookform/resolvers/zod";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { MoreHorizontal, Pencil, Search } from "lucide-react";
import { useState } from "react";
import { useForm, useWatch } from "react-hook-form";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";
import { z } from "zod";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Spinner } from "@/components/ui/spinner";
import { Table, TableActionCell, TableActionHead, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { listModels, updateModel, updateModelsEnabled } from "@/entities/model/model-api";
import type { ModelRouteDTO } from "@/entities/model/types";
import { EmptyState, ErrorState, TableLoadingRow } from "@/shared/components/data-state";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { DataTableFilters } from "@/shared/components/data-table-filters";
import { Pagination } from "@/shared/components/pagination";
import { SortableTableHead } from "@/shared/components/sortable-table-head";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { formatDateTime } from "@/shared/lib/format";
import { nextTableSort, type SortOrder, type TableSort } from "@/shared/lib/table-sort";

export function ModelsPage() {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(50);
  const [search, setSearch] = useState("");
  const [statusFilter, setStatusFilter] = useState("");
  const [providerFilter, setProviderFilter] = useState<"grok_build" | "grok_web" | "">("");
  const [sort, setSort] = useState<TableSort>({ field: "", order: "asc" });
  const [selected, setSelected] = useState<Set<string>>(() => new Set());
  const [editing, setEditing] = useState<ModelRouteDTO | null>(null);
  const debouncedSearch = useDebouncedValue(search);
  const schema = z.object({
    publicId: z.string().min(1, t("errors.required")),
    enabled: z.boolean(),
  });
  type ModelForm = z.infer<typeof schema>;
  const form = useForm<ModelForm>({
    resolver: zodResolver(schema),
    defaultValues: { publicId: "", enabled: true },
  });
  const modelEnabled = useWatch({ control: form.control, name: "enabled" });

  const modelsQuery = useQuery({
    queryKey: ["models", page, pageSize, debouncedSearch, statusFilter, providerFilter, sort.field, sort.order],
    queryFn: () => listModels({ page, pageSize, search: debouncedSearch, status: statusFilter, provider: providerFilter, sortBy: sort.field || undefined, sortOrder: sort.field ? sort.order : undefined }),
  });

  const updateMutation = useMutation({
    mutationFn: (values: ModelForm) => {
      if (!editing) throw new Error(t("errors.generic"));
      return updateModel(editing.id, values);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["models"] });
      setEditing(null);
      toast.success(t("models.updated"));
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

  function showError(error: unknown): void {
    toast.error(error instanceof Error ? error.message : t("errors.generic"));
  }

  function beginEdit(model: ModelRouteDTO): void {
    setEditing(model);
    form.reset({ publicId: model.publicId, enabled: model.enabled });
  }

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
    <div className="space-y-8">
      <header>
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
                { id: "provider", label: t("models.provider"), value: providerFilter, onChange: (value) => { setProviderFilter(value as "grok_build" | "grok_web" | ""); setPage(1); }, options: [
                  { value: "grok_build", label: t("models.providerGrokBuild") },
                  { value: "grok_web", label: t("models.providerGrokWeb") },
                ] },
                { id: "status", label: t("models.status"), value: statusFilter, onChange: (value) => { setStatusFilter(value); setPage(1); }, options: [
                  { value: "enabled", label: t("common.enabled") },
                  { value: "disabled", label: t("common.disabled") },
                ] },
              ]} />
            </div>
            {selected.size > 0 ? (
              <div className="flex flex-wrap items-center gap-1.5">
                <span className="mr-1 text-xs text-muted-foreground">{t("common.selectedCount", { count: selected.size })}</span>
                <Button variant="secondary" size="sm" onClick={() => batchUpdateMutation.mutate(true)}>{t("common.enable")}</Button>
                <Button variant="secondary" size="sm" onClick={() => batchUpdateMutation.mutate(false)}>{t("common.disable")}</Button>
              </div>
            ) : null}
          </>
        )}
        footer={result && result.total > 0 ? <Pagination page={result.page} pageSize={result.pageSize} total={result.total} onPageChange={setPage} onPageSizeChange={(value) => { setPageSize(value); setPage(1); }} /> : undefined}
      >
        {modelsQuery.isError ? <ErrorState message={modelsQuery.error.message} onRetry={() => void modelsQuery.refetch()} /> : null}
        {result && result.items.length === 0 ? <EmptyState /> : null}
        {modelsQuery.isPending || (result && result.items.length > 0) ? (
          <Table className="min-w-[1000px] table-fixed text-xs">
            <colgroup>
              <col className="w-12" />
              <col className="w-56" />
              <col className="w-52" />
              <col className="w-24" />
              <col className="w-32" />
              <col className="w-40" />
              <col className="w-44" />
              <col className="w-12" />
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
            <TableBody>
              {modelsQuery.isPending ? <TableLoadingRow colSpan={8} /> : result?.items.map((model) => (
                <TableRow className="group" key={model.id} data-state={selected.has(model.id) ? "selected" : undefined}>
                  <TableCell className="px-2 text-center"><Checkbox checked={selected.has(model.id)} onCheckedChange={(checked) => toggleModel(model.id, checked === true)} aria-label={t("common.selectItem", { name: model.publicId })} /></TableCell>
                  <TableCell className="min-w-0">
                    <span className="block truncate text-xs font-medium" title={model.publicId}>{model.publicId}</span>
                  </TableCell>
                  <TableCell className="min-w-0">
                    <span className="block truncate text-xs text-muted-foreground" title={model.upstreamModel}>{model.upstreamModel}</span>
                  </TableCell>
                  <TableCell className="text-center">{model.enabled ? <Badge variant="secondary" className="bg-emerald-500/10 text-emerald-700 dark:text-emerald-300">{t("common.enabled")}</Badge> : <Badge variant="outline" className="text-muted-foreground">{t("common.disabled")}</Badge>}</TableCell>
                  <TableCell className="text-center"><Badge variant="outline">{model.provider === "grok_web" ? t("models.providerGrokWeb") : t("models.providerGrokBuild")}</Badge></TableCell>
                  <TableCell className="text-center text-xs">
                    <span
                      className="inline-flex items-baseline gap-1 tabular-nums"
                      title={t("models.supportSummary", { supported: model.supportedAccounts, total: model.totalAccounts })}
                    >
                      <span className="font-medium text-foreground">{model.supportedAccounts}</span>
                      <span className="text-muted-foreground">/ {model.totalAccounts}</span>
                    </span>
                  </TableCell>
                  <TableCell className="whitespace-nowrap text-xs text-muted-foreground">{formatDateTime(model.lastSyncedAt, i18n.language)}</TableCell>
                  <TableActionCell>
                    <DropdownMenu>
                      <DropdownMenuTrigger asChild><Button type="button" variant="ghost" size="icon" className="size-8" aria-label={t("common.actions")}><MoreHorizontal /></Button></DropdownMenuTrigger>
                      <DropdownMenuContent align="end">
                        <DropdownMenuItem onClick={() => beginEdit(model)}><Pencil />{t("common.edit")}</DropdownMenuItem>
                      </DropdownMenuContent>
                    </DropdownMenu>
                  </TableActionCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        ) : null}
      </DataTableShell>

      <Dialog open={Boolean(editing)} onOpenChange={(open) => !open && setEditing(null)}>
        <DialogContent>
          <DialogHeader><DialogTitle>{t("models.editTitle")}</DialogTitle><DialogDescription className="font-mono">{editing?.upstreamModel}</DialogDescription></DialogHeader>
          <form className="space-y-4" onSubmit={form.handleSubmit((values) => updateMutation.mutate(values))}>
            <div className="space-y-2"><Label htmlFor="model-public-id">{t("models.publicId")}</Label><Input id="model-public-id" className="font-mono" {...form.register("publicId")} />{form.formState.errors.publicId ? <p className="text-xs text-destructive">{form.formState.errors.publicId.message}</p> : null}</div>
            <div className="flex items-center justify-between border-b py-2"><Label htmlFor="model-enabled">{modelEnabled ? t("common.enabled") : t("common.disabled")}</Label><Switch id="model-enabled" checked={modelEnabled} onCheckedChange={(checked) => form.setValue("enabled", checked)} /></div>
            <DialogFooter><Button type="button" variant="secondary" size="sm" onClick={() => setEditing(null)}>{t("common.cancel")}</Button><Button type="submit" size="sm" disabled={updateMutation.isPending}>{updateMutation.isPending ? <Spinner /> : null}{t("common.save")}</Button></DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </div>
  );
}
