import { zodResolver } from "@hookform/resolvers/zod";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowRight, CircleCheck, ClipboardPaste, Clock3, Copy, Download, ExternalLink, FileUp, Info, Link2, MoreHorizontal, Pencil, RefreshCw, RotateCw, Search, Trash2, TriangleAlert, Users } from "lucide-react";
import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
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
import { Switch } from "@/components/ui/switch";
import { Spinner } from "@/components/ui/spinner";
import { Table, TableActionCell, TableActionHead, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { ApiError } from "@/shared/api/client";
import { EmptyState, ErrorState, LoadingState, TableLoadingRow } from "@/shared/components/data-state";
import { DataTableShell } from "@/shared/components/data-table-shell";
import { DataTableFilters } from "@/shared/components/data-table-filters";
import { Pagination } from "@/shared/components/pagination";
import { SortableTableHead } from "@/shared/components/sortable-table-head";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { cn } from "@/shared/lib/cn";
import { formatDateTime, formatNumber } from "@/shared/lib/format";
import { nextTableSort, type SortOrder, type TableSort } from "@/shared/lib/table-sort";
import {
  deleteAccount,
  deleteAccounts,
  convertWebAccountsToBuild,
  exportAccounts,
  getAccountSummary,
  importAccounts,
  importWebAccounts,
  listAccounts,
  pollDeviceAuthorization,
  refreshAccountBilling,
  refreshAccountsBilling,
  refreshAccountToken,
  refreshAccountQuota,
  refreshAllAccountBilling,
  refreshAllAccountTokens,
  refreshAllWebAccountQuotas,
  startDeviceAuthorization,
  updateAccount,
  updateAccountsEnabled,
  type AccountDTO,
  type AccountUpdateInput,
  type AccountTaskProgressDTO,
  type BillingDTO,
  type BuildConversionInput,
  type DeviceSessionDTO,
  type QuotaDTO,
} from "@/features/accounts/accounts-api";

function isAbortError(error: unknown): boolean {
  return (error instanceof DOMException || error instanceof Error) && error.name === "AbortError";
}

export function AccountsPage() {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const fileInputRef = useRef<HTMLInputElement>(null);
  const syncAbortRef = useRef<AbortController | null>(null);
  const renewalAbortRef = useRef<AbortController | null>(null);
  const conversionAbortRef = useRef<AbortController | null>(null);
  const importAbortRef = useRef<AbortController | null>(null);
  const importToastRef = useRef<string | number | null>(null);
  const [provider, setProvider] = useState<"grok_build" | "grok_web">("grok_build");
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [search, setSearch] = useState("");
  const [typeFilter, setTypeFilter] = useState("");
  const [statusFilter, setStatusFilter] = useState("");
  const [renewalFilter, setRenewalFilter] = useState("");
  const [sort, setSort] = useState<TableSort>({ field: "createdAt", order: "desc" });
  const [selected, setSelected] = useState<Set<string>>(() => new Set());
  const [batchDeleteOpen, setBatchDeleteOpen] = useState(false);
  const [exportOpen, setExportOpen] = useState(false);
  const [syncAllOpen, setSyncAllOpen] = useState(false);
  const [syncProgress, setSyncProgress] = useState<AccountTaskProgressDTO | null>(null);
  const [conversionTargets, setConversionTargets] = useState<string[] | "all" | null>(null);
  const [conversionProgress, setConversionProgress] = useState<AccountTaskProgressDTO | null>(null);
  const [renewAllOpen, setRenewAllOpen] = useState(false);
  const [renewalProgress, setRenewalProgress] = useState<AccountTaskProgressDTO | null>(null);
  const [editing, setEditing] = useState<AccountDTO | null>(null);
  const [deleting, setDeleting] = useState<AccountDTO | null>(null);
  const [deviceOpen, setDeviceOpen] = useState(false);
  const [deviceSession, setDeviceSession] = useState<DeviceSessionDTO | null>(null);
  const [deviceStatus, setDeviceStatus] = useState<"starting" | "pending" | "failed">("starting");
  const [quickImportOpen, setQuickImportOpen] = useState(false);
  const [quickImportTokens, setQuickImportTokens] = useState("");
  const debouncedSearch = useDebouncedValue(search);

  useEffect(() => () => {
    syncAbortRef.current?.abort();
    renewalAbortRef.current?.abort();
    conversionAbortRef.current?.abort();
    importAbortRef.current?.abort();
    if (importToastRef.current !== null) toast.dismiss(importToastRef.current);
  }, []);

  const accountSchema = z.object({
    name: z.string().min(1, t("errors.required")),
    enabled: z.boolean(),
    priority: z.number().int(),
    maxConcurrent: z.number().int().min(1, t("errors.positive")).max(256),
    minimumRemaining: z.number().min(0),
  });
  type AccountForm = z.infer<typeof accountSchema>;
  const form = useForm<AccountForm>({
    resolver: zodResolver(accountSchema),
    defaultValues: { name: "", enabled: true, priority: 1, maxConcurrent: 8, minimumRemaining: 0 },
  });
  const accountEnabled = useWatch({ control: form.control, name: "enabled" });

  const accountsQuery = useQuery({
    queryKey: ["accounts", provider, page, pageSize, debouncedSearch, typeFilter, statusFilter, renewalFilter, sort.field, sort.order],
    queryFn: () => listAccounts({ provider, page, pageSize, search: debouncedSearch, type: typeFilter, status: statusFilter, renewal: provider === "grok_build" ? renewalFilter : undefined, sortBy: sort.field, sortOrder: sort.order }),
  });

  const summaryQuery = useQuery({
    queryKey: ["accounts", "summary"],
    queryFn: getAccountSummary,
  });

  const invalidateAccountData = useCallback(() => {
    void queryClient.invalidateQueries({ queryKey: ["accounts"] });
    void queryClient.invalidateQueries({ queryKey: ["accounts", "summary"] });
  }, [queryClient]);

  const updateMutation = useMutation({
    mutationFn: (values: AccountForm) => {
      if (!editing) throw new Error(t("errors.generic"));
      return updateAccount(editing.id, values satisfies AccountUpdateInput);
    },
    onSuccess: () => {
      invalidateAccountData();
      setEditing(null);
      toast.success(t("accounts.updated"));
    },
    onError: showError,
  });

  const deleteMutation = useMutation({
    mutationFn: deleteAccount,
    onSuccess: () => {
      invalidateAccountData();
      setDeleting(null);
      toast.success(t("accounts.deleted"));
    },
    onError: showError,
  });

  const billingMutation = useMutation({
    mutationFn: refreshAccountBilling,
    onSuccess: () => {
      invalidateAccountData();
      toast.success(t("accounts.billingRefreshed"));
    },
    onError: showError,
  });

  const tokenMutation = useMutation({
    mutationFn: refreshAccountToken,
    onSuccess: () => {
      invalidateAccountData();
      toast.success(t("accounts.authRefreshed"));
    },
    onError: showError,
  });

  const quotaMutation = useMutation({
    mutationFn: refreshAccountQuota,
    onSuccess: () => {
      invalidateAccountData();
      toast.success(t("accounts.billingRefreshed"));
    },
    onError: showError,
  });

  const allBillingMutation = useMutation({
    mutationFn: () => {
      const controller = new AbortController();
      syncAbortRef.current = controller;
      setSyncProgress(null);
      return refreshAllAccountBilling(setSyncProgress, controller.signal);
    },
    onSuccess: (result) => {
      setSyncAllOpen(false);
      toast.success(t("accounts.allBillingRefreshed", result));
    },
    onError: (error) => { if (!isAbortError(error)) showError(error); },
    onSettled: () => { syncAbortRef.current = null; setSyncProgress(null); invalidateAccountData(); },
  });

  const allTokenMutation = useMutation({
    mutationFn: () => {
      const controller = new AbortController();
      renewalAbortRef.current = controller;
      setRenewalProgress(null);
      return refreshAllAccountTokens(setRenewalProgress, controller.signal);
    },
    onSuccess: (result) => {
      setRenewAllOpen(false);
      toast.success(t("accounts.allTokensRefreshed", result));
    },
    onError: (error) => { if (!isAbortError(error)) showError(error); },
    onSettled: () => { renewalAbortRef.current = null; setRenewalProgress(null); invalidateAccountData(); },
  });

  const allWebQuotaMutation = useMutation({
    mutationFn: () => {
      const controller = new AbortController();
      syncAbortRef.current = controller;
      setSyncProgress(null);
      return refreshAllWebAccountQuotas(setSyncProgress, controller.signal);
    },
    onSuccess: (result) => {
      setSyncAllOpen(false);
      toast.success(t("accounts.allBillingRefreshed", result));
    },
    onError: (error) => { if (!isAbortError(error)) showError(error); },
    onSettled: () => { syncAbortRef.current = null; setSyncProgress(null); invalidateAccountData(); },
  });
  const conversionMutation = useMutation({
    mutationFn: (input: BuildConversionInput) => {
      const controller = new AbortController();
      conversionAbortRef.current = controller;
      setConversionProgress(Array.isArray(conversionTargets) ? { completed: 0, total: conversionTargets.length } : null);
      return convertWebAccountsToBuild(input, setConversionProgress, controller.signal);
    },
    onSuccess: (conversion) => {
      setConversionProgress(null);
      setConversionTargets(null);
      setSelected(new Set());
      toast.success(t("accounts.conversionCompleted", conversion));
    },
    onError: (error) => { if (!isAbortError(error)) showError(error); },
    onSettled: () => {
      conversionAbortRef.current = null;
      setConversionProgress(null);
      invalidateAccountData();
      void queryClient.invalidateQueries({ queryKey: ["models"] });
    },
  });

  const importMutation = useMutation({
    mutationFn: (file: File) => {
      const controller = new AbortController();
      importAbortRef.current = controller;
      const toastID = toast.loading(t("common.importingProgress", { completed: 0, total: "…" }));
      importToastRef.current = toastID;
      const onProgress = (progress: AccountTaskProgressDTO) => {
        toast.loading(t(progress.phase === "syncing" ? "common.syncingProgress" : "common.importingProgress", progress), { id: toastID });
      };
      return provider === "grok_web" ? importWebAccounts(file, onProgress, controller.signal) : importAccounts(file, onProgress, controller.signal);
    },
    onSuccess: (result) => {
      if (importToastRef.current !== null) toast.dismiss(importToastRef.current);
      importToastRef.current = null;
      importAbortRef.current = null;
      setQuickImportOpen(false);
      setQuickImportTokens("");
      if (result.syncFailed > 0) {
        toast.warning(t("accounts.importedWithSyncFailures", result));
        return;
      }
      toast.success(t("accounts.imported", result));
    },
    onError: (error) => {
      if (importToastRef.current !== null) toast.dismiss(importToastRef.current);
      importToastRef.current = null;
      importAbortRef.current = null;
      if (!isAbortError(error)) showError(error);
    },
    onSettled: () => {
      importAbortRef.current = null;
      invalidateAccountData();
    },
  });

  const exportMutation = useMutation({
    mutationFn: exportAccounts,
    onSuccess: (blob) => {
      downloadAccountExport(blob);
      setExportOpen(false);
      toast.success(t("accounts.exported"));
    },
    onError: showError,
  });

  const batchUpdateMutation = useMutation({
    mutationFn: (enabled: boolean) => updateAccountsEnabled([...selected], enabled, provider),
    onSuccess: () => {
      setSelected(new Set());
      invalidateAccountData();
      toast.success(t("accounts.batchUpdated"));
    },
    onError: showError,
  });

  const batchBillingMutation = useMutation({
    mutationFn: () => refreshAccountsBilling([...selected], provider),
    onSuccess: (result) => {
      setSelected(new Set());
      invalidateAccountData();
      toast.success(t("accounts.batchBillingRefreshed", result));
    },
    onError: showError,
  });

  const batchDeleteMutation = useMutation({
    mutationFn: () => deleteAccounts([...selected], provider),
    onSuccess: () => {
      setSelected(new Set());
      setBatchDeleteOpen(false);
      invalidateAccountData();
      toast.success(t("accounts.deleted"));
    },
    onError: showError,
  });

  useEffect(() => {
    if (!deviceOpen || !deviceSession || deviceStatus !== "pending") {
      return;
    }
    const controller = new AbortController();
    let timeout = 0;
    const poll = async () => {
      try {
        const result = await pollDeviceAuthorization(deviceSession.sessionId, controller.signal);
        if (result.status === "succeeded") {
          toast.success(t("accounts.created"));
          setDeviceOpen(false);
          setDeviceSession(null);
          invalidateAccountData();
          return;
        }
        if (result.status === "syncFailed") {
          toast.warning(t("accounts.createdWithSyncFailure"));
          setDeviceOpen(false);
          setDeviceSession(null);
          invalidateAccountData();
          return;
        }
        timeout = window.setTimeout(poll, deviceSession.intervalSeconds * 1000);
      } catch (error) {
        if (controller.signal.aborted) return;
        if (error instanceof ApiError && error.status === 429) {
          timeout = window.setTimeout(poll, (deviceSession.intervalSeconds + 5) * 1000);
          return;
        }
        setDeviceStatus("failed");
        toast.error(error instanceof Error ? error.message : t("errors.generic"));
      }
    };
    timeout = window.setTimeout(poll, deviceSession.intervalSeconds * 1000);
    return () => {
      controller.abort();
      window.clearTimeout(timeout);
    };
  }, [deviceOpen, deviceSession, deviceStatus, invalidateAccountData, t]);

  function changeProvider(value: "grok_build" | "grok_web") {
    setProvider(value);
    setPage(1);
    setSelected(new Set());
    setTypeFilter("");
    setStatusFilter("");
    setRenewalFilter("");
    setQuickImportOpen(false);
    setQuickImportTokens("");
  }

  function submitQuickImport(): void {
    const value = quickImportTokens.trim();
    if (!value) return;
    importMutation.mutate(new File([value], "grok-web-sso-tokens.txt", { type: "text/plain" }));
  }

  async function startDeviceLogin(): Promise<void> {
    setDeviceOpen(true);
    setDeviceStatus("starting");
    setDeviceSession(null);
    try {
      const session = await startDeviceAuthorization();
      setDeviceSession(session);
      setDeviceStatus("pending");
    } catch (error) {
      setDeviceStatus("failed");
      showError(error);
    }
  }

  function beginEdit(account: AccountDTO): void {
    setEditing(account);
    form.reset({
      name: account.name,
      enabled: account.enabled,
      priority: account.priority,
      maxConcurrent: account.maxConcurrent,
      minimumRemaining: account.minimumRemaining,
    });
  }

  function showError(error: unknown): void {
    toast.error(error instanceof Error ? error.message : t("errors.generic"));
  }

  const result = accountsQuery.data;
  const pageIDs = result?.items.map((account) => account.id) ?? [];
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

  function toggleAccount(id: string, checked: boolean): void {
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

  const summary = summaryQuery.data;
  const totalAccounts = summary?.total ?? 0;
  const activeAccounts = summary?.available ?? 0;
  const recoveringAccounts = summary?.recovering ?? 0;
  const attentionAccounts = summary?.attention ?? 0;
  const buildSummary = summary?.providers.grok_build ?? { total: 0, available: 0 };
  const webSummary = summary?.providers.grok_web ?? { total: 0, available: 0 };
  const summaryLoading = summaryQuery.isPending;
  const summaryUnavailable = summaryQuery.isError;
  const hasAccounts = totalAccounts > 0 || (result?.total ?? 0) > 0;

  return (
    <div className="space-y-8">
      <header>
        <h1 className="text-xl font-medium">{t("accounts.title")}</h1>
        <p className="sr-only">{t("accounts.description")}</p>
      </header>
      <section className="grid gap-2 sm:grid-cols-2 xl:grid-cols-4">
        <AccountMetricPanel icon={<Users />} loading={summaryLoading} label={t("accounts.totalAccounts")} value={summaryUnavailable ? "-" : formatNumber(totalAccounts, i18n.language, 0)} detail={t("accounts.providerAccountBreakdown", { build: formatNumber(buildSummary.total, i18n.language, 0), web: formatNumber(webSummary.total, i18n.language, 0) })} />
        <AccountMetricPanel icon={<CircleCheck />} loading={summaryLoading} label={t("accounts.availableAccounts")} value={summaryUnavailable ? "-" : formatNumber(activeAccounts, i18n.language, 0)} detail={t("accounts.providerAvailableBreakdown", { build: formatNumber(buildSummary.available, i18n.language, 0), web: formatNumber(webSummary.available, i18n.language, 0) })} />
        <AccountMetricPanel icon={<Clock3 />} loading={summaryLoading} label={t("accounts.recoveringAccounts")} value={summaryUnavailable ? "-" : formatNumber(recoveringAccounts, i18n.language, 0)} detail={t("accounts.recoveryBreakdown", { cooldown: formatNumber(summary?.recovery.cooldown ?? 0, i18n.language, 0), reset: formatNumber(summary?.recovery.waitingReset ?? 0, i18n.language, 0), probing: formatNumber(summary?.recovery.probing ?? 0, i18n.language, 0) })} />
        <AccountMetricPanel icon={<TriangleAlert />} loading={summaryLoading} label={t("accounts.attentionAccounts")} value={summaryUnavailable ? "-" : formatNumber(attentionAccounts, i18n.language, 0)} detail={t("accounts.issueBreakdown", { disabled: formatNumber(summary?.issues.disabled ?? 0, i18n.language, 0), reauth: formatNumber(summary?.issues.reauthRequired ?? 0, i18n.language, 0) })} />
      </section>
      <div className="space-y-6">
        <Tabs value={provider} onValueChange={(value) => changeProvider(value as "grok_build" | "grok_web")}>
          <TabsList>
            <TabsTrigger value="grok_build">Grok Build</TabsTrigger>
            <TabsTrigger value="grok_web">Grok Web</TabsTrigger>
          </TabsList>
        </Tabs>
        <input
          ref={fileInputRef}
          type="file"
          accept={provider === "grok_web" ? "application/json,text/plain,.json,.txt" : "application/json,.json"}
          className="hidden"
          onChange={(event) => {
            const file = event.target.files?.[0];
            if (file) {
              importMutation.mutate(file);
            }
            event.target.value = "";
          }}
        />

        <DataTableShell
        toolbar={(
          <>
            <div className="flex w-full items-center gap-2 sm:w-auto">
              <div className="relative min-w-0 flex-1 sm:w-64 sm:flex-none">
                <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                <Input className="h-8 pl-9 text-xs" value={search} onChange={(event) => { setSearch(event.target.value); setPage(1); }} placeholder={t("accounts.search")} aria-label={t("accounts.search")} />
              </div>
              <DataTableFilters filters={[
                { id: "type", label: t("accounts.type"), value: typeFilter, onChange: (value: string) => { setTypeFilter(value); setPage(1); }, options: provider === "grok_web" ? [
                  { value: "auto", label: "Auto" },
                  { value: "basic", label: "Basic" },
                  { value: "super", label: "Super" },
                  { value: "heavy", label: "Heavy" },
                ] : [
                  { value: "free", label: t("accounts.quotaFree") },
                  { value: "paid", label: t("accounts.quotaSuper") },
                  { value: "unknown", label: t("dashboard.unknown") },
                ] },
                { id: "status", label: t("accounts.status"), value: statusFilter, onChange: (value) => { setStatusFilter(value); setPage(1); }, options: [
                  { value: "active", label: t("accounts.statusActive") },
                  { value: "disabled", label: t("accounts.statusDisabled") },
                  { value: "reauthRequired", label: t("accounts.statusReauthRequired") },
                  { value: "cooldown", label: t("accounts.statusCooldown") },
                  { value: "waitingReset", label: t("accounts.waitingReset") },
                  { value: "probing", label: t("accounts.probing") },
                ] },
                ...(provider === "grok_build" ? [{ id: "renewal", label: t("accountCredential.label"), value: renewalFilter, onChange: (value: string) => { setRenewalFilter(value); setPage(1); }, options: [
                  { value: "refreshable", label: t("accountCredential.autoRefresh") },
                  { value: "unrefreshable", label: t("accountCredential.noAutoRefresh") },
                ] }] : []),
              ]} />
            </div>
            {selected.size > 0 ? (
              <div className="flex flex-wrap items-center gap-1.5">
                <span className="mr-1 text-xs text-muted-foreground">{t("common.selectedCount", { count: selected.size })}</span>
                <Button variant="secondary" size="sm" onClick={() => batchUpdateMutation.mutate(true)}>{t("common.enable")}</Button>
                <Button variant="secondary" size="sm" onClick={() => batchUpdateMutation.mutate(false)}>{t("common.disable")}</Button>
                {provider === "grok_web" ? <Button variant="secondary" size="sm" onClick={() => setConversionTargets([...selected])}>{t("accounts.convertToBuild")}</Button> : null}
                {provider === "grok_build" ? <Button variant="secondary" size="sm" onClick={() => batchBillingMutation.mutate()}>{t("accounts.refreshBilling")}</Button> : null}
                <Button variant="ghost" size="sm" className="text-destructive hover:text-destructive" onClick={() => setBatchDeleteOpen(true)}>{t("common.delete")}</Button>
              </div>
            ) : (
              <div className="flex items-center gap-1.5">
                {provider === "grok_web" && webSummary.total > 0 ? <Button variant="secondary" size="sm" onClick={() => setConversionTargets("all")}>{t("accountBulk.convertAllToBuild")}</Button> : null}
                {hasAccounts ? <Button variant="secondary" size="sm" onClick={() => setSyncAllOpen(true)}>{t("accounts.syncAll")}</Button> : null}
                {hasAccounts && provider === "grok_build" ? <Button variant="secondary" size="sm" onClick={() => setRenewAllOpen(true)}>{t("accounts.renewAll")}</Button> : null}
                <DropdownMenu>
                  <DropdownMenuTrigger asChild><Button size="sm">{t("accounts.connectAccount")}</Button></DropdownMenuTrigger>
                  <DropdownMenuContent align="end">
                    {provider === "grok_build" ? <DropdownMenuItem onClick={() => void startDeviceLogin()}><ExternalLink />{t("accounts.deviceLogin")}</DropdownMenuItem> : null}
                    {provider === "grok_web" ? <DropdownMenuItem disabled={importMutation.isPending} onClick={() => setQuickImportOpen(true)}><ClipboardPaste />{t("accounts.quickImportSSO")}</DropdownMenuItem> : null}
                    <DropdownMenuItem disabled={importMutation.isPending} onClick={() => fileInputRef.current?.click()}><FileUp />{provider === "grok_web" ? t("accounts.importWebFile") : t("accounts.importAuth")}</DropdownMenuItem>
                    {hasAccounts && provider === "grok_build" ? (
                      <>
                        <DropdownMenuSeparator />
                        <DropdownMenuItem onClick={() => setExportOpen(true)}><Download />{t("accounts.exportAuth")}</DropdownMenuItem>
                      </>
                    ) : null}
                  </DropdownMenuContent>
                </DropdownMenu>
              </div>
            )}
          </>
        )}
        footer={result && result.total > 0 ? <Pagination page={result.page} pageSize={result.pageSize} total={result.total} onPageChange={setPage} onPageSizeChange={(value) => { setPageSize(value); setPage(1); }} /> : undefined}
      >
        {accountsQuery.isError ? <ErrorState message={accountsQuery.error.message} onRetry={() => void accountsQuery.refetch()} /> : null}
        {result && result.items.length === 0 ? <EmptyState /> : null}
        {accountsQuery.isPending || (result && result.items.length > 0) ? (
          <Table className="table-fixed border-collapse min-w-[780px] xl:min-w-[960px] 2xl:min-w-[1080px]">
            <colgroup>
              <col style={{ width: "3%" }} />
              <col style={{ width: "15%" }} />
              <col style={{ width: "7%" }} />
              <col style={{ width: "7%" }} />
              <col style={{ width: provider === "grok_web" ? "46%" : "30%" }} />
              {provider === "grok_build" ? <col style={{ width: "16%" }} /> : null}
              <col style={{ width: "18%" }} />
              <col style={{ width: "4%" }} />
            </colgroup>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="px-2"><Checkbox checked={allPageSelected ? true : selectedOnPage.length > 0 ? "indeterminate" : false} onCheckedChange={(checked) => togglePage(checked === true)} aria-label={t("common.selectPage")} /></TableHead>
                <SortableTableHead field="name" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("accounts.account")}</SortableTableHead>
                <SortableTableHead field="type" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort} className="whitespace-nowrap">{t("accounts.type")}</SortableTableHead>
                <SortableTableHead field="status" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort} className="whitespace-nowrap">{t("accounts.status")}</SortableTableHead>
                <TableHead className="whitespace-nowrap">{t("accounts.quota")}</TableHead>
                {provider === "grok_build" ? <TableHead className="whitespace-nowrap pl-4">{t("accountCredential.label")}</TableHead> : null}
				<SortableTableHead field="createdAt" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort} className="whitespace-nowrap">{t("accounts.createdAt")}</SortableTableHead>
                <TableActionHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {accountsQuery.isPending ? <TableLoadingRow colSpan={provider === "grok_web" ? 7 : 8} /> : result?.items.map((account) => {
                const accountDetail = account.email ?? account.userId ?? account.teamId;
                const showAccountDetail = accountDetail?.trim().toLocaleLowerCase() !== account.name.trim().toLocaleLowerCase();
                const linkedProviderLabel = account.linkedProvider === "grok_build" ? "Grok Build" : "Grok Web";
                return (
                  <TableRow className="group" key={account.id} data-state={selected.has(account.id) ? "selected" : undefined}>
                    <TableCell className="px-2"><Checkbox checked={selected.has(account.id)} onCheckedChange={(checked) => toggleAccount(account.id, checked === true)} aria-label={t("common.selectItem", { name: account.name })} /></TableCell>
                    <TableCell className="min-w-0">
                      <div className="truncate text-xs font-medium" title={account.name}>{account.name}</div>
                      {showAccountDetail || account.linkedAccountId ? (
                        <div className="mt-0.5 flex min-w-0 items-center gap-1.5 text-xs text-muted-foreground">
                          {showAccountDetail ? <span className="truncate" title={accountDetail}>{accountDetail}</span> : null}
                          {showAccountDetail && account.linkedAccountId ? <span className="shrink-0 text-border" aria-hidden="true">·</span> : null}
                          {account.linkedAccountId ? (
                            <Tooltip>
                              <TooltipTrigger asChild>
                                <span className="inline-flex shrink-0 items-center gap-1 whitespace-nowrap text-muted-foreground/80">
                                  <Link2 className="size-3" />
                                  {linkedProviderLabel}
                                </span>
                              </TooltipTrigger>
                              <TooltipContent>{t("accounts.linkedAccountTooltip", { name: account.linkedAccountName || linkedProviderLabel })}</TooltipContent>
                            </Tooltip>
                          ) : null}
                        </div>
                      ) : null}
                    </TableCell>
                    <TableCell className="text-center whitespace-nowrap">{provider === "grok_web" ? <WebAccountType tier={account.webTier} /> : <AccountType quota={account.quota} />}</TableCell>
                    <TableCell className="text-center whitespace-nowrap"><AccountStatus account={account} /></TableCell>
                    <TableCell>{provider === "grok_web" ? <WebQuota windows={account.quotaWindows ?? []} locale={i18n.language} tier={account.webTier} /> : <AccountQuota quota={account.quota} billing={account.billing} locale={i18n.language} />}</TableCell>
					{provider === "grok_build" ? <TableCell className="whitespace-nowrap pl-4 text-xs">
					  {account.refreshable ? (
						<Tooltip>
						  <TooltipTrigger asChild><span tabIndex={0} className="cursor-help font-medium text-emerald-700 dark:text-emerald-300">{t("accountCredential.autoRefresh")}</span></TooltipTrigger>
						  <TooltipContent>{account.expiresAt ? t("accountCredential.expiresAt", { time: formatDateTime(account.expiresAt, i18n.language) }) : t("accountCredential.expiryUnknown")}</TooltipContent>
						</Tooltip>
					  ) : <span className="font-medium text-amber-700 dark:text-amber-300">{t("accountCredential.noAutoRefresh")}</span>}
					</TableCell> : null}
					<TableCell className="whitespace-nowrap text-xs text-muted-foreground">{formatDateTime(account.createdAt, i18n.language)}</TableCell>
                    <TableActionCell>
                      <DropdownMenu>
                        <DropdownMenuTrigger asChild><Button variant="ghost" size="icon" className="size-8" aria-label={t("common.actions")}><MoreHorizontal /></Button></DropdownMenuTrigger>
                        <DropdownMenuContent align="end">
                          <DropdownMenuItem onClick={() => beginEdit(account)}><Pencil />{t("common.edit")}</DropdownMenuItem>
                          {provider === "grok_web" && !account.linkedAccountId ? <DropdownMenuItem onClick={() => setConversionTargets([account.id])}><ArrowRight />{t("accounts.convertToBuild")}</DropdownMenuItem> : null}
                          {provider === "grok_build" ? <DropdownMenuItem onClick={() => tokenMutation.mutate(account.id)}><RotateCw />{t("accounts.refreshToken")}</DropdownMenuItem> : null}
                          <DropdownMenuItem onClick={() => provider === "grok_web" ? quotaMutation.mutate(account.id) : billingMutation.mutate(account.id)}><RefreshCw />{provider === "grok_web" ? t("accounts.refreshModeQuota") : t("accounts.refreshBilling")}</DropdownMenuItem>
                          <DropdownMenuSeparator />
                          <DropdownMenuItem className="text-destructive focus:text-destructive" onClick={() => setDeleting(account)}><Trash2 />{t("common.delete")}</DropdownMenuItem>
                        </DropdownMenuContent>
                      </DropdownMenu>
                    </TableActionCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        ) : null}
        </DataTableShell>
      </div>

      <AlertDialog open={syncAllOpen} onOpenChange={(open) => { if (!open) syncAbortRef.current?.abort(); setSyncAllOpen(open); }}>
        <AlertDialogContent>
          <AlertDialogHeader><AlertDialogTitle>{t("accounts.syncAllTitle")}</AlertDialogTitle><AlertDialogDescription>{t(provider === "grok_web" ? "accounts.syncAllWebDescription" : "accounts.syncAllDescription")}</AlertDialogDescription></AlertDialogHeader>
          <AlertDialogFooter><AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel><AlertDialogAction disabled={allBillingMutation.isPending || allWebQuotaMutation.isPending} onClick={() => provider === "grok_web" ? allWebQuotaMutation.mutate() : allBillingMutation.mutate()}>{allBillingMutation.isPending || allWebQuotaMutation.isPending ? <><Spinner />{syncProgress ? <span className="tabular-nums">{syncProgress.completed} / {syncProgress.total}</span> : t("common.loading")}</> : t("accounts.syncAll")}</AlertDialogAction></AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={conversionTargets !== null} onOpenChange={(open) => { if (!open) { conversionAbortRef.current?.abort(); setConversionTargets(null); } }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t(conversionTargets === "all" ? "accountBulk.convertAllToBuildTitle" : "accounts.convertToBuildTitle")}</AlertDialogTitle>
            <AlertDialogDescription>{conversionTargets === "all" ? t("accountBulk.convertAllToBuildDescription") : t("accounts.convertToBuildDescription", { count: conversionTargets?.length ?? 0 })}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction disabled={conversionMutation.isPending || conversionTargets === null || (Array.isArray(conversionTargets) && conversionTargets.length === 0)} onClick={(event) => { event.preventDefault(); if (conversionTargets === "all") conversionMutation.mutate({ all: true }); else if (conversionTargets) conversionMutation.mutate({ ids: conversionTargets }); }}>
              {conversionMutation.isPending ? <><Spinner />{conversionProgress ? <span className="tabular-nums">{conversionProgress.completed} / {conversionProgress.total}</span> : t("common.loading")}</> : t(conversionTargets === "all" ? "accountBulk.convertAllToBuild" : "accounts.convertToBuild")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={renewAllOpen} onOpenChange={(open) => { if (!open) renewalAbortRef.current?.abort(); setRenewAllOpen(open); }}>
        <AlertDialogContent>
          <AlertDialogHeader><AlertDialogTitle>{t("accounts.renewAllTitle")}</AlertDialogTitle><AlertDialogDescription>{t("accounts.renewAllDescription")}</AlertDialogDescription></AlertDialogHeader>
          <AlertDialogFooter><AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel><AlertDialogAction disabled={allTokenMutation.isPending} onClick={() => allTokenMutation.mutate()}>{allTokenMutation.isPending ? <><Spinner />{renewalProgress ? <span className="tabular-nums">{renewalProgress.completed} / {renewalProgress.total}</span> : t("common.loading")}</> : t("accounts.renewAll")}</AlertDialogAction></AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={exportOpen} onOpenChange={setExportOpen}>
        <AlertDialogContent>
          <AlertDialogHeader><AlertDialogTitle>{t("accounts.exportTitle")}</AlertDialogTitle><AlertDialogDescription>{t("accounts.exportDescription")}</AlertDialogDescription></AlertDialogHeader>
          <AlertDialogFooter><AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel><AlertDialogAction disabled={exportMutation.isPending} onClick={() => exportMutation.mutate()}>{t("accounts.exportAuth")}</AlertDialogAction></AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <Dialog open={deviceOpen} onOpenChange={setDeviceOpen}>
        <DialogContent className="max-w-[420px]">
          <DialogHeader>
            <DialogTitle>{t("accounts.deviceTitle")}</DialogTitle>
            <DialogDescription>{t("accounts.deviceDescription")}</DialogDescription>
          </DialogHeader>
          {deviceStatus === "starting" ? <LoadingState className="min-h-36" /> : null}
          {deviceSession ? (
            <div className="space-y-4">
              <div className="space-y-1.5">
                <Label>{t("accounts.userCode")}</Label>
                <div className="relative">
                  <code className="flex h-11 items-center rounded-md border bg-muted/40 px-3 pr-11 font-mono text-lg font-semibold tabular-nums">{deviceSession.userCode}</code>
                  <Tooltip>
                    <TooltipTrigger asChild>
                      <Button type="button" variant="ghost" size="icon" className="absolute right-1.5 top-1/2 size-8 -translate-y-1/2 rounded-md" aria-label={t("common.copy")} onClick={() => {
                        void navigator.clipboard.writeText(deviceSession.userCode);
                        toast.success(t("common.copied"));
                      }}><Copy /></Button>
                    </TooltipTrigger>
                    <TooltipContent>{t("common.copy")}</TooltipContent>
                  </Tooltip>
                </div>
              </div>
              <Button type="button" size="sm" className="w-full" onClick={() => window.open(deviceSession.verificationUriComplete || deviceSession.verificationUri, "_blank", "noopener,noreferrer")}>
                <ExternalLink />{t("accounts.openVerification")}
              </Button>
              <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
                <span className="flex items-center gap-2"><Spinner className="size-3.5" />{t("accounts.waiting")}</span>
                <span className="whitespace-nowrap">{t("accounts.expiresAt", { time: formatDateTime(deviceSession.expiresAt, i18n.language) })}</span>
              </div>
            </div>
          ) : null}
          {deviceStatus === "failed" ? <Button type="button" variant="secondary" size="sm" className="w-full" onClick={() => void startDeviceLogin()}>{t("common.refresh")}</Button> : null}
        </DialogContent>
      </Dialog>

      <Dialog open={quickImportOpen} onOpenChange={(open) => { setQuickImportOpen(open); if (!open) setQuickImportTokens(""); }}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("accounts.quickImportTitle")}</DialogTitle>
            <DialogDescription>{t("accounts.quickImportDescription")}</DialogDescription>
          </DialogHeader>
          <div className="space-y-2">
            <Label htmlFor="quick-sso-tokens">{t("accounts.ssoTokens")}</Label>
            <Textarea
              id="quick-sso-tokens"
              className="min-h-56 font-mono"
              autoComplete="off"
              spellCheck={false}
              value={quickImportTokens}
              onChange={(event) => setQuickImportTokens(event.target.value)}
              placeholder={t("accounts.ssoTokenPlaceholder")}
            />
          </div>
          <DialogFooter>
            <Button type="button" variant="secondary" size="sm" onClick={() => { setQuickImportOpen(false); setQuickImportTokens(""); }}>{t("common.cancel")}</Button>
            <Button type="button" size="sm" disabled={!quickImportTokens.trim() || importMutation.isPending} onClick={submitQuickImport}>{importMutation.isPending ? <Spinner /> : null}{t("accounts.importAction")}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={Boolean(editing)} onOpenChange={(open) => !open && setEditing(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("common.edit")} {editing?.name}</DialogTitle>
            <DialogDescription>{editing?.email ?? editing?.userId}</DialogDescription>
          </DialogHeader>
          <form className="space-y-4" onSubmit={form.handleSubmit((values) => updateMutation.mutate(values))}>
            <div className="space-y-2"><Label htmlFor="account-name">{t("accounts.name")}</Label><Input id="account-name" {...form.register("name")} />{form.formState.errors.name ? <p className="text-xs text-destructive">{form.formState.errors.name.message}</p> : null}</div>
            <div className="flex items-center justify-between border-b py-2"><Label htmlFor="account-enabled">{accountEnabled ? t("common.enabled") : t("common.disabled")}</Label><Switch id="account-enabled" checked={accountEnabled} onCheckedChange={(checked) => form.setValue("enabled", checked)} /></div>
            <div className="grid gap-4 sm:grid-cols-2">
              <div className="space-y-2"><Label htmlFor="account-priority">{t("accounts.priority")}</Label><Input id="account-priority" type="number" {...form.register("priority", { valueAsNumber: true })} /></div>
              <div className="space-y-2"><Label htmlFor="account-concurrency">{t("accounts.maxConcurrent")}</Label><Input id="account-concurrency" type="number" min="1" max="256" {...form.register("maxConcurrent", { valueAsNumber: true })} /></div>
            </div>
            <div className="space-y-2"><Label htmlFor="account-minimum">{t("accounts.minimumRemaining")}</Label><Input id="account-minimum" type="number" min="0" step="0.01" {...form.register("minimumRemaining", { valueAsNumber: true })} /></div>
            <DialogFooter><Button type="button" variant="secondary" size="sm" onClick={() => setEditing(null)}>{t("common.cancel")}</Button><Button type="submit" size="sm" disabled={updateMutation.isPending}>{updateMutation.isPending ? <Spinner /> : null}{t("common.save")}</Button></DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      <AlertDialog open={Boolean(deleting)} onOpenChange={(open) => !open && setDeleting(null)}>
        <AlertDialogContent>
          <AlertDialogHeader><AlertDialogTitle>{t("accounts.deleteTitle")}</AlertDialogTitle><AlertDialogDescription>{t("accounts.deleteDescription")}</AlertDialogDescription></AlertDialogHeader>
          <AlertDialogFooter><AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel><AlertDialogAction className="bg-destructive text-white hover:bg-destructive/90" onClick={() => deleting && deleteMutation.mutate(deleting.id)}>{t("common.delete")}</AlertDialogAction></AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={batchDeleteOpen} onOpenChange={setBatchDeleteOpen}>
        <AlertDialogContent>
          <AlertDialogHeader><AlertDialogTitle>{t("accounts.batchDeleteTitle", { count: selected.size })}</AlertDialogTitle><AlertDialogDescription>{t("accounts.deleteDescription")}</AlertDialogDescription></AlertDialogHeader>
          <AlertDialogFooter><AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel><AlertDialogAction className="bg-destructive text-white hover:bg-destructive/90" onClick={() => batchDeleteMutation.mutate()}>{t("common.delete")}</AlertDialogAction></AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function downloadAccountExport(blob: Blob): void {
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = `grok2api-accounts-${new Date().toISOString().slice(0, 10)}.json`;
  anchor.click();
  window.setTimeout(() => URL.revokeObjectURL(url), 0);
}

function AccountMetricPanel({ icon, label, value, detail, loading }: { icon: ReactNode; label: string; value: string; detail: string; loading: boolean }) {
  return (
    <div className="min-h-28 rounded-lg bg-card p-4">
      <div className="flex items-center justify-between gap-3">
        <span className="text-xs text-muted-foreground">{label}</span>
        <span className="flex size-5 items-center justify-center text-muted-foreground [&_svg]:size-4">{icon}</span>
      </div>
      <div className="mt-3 flex min-h-7 items-center text-xl font-medium tabular-nums">{loading ? <Spinner /> : value}</div>
      <p className={cn("mt-1 text-xs text-muted-foreground", loading && "invisible")}>{detail}</p>
    </div>
  );
}

function AccountQuota({ quota, billing, locale }: { quota: QuotaDTO; billing?: BillingDTO; locale: string }) {
  const { t } = useTranslation();
  if (quota.type === "unknown") {
    return <span className="text-xs text-muted-foreground">{t("accounts.quotaUnknown")}</span>;
  }

  const isFree = quota.type === "free";
  if (!isFree) {
    return <BuildQuota quota={quota} billing={billing} locale={locale} />;
  }

  const percent = Math.min(100, Math.max(0, quota.usagePercent));
  const isWaitingReset = quota.status === "waitingReset";
  const isProbing = quota.status === "probing";
  const used = formatNumber(quota.used, locale, 0);
  const limit = formatNumber(quota.limit, locale, 0);
  const isEstimated = !quota.limitKnown;
  const statusDescription = isWaitingReset && quota.nextProbeAt
    ? t("accounts.waitingResetUntil", { time: formatDateTime(quota.nextProbeAt, locale) })
    : isProbing
      ? t("accounts.probingQuota")
      : quota.confirmed
        ? t("accounts.upstreamConfirmed")
        : null;
  const usage = quota.limit > 0
    ? isEstimated
      ? t("accounts.freeEstimatedUsage", { used, limit })
      : `${used} / ${limit} tokens`
    : t("accounts.freeObservedUsage", { used });
  return (
    <div className="w-full min-w-0 space-y-1.5">
      <div className="flex items-start justify-between gap-3 text-[11px] font-normal">
        <div className="inline-flex min-w-0 items-center gap-1 text-muted-foreground">
          <span>{usage}</span>
          {isEstimated ? (
            <Tooltip>
              <TooltipTrigger asChild>
                <button type="button" className="inline-flex shrink-0 text-muted-foreground transition-colors hover:text-foreground" aria-label={t("accounts.freeEstimatedDescription")}>
                  <Info className="size-3.5" />
                </button>
              </TooltipTrigger>
              <TooltipContent>{t("accounts.freeEstimatedDescription")}</TooltipContent>
            </Tooltip>
          ) : null}
        </div>
        <span className="shrink-0 text-muted-foreground">{isEstimated ? "≈" : ""}{formatNumber(quota.usagePercent, locale, 1)}%</span>
      </div>
      <div className="h-1.5 overflow-hidden rounded-full bg-muted">
        <div className="h-full bg-emerald-500" style={{ width: `${percent}%` }} />
      </div>
      {statusDescription ? <div className="text-[11px] text-muted-foreground">{statusDescription}</div> : null}
    </div>
  );
}

function BuildQuota({ quota, billing, locale }: { quota: QuotaDTO; billing?: BillingDTO; locale: string }) {
  const { t } = useTranslation();
  const hasWeekly = billing?.usagePeriodType === "USAGE_PERIOD_TYPE_WEEKLY";
  const hasMonthly = quota.limit > 0;
  if (!hasWeekly && !hasMonthly) {
    return <span className="text-xs text-muted-foreground">{t("accounts.paidQuotaUsage")}</span>;
  }

  const weeklyPercent = Math.max(0, Math.min(100, billing?.creditUsagePercent ?? 0));
  const weeklyRemainingPercent = 100 - weeklyPercent;
  const monthlyPercent = Math.max(0, Math.min(100, quota.usagePercent));
  const monthlyUsed = formatNumber(quota.used, locale, 2);
  const monthlyLimit = formatNumber(quota.limit, locale, 2);
  const monthlyRemaining = formatNumber(quota.remaining, locale, 2);
  const statusDescription = quota.status === "waitingReset" && quota.nextProbeAt
    ? t("accounts.paidWaitingResetUntil", { time: formatDateTime(quota.nextProbeAt, locale) })
    : quota.status === "probing"
      ? t("accounts.paidProbingQuota")
      : null;
  return (
    <div className="w-full min-w-0 space-y-1.5">
      <div className={cn("grid w-full min-w-0 divide-x divide-border/70", hasWeekly && hasMonthly ? "grid-cols-2" : "grid-cols-1")}>
        {hasWeekly ? (
          <Tooltip>
            <TooltipTrigger asChild>
              <button type="button" className="min-w-0 px-2 text-left font-normal first:pl-0 last:pr-0">
                <div className="flex items-center justify-between gap-1 text-[11px]">
                  <span className="truncate text-muted-foreground">{t("accounts.weeklyQuota")}</span>
                  <span className="shrink-0 tabular-nums">{formatNumber(weeklyPercent, locale, 1)}%</span>
                </div>
                <div className="mt-1.5 h-1.5 overflow-hidden rounded-full bg-muted"><div className="h-full bg-primary" style={{ width: `${weeklyPercent}%` }} /></div>
              </button>
            </TooltipTrigger>
            <TooltipContent>
              <div>{t("accounts.weeklyLimit", { percent: formatNumber(weeklyRemainingPercent, locale, 1) })}</div>
              <div className="text-muted-foreground">{billing?.usagePeriodEnd ? t("accounts.quotaResetAt", { time: formatDateTime(billing.usagePeriodEnd, locale) }) : t("accounts.quotaResetUnknown")}</div>
            </TooltipContent>
          </Tooltip>
        ) : null}
        {hasMonthly ? (
          <Tooltip>
            <TooltipTrigger asChild>
              <button type="button" className="min-w-0 px-2 text-left font-normal first:pl-0 last:pr-0">
                <div className="flex items-center justify-between gap-1 text-[11px]">
                  <span className="truncate text-muted-foreground">{t("accounts.monthlyQuota")}</span>
                  <span className="shrink-0 tabular-nums">{monthlyUsed}/{monthlyLimit}</span>
                </div>
                <div className="mt-1.5 h-1.5 overflow-hidden rounded-full bg-muted"><div className="h-full bg-primary" style={{ width: `${monthlyPercent}%` }} /></div>
              </button>
            </TooltipTrigger>
            <TooltipContent>
              <div>{t("accounts.paidQuotaDetails", { remaining: monthlyRemaining })}</div>
              <div className="text-muted-foreground">{billing?.billingPeriodEnd ? t("accounts.quotaResetAt", { time: formatDateTime(billing.billingPeriodEnd, locale) }) : t("accounts.quotaResetUnknown")}</div>
            </TooltipContent>
          </Tooltip>
        ) : null}
      </div>
      {statusDescription ? <div className="text-[11px] text-muted-foreground">{statusDescription}</div> : null}
    </div>
  );
}

const visibleWebQuotaModes = ["auto", "fast", "expert", "heavy"] as const;

function WebQuota({ windows, locale, tier }: { windows: NonNullable<AccountDTO["quotaWindows"]>; locale: string; tier?: AccountDTO["webTier"] }) {
  const { t } = useTranslation();
  if (windows.length === 0) return <span className="text-xs text-muted-foreground">{t("accounts.quotaNotSynced")}</span>;
  const windowsByMode = new Map(windows.map((window) => [window.mode, window]));
  const weekly = windowsByMode.get("weekly");
  if (weekly) {
    const usedPercent = Math.max(0, Math.min(100, weekly.usagePercent));
    const remainingPercent = 100 - usedPercent;
    const breakdown = (weekly.breakdown ?? []).filter((item) => item.usagePercent > 0);
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <div className="min-w-0">
            <div className="flex items-center justify-between gap-2 text-[11px]">
              {breakdown.length > 0 ? <div className="flex min-w-0 items-center gap-2.5 overflow-hidden text-muted-foreground">
                {breakdown.slice(0, 3).map((item) => (
                  <span key={item.productCode} className="flex shrink-0 items-center gap-1">
                    <span className={`size-1.5 rounded-full ${quotaProductColor(item.productCode)}`} />
                    <span>{quotaProductLabel(item.productCode, locale)}</span>
                    <span className="tabular-nums text-foreground">{formatNumber(item.usagePercent, locale, 1)}%</span>
                  </span>
                ))}
                {breakdown.length > 3 ? <span className="shrink-0">+{breakdown.length - 3}</span> : null}
              </div> : <span className="truncate text-muted-foreground">{t("accounts.weeklyQuota")}</span>}
              <span className="shrink-0 tabular-nums">{formatNumber(usedPercent, locale, 1)}%</span>
            </div>
            <div className="mt-1.5 flex h-1.5 overflow-hidden rounded-full bg-muted">
              {breakdown.length > 0 ? breakdown.map((item) => (
                <div key={item.productCode} className={`h-full shrink-0 ${quotaProductColor(item.productCode)}`} style={{ width: `${Math.max(0, Math.min(100, item.usagePercent))}%` }} />
              )) : <div className="h-full bg-primary" style={{ width: `${usedPercent}%` }} />}
            </div>
          </div>
        </TooltipTrigger>
        <TooltipContent>
          <div>{t("accounts.webWeeklyQuotaUsage", { remaining: formatNumber(remainingPercent, locale, 1) })}</div>
          <div className="text-muted-foreground">{weekly.resetAt ? t("accounts.quotaResetAt", { time: formatDateTime(weekly.resetAt, locale) }) : t("accounts.quotaResetUnknown")}</div>
          {breakdown.length > 0 ? <div className="mt-2 grid gap-1 border-t pt-2">{breakdown.map((item) => (
            <div key={item.productCode} className="flex items-center justify-between gap-4">
              <span className="flex items-center gap-1.5"><span className={`size-2 rounded-full ${quotaProductColor(item.productCode)}`} />{quotaProductLabel(item.productCode, locale)}</span>
              <span className="tabular-nums">{formatNumber(item.usagePercent, locale, 1)}%</span>
            </div>
          ))}</div> : null}
        </TooltipContent>
      </Tooltip>
    );
  }
  const fast = windowsByMode.get("fast");
  if (tier === "basic" && fast) {
    const used = Math.max(0, fast.total - fast.remaining);
    const percent = fast.total > 0 ? Math.max(0, Math.min(100, used / fast.total * 100)) : 0;
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <div className="min-w-0">
            <div className="flex items-center justify-between gap-2 text-[11px]">
              <span className="truncate text-muted-foreground">Fast</span>
              <span className="shrink-0 tabular-nums">{formatNumber(used, locale, 0)}/{formatNumber(fast.total, locale, 0)}</span>
            </div>
            <div className="mt-1.5 h-1.5 overflow-hidden rounded-full bg-muted"><div className="h-full bg-primary" style={{ width: `${percent}%` }} /></div>
          </div>
        </TooltipTrigger>
        <TooltipContent>
          <div>{t("accounts.webModeQuotaRemaining", { mode: "Fast", remaining: formatNumber(fast.remaining, locale, 0) })}</div>
          <div className="text-muted-foreground">{fast.resetAt ? t("accounts.quotaResetAt", { time: formatDateTime(fast.resetAt, locale) }) : t("accounts.quotaResetUnknown")}</div>
        </TooltipContent>
      </Tooltip>
    );
  }
  return (
    <div className="grid w-full min-w-0 grid-cols-4 divide-x divide-border/70">
      {visibleWebQuotaModes.map((mode) => {
        const window = windowsByMode.get(mode);
        if (!window) {
          return (
            <div key={mode} className="min-w-0 px-2 first:pl-0 last:pr-0">
              <div className="flex items-center justify-between gap-1 text-[11px]">
                <span className="truncate capitalize text-muted-foreground">{mode}</span>
                <span className="text-muted-foreground">-</span>
              </div>
              <div className="mt-1.5 h-1.5 rounded-full bg-muted" />
            </div>
          );
        }
        const used = Math.max(0, window.total - window.remaining);
        const percent = window.total > 0 ? Math.max(0, Math.min(100, used / window.total * 100)) : 0;
        return (
          <Tooltip key={mode}>
            <TooltipTrigger asChild>
              <div className="min-w-0 px-2 first:pl-0 last:pr-0">
                <div className="flex items-center justify-between gap-1 text-[11px]">
                  <span className="truncate capitalize text-muted-foreground">{mode}</span>
                  <span className="shrink-0 tabular-nums">{formatNumber(used, locale, 0)}/{formatNumber(window.total, locale, 0)}</span>
                </div>
                <div className="mt-1.5 h-1.5 overflow-hidden rounded-full bg-muted"><div className="h-full bg-primary" style={{ width: `${percent}%` }} /></div>
              </div>
            </TooltipTrigger>
            <TooltipContent>
              <div>{t("accounts.webModeQuotaRemaining", { mode: formatWebQuotaMode(mode), remaining: formatNumber(window.remaining, locale, 0) })}</div>
              <div className="text-muted-foreground">{window.resetAt ? t("accounts.quotaResetAt", { time: formatDateTime(window.resetAt, locale) }) : t("accounts.quotaResetUnknown")}</div>
            </TooltipContent>
          </Tooltip>
        );
      })}
    </div>
  );
}

function webTierLabel(tier: AccountDTO["webTier"]) {
  if (tier === "basic") return "Free";
  if (tier === "super") return "Super";
  if (tier === "heavy") return "Heavy";
  return "Auto";
}

function WebAccountType({ tier }: { tier?: AccountDTO["webTier"] }) {
  const label = webTierLabel(tier);
  return <AccountTypeText label={label} variant={tier === "basic" ? "free" : "default"} />;
}

function formatWebQuotaMode(mode: string) {
  return mode ? mode.charAt(0).toUpperCase() + mode.slice(1) : mode;
}

function quotaProductLabel(code: number, locale: string) {
  const chinese = locale.toLowerCase().startsWith("zh");
  const labels: Record<number, [string, string]> = {
    0: ["第三方", "Third Party"],
    1: ["API", "API"],
    2: ["Grok Build", "Grok Build"],
    3: ["Grok Plugins", "Grok Plugins"],
    4: ["聊天", "Chat"],
    5: ["Imagine", "Imagine"],
    6: ["语音", "Voice"],
  };
  const label = labels[code];
  return label ? label[chinese ? 0 : 1] : `${chinese ? "产品" : "Product"} ${code}`;
}

function quotaProductColor(code: number) {
  const colors: Record<number, string> = {
    0: "bg-zinc-500",
    1: "bg-sky-500",
    2: "bg-emerald-500",
    3: "bg-amber-500",
    4: "bg-indigo-400",
    5: "bg-blue-600",
    6: "bg-rose-500",
  };
  return colors[code] ?? "bg-muted-foreground";
}

function AccountType({ quota }: { quota: QuotaDTO }) {
  const { t } = useTranslation();
  if (quota.type === "unknown") {
    return <AccountTypeText label={t("dashboard.unknown")} variant="muted" />;
  }

  const isFree = quota.type === "free";
  const label = isFree ? t("accounts.quotaFree") : t("accounts.quotaSuper");
  return <AccountTypeText label={label} variant={isFree ? "free" : "default"} />;
}

function AccountTypeText({ label, variant }: { label: string; variant: "default" | "free" | "muted" }) {
  if (variant === "muted") {
    return <span className="text-xs text-muted-foreground">{label}</span>;
  }
  return <span title={label} className={cn("max-w-32 truncate text-xs font-medium", variant === "free" ? "text-emerald-700 dark:text-emerald-300" : "text-primary")}>{label}</span>;
}

function AccountStatus({ account }: { account: AccountDTO }) {
  const { t } = useTranslation();
  if (!account.enabled) {
    return <Badge variant="outline" className="text-muted-foreground">{t("accounts.statusDisabled")}</Badge>;
  }
  if (account.authStatus === "reauthRequired") {
    return <Badge variant="destructive">{t("accounts.statusReauthRequired")}</Badge>;
  }
  if (account.quota.status === "waitingReset") {
    return <Badge variant="secondary" className="bg-amber-500/10 text-amber-700 dark:text-amber-300">{t("accounts.waitingReset")}</Badge>;
  }
  if (account.quota.status === "probing") {
    return <Badge variant="secondary" className="bg-sky-500/10 text-sky-700 dark:text-sky-300">{t("accounts.probing")}</Badge>;
  }
  if (account.cooldownUntil && new Date(account.cooldownUntil) > new Date()) {
    return <Badge variant="secondary" className="bg-amber-500/10 text-amber-700 dark:text-amber-300">{t("accounts.statusCooldown")}</Badge>;
  }
  return <Badge variant="secondary" className="bg-emerald-500/10 text-emerald-700 dark:text-emerald-300">{t("accounts.statusActive")}</Badge>;
}
