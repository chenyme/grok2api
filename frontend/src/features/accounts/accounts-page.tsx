import { zodResolver } from "@hookform/resolvers/zod";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowRight, ClipboardPaste, Compass, Download, ExternalLink, FileUp, Link2, MoreHorizontal, Pencil, RefreshCw, RotateCw, Search, SquareTerminal, Trash2, TriangleAlert, Webhook } from "lucide-react";
import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { useForm, useWatch } from "react-hook-form";
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
  importConsoleAccounts,
  importWebAccounts,
  listAccounts,
  pollDeviceAuthorization,
  refreshAccountBilling,
  refreshAccountsQuota,
  refreshAccountToken,
  refreshAccountQuota,
  refreshAllAccountBilling,
  refreshAllAccountTokens,
  refreshAllConsoleAccountQuotas,
  refreshAllWebAccountQuotas,
  startDeviceAuthorization,
  syncWebAccountsToConsole,
  updateAccount,
  updateAccountsEnabled,
  type AccountDTO,
  type AccountProvider,
  type AccountUpdateInput,
  type AccountTaskProgressDTO,
  type BuildConversionInput,
  type BuildConversionStrategy,
  type WebConsoleSyncInput,
  type DeviceSessionDTO,
  type QuotaDTO,
} from "@/features/accounts/accounts-api";
import { AccountQuota, ConsoleQuota, WebQuota } from "@/features/accounts/account-quota";

function isAbortError(error: unknown): boolean {
  return (error instanceof DOMException || error instanceof Error) && error.name === "AbortError";
}

type BuildConversionProgressState = {
  converting?: AccountTaskProgressDTO;
  syncing?: AccountTaskProgressDTO;
};

export function AccountsPage() {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const fileInputRef = useRef<HTMLInputElement>(null);
  const quotaSyncAbortRef = useRef<AbortController | null>(null);
  const renewalAbortRef = useRef<AbortController | null>(null);
  const conversionAbortRef = useRef<AbortController | null>(null);
  const webConsoleSyncAbortRef = useRef<AbortController | null>(null);
  const importAbortRef = useRef<AbortController | null>(null);
  const importToastRef = useRef<string | number | null>(null);
  const [provider, setProvider] = useState<AccountProvider>("grok_build");
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
  const [quotaSyncProgress, setQuotaSyncProgress] = useState<AccountTaskProgressDTO | null>(null);
  const [conversionTargets, setConversionTargets] = useState<string[] | "all" | null>(null);
  const [conversionStrategy, setConversionStrategy] = useState<BuildConversionStrategy>("missing");
  const [conversionProgress, setConversionProgress] = useState<BuildConversionProgressState | null>(null);
  const [webConsoleSyncTargets, setWebConsoleSyncTargets] = useState<string[] | "all" | null>(null);
  const [webConsoleSyncStrategy, setWebConsoleSyncStrategy] = useState<"missing" | "all">("missing");
  const [webConsoleSyncProgress, setWebConsoleSyncProgress] = useState<AccountTaskProgressDTO | null>(null);
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
    quotaSyncAbortRef.current?.abort();
    renewalAbortRef.current?.abort();
    conversionAbortRef.current?.abort();
    webConsoleSyncAbortRef.current?.abort();
    importAbortRef.current?.abort();
    if (importToastRef.current !== null) toast.dismiss(importToastRef.current);
  }, []);

  const accountSchema = z.object({
    name: z.string().min(1, t("errors.required")),
    enabled: z.boolean(),
    priority: z.number().int(),
    maxConcurrent: z.number().int().min(1, t("errors.positive")).max(256),
    minimumRemaining: z.number().min(0),
    cloudflareCookies: z.string().max(16 << 10, t("settings.invalidValue")),
    clearCloudflareCookies: z.boolean(),
  });
  type AccountForm = z.infer<typeof accountSchema>;
  const form = useForm<AccountForm>({
    resolver: zodResolver(accountSchema),
    defaultValues: { name: "", enabled: true, priority: 1, maxConcurrent: 8, minimumRemaining: 0, cloudflareCookies: "", clearCloudflareCookies: false },
  });
  const accountEnabled = useWatch({ control: form.control, name: "enabled" });
  const clearCloudflareCookies = useWatch({ control: form.control, name: "clearCloudflareCookies" });

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
      const input: AccountUpdateInput = {
        name: values.name,
        enabled: values.enabled,
        priority: values.priority,
        maxConcurrent: values.maxConcurrent,
        minimumRemaining: values.minimumRemaining,
      };
      if (editing.provider !== "grok_build") {
        if (values.clearCloudflareCookies) input.clearCloudflareCookies = true;
        else if (values.cloudflareCookies.trim()) input.cloudflareCookies = values.cloudflareCookies;
      }
      return updateAccount(editing.id, input);
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

  const quotaSyncMutation = useMutation({
    mutationFn: (targetProvider: AccountProvider) => {
      const controller = new AbortController();
      quotaSyncAbortRef.current = controller;
      setQuotaSyncProgress(null);
      if (targetProvider === "grok_web") return refreshAllWebAccountQuotas(setQuotaSyncProgress, controller.signal);
      if (targetProvider === "grok_console") return refreshAllConsoleAccountQuotas(setQuotaSyncProgress, controller.signal);
      return refreshAllAccountBilling(setQuotaSyncProgress, controller.signal);
    },
    onSuccess: (result) => {
      setSyncAllOpen(false);
      toast.success(t("accounts.allBillingRefreshed", result));
    },
    onError: (error) => { if (!isAbortError(error)) showError(error); },
    onSettled: () => { quotaSyncAbortRef.current = null; setQuotaSyncProgress(null); invalidateAccountData(); },
  });
  const conversionMutation = useMutation({
    mutationFn: (input: BuildConversionInput) => {
      const controller = new AbortController();
      conversionAbortRef.current = controller;
      setConversionProgress(null);
      return convertWebAccountsToBuild(input, (progress) => {
        const phase = progress.phase === "syncing" ? "syncing" : "converting";
        setConversionProgress((current) => ({ ...(current ?? {}), [phase]: progress }));
      }, controller.signal);
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

  const webConsoleSyncMutation = useMutation({
    mutationFn: (input: WebConsoleSyncInput) => {
      const controller = new AbortController();
      webConsoleSyncAbortRef.current = controller;
      setWebConsoleSyncProgress(null);
      return syncWebAccountsToConsole(input, setWebConsoleSyncProgress, controller.signal);
    },
    onSuccess: (result) => {
      setWebConsoleSyncTargets(null);
      setSelected(new Set());
      toast.success(t("webConsoleSync.completed", result));
    },
    onError: (error) => { if (!isAbortError(error)) showError(error); },
    onSettled: () => {
      webConsoleSyncAbortRef.current = null;
      setWebConsoleSyncProgress(null);
      invalidateAccountData();
      void queryClient.invalidateQueries({ queryKey: ["models"] });
    },
  });

  const importMutation = useMutation({
    mutationFn: (files: File[]) => {
      const controller = new AbortController();
      importAbortRef.current = controller;
      const toastID = toast.loading(t("common.importingProgress", { completed: 0, total: "…" }));
      importToastRef.current = toastID;
      const onProgress = (progress: AccountTaskProgressDTO) => {
        toast.loading(t(progress.phase === "syncing" ? "common.syncingProgress" : "common.importingProgress", progress), { id: toastID });
      };
      if (provider === "grok_web") return importWebAccounts(files, onProgress, controller.signal);
      if (provider === "grok_console") return importConsoleAccounts(files, onProgress, controller.signal);
      return importAccounts(files, onProgress, controller.signal);
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
    mutationFn: () => refreshAccountsQuota([...selected], provider),
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

  function changeProvider(value: AccountProvider) {
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
    const filename = provider === "grok_console" ? "grok-console-sso-tokens.txt" : "grok-web-sso-tokens.txt";
    importMutation.mutate([new File([value], filename, { type: "text/plain" })]);
  }

  function openWebConsoleSync(targets: string[] | "all"): void {
    setWebConsoleSyncStrategy("missing");
    setWebConsoleSyncTargets(targets);
  }

  function openBuildConversion(targets: string[] | "all"): void {
    setConversionStrategy("missing");
    setConversionTargets(targets);
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
      cloudflareCookies: "",
      clearCloudflareCookies: false,
    });
  }

  const convertingProgress = conversionProgress?.converting;
  const syncingProgress = conversionProgress?.syncing;
  const activeConversionProgress = convertingProgress?.completed === convertingProgress?.total && syncingProgress
    ? syncingProgress
    : convertingProgress ?? syncingProgress;

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
  const recoveringAccounts = summary?.recovering ?? 0;
  const attentionAccounts = summary?.attention ?? 0;
  const abnormalAccounts = recoveringAccounts + attentionAccounts;
  const buildSummary = summary?.providers.grok_build ?? { total: 0, available: 0 };
  const webSummary = summary?.providers.grok_web ?? { total: 0, available: 0 };
  const consoleSummary = summary?.providers.grok_console ?? { total: 0, available: 0 };
  const summaryLoading = summaryQuery.isPending;
  const summaryUnavailable = summaryQuery.isError;
  const providerAccountTotal = provider === "grok_build" ? buildSummary.total : provider === "grok_web" ? webSummary.total : consoleSummary.total;
  const hasProviderAccounts = providerAccountTotal > 0 || (result?.total ?? 0) > 0;
  const bulkTaskPending = quotaSyncMutation.isPending
    || allTokenMutation.isPending
    || conversionMutation.isPending
    || webConsoleSyncMutation.isPending
    || importMutation.isPending
    || batchUpdateMutation.isPending
    || batchBillingMutation.isPending
    || batchDeleteMutation.isPending;

  return (
    <div className="space-y-8">
      <header>
        <h1 className="text-xl font-medium">{t("accounts.title")}</h1>
        <p className="sr-only">{t("console.accountsDescription")}</p>
      </header>
      <section className="grid gap-2 sm:grid-cols-2 xl:grid-cols-4">
        <AccountMetricPanel icon={<SquareTerminal />} loading={summaryLoading} label={t("accounts.buildAccountCount")} value={summaryUnavailable ? "-" : formatNumber(buildSummary.total, i18n.language, 0)} detail={t("accounts.routableAccountCount", { count: formatNumber(buildSummary.available, i18n.language, 0) })} />
        <AccountMetricPanel icon={<Compass />} loading={summaryLoading} label={t("accounts.webAccountCount")} value={summaryUnavailable ? "-" : formatNumber(webSummary.total, i18n.language, 0)} detail={t("accounts.routableAccountCount", { count: formatNumber(webSummary.available, i18n.language, 0) })} />
        <AccountMetricPanel icon={<Webhook />} loading={summaryLoading} label={t("accounts.consoleAccountCount")} value={summaryUnavailable ? "-" : formatNumber(consoleSummary.total, i18n.language, 0)} detail={t("accounts.routableAccountCount", { count: formatNumber(consoleSummary.available, i18n.language, 0) })} />
        <AccountMetricPanel icon={<TriangleAlert />} loading={summaryLoading} label={t("accounts.abnormalAccountCount")} value={summaryUnavailable ? "-" : formatNumber(abnormalAccounts, i18n.language, 0)} detail={t("accounts.abnormalAccountBreakdown", { recovering: formatNumber(recoveringAccounts, i18n.language, 0), attention: formatNumber(attentionAccounts, i18n.language, 0) })} />
      </section>
      <div className="space-y-6">
        <Tabs value={provider} onValueChange={(value) => changeProvider(value as AccountProvider)}>
          <TabsList>
            <TabsTrigger value="grok_build">Grok Build</TabsTrigger>
            <TabsTrigger value="grok_web">Grok Web</TabsTrigger>
            <TabsTrigger value="grok_console">Grok Console</TabsTrigger>
          </TabsList>
        </Tabs>
        <input
          ref={fileInputRef}
          type="file"
          multiple
          accept={provider === "grok_build" ? "application/json,.json" : "application/json,text/plain,.json,.txt"}
          className="hidden"
          onChange={(event) => {
            const files = Array.from(event.target.files ?? []);
            if (files.length > 0) {
              importMutation.mutate(files);
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
                ...(provider === "grok_console" ? [] : [{ id: "type", label: t("accountType.label"), value: typeFilter, onChange: (value: string) => { setTypeFilter(value); setPage(1); }, options: provider === "grok_web" ? [
                  { value: "auto", label: t("accountType.auto") },
                  { value: "basic", label: t("accountType.free") },
                  { value: "super", label: t("accountType.super") },
                  { value: "heavy", label: t("accountType.heavy") },
                ] : [
                  { value: "free", label: t("accountType.free") },
                  { value: "paid", label: t("accountType.paid") },
                  { value: "unknown", label: t("accountType.pending") },
                ] }]),
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
                <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => batchUpdateMutation.mutate(true)}>{t("common.enable")}</Button>
                <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => batchUpdateMutation.mutate(false)}>{t("common.disable")}</Button>
                {provider === "grok_web" ? <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => openBuildConversion([...selected])}>{t("accounts.convertToBuild")}</Button> : null}
                {provider === "grok_web" ? <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => openWebConsoleSync([...selected])}>{t("webConsoleSync.action")}</Button> : null}
                <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => batchBillingMutation.mutate()}>{t("accountCredential.quotaSyncAction")}</Button>
                <Button variant="ghost" size="sm" className="text-destructive hover:text-destructive" disabled={bulkTaskPending} onClick={() => setBatchDeleteOpen(true)}>{t("common.delete")}</Button>
              </div>
            ) : (
              <div className="flex items-center gap-1.5">
                {provider === "grok_web" && webSummary.total > 0 ? <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => openBuildConversion("all")}>{t("accountBulk.convertAllToBuild")}</Button> : null}
                {provider === "grok_web" && webSummary.total > 0 ? <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => openWebConsoleSync("all")}>{t("webConsoleSync.allAction")}</Button> : null}
                {hasProviderAccounts ? <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => setSyncAllOpen(true)}>{t("accountCredential.quotaSyncAction")}</Button> : null}
                {hasProviderAccounts && provider === "grok_build" ? <Button variant="secondary" size="sm" disabled={bulkTaskPending} onClick={() => setRenewAllOpen(true)}>{t("accountCredential.refreshAction")}</Button> : null}
                <DropdownMenu>
                  <DropdownMenuTrigger asChild><Button size="sm">{t("accounts.connectAccount")}</Button></DropdownMenuTrigger>
                  <DropdownMenuContent align="end">
                    {provider === "grok_build" ? <DropdownMenuItem onClick={() => void startDeviceLogin()}><ExternalLink />{t("accounts.deviceLogin")}</DropdownMenuItem> : null}
                    {provider !== "grok_build" ? <DropdownMenuItem disabled={bulkTaskPending} onClick={() => setQuickImportOpen(true)}><ClipboardPaste />{t("accounts.quickImportSSO")}</DropdownMenuItem> : null}
                    <DropdownMenuItem disabled={bulkTaskPending} onClick={() => fileInputRef.current?.click()}><FileUp />{provider === "grok_build" ? t("accounts.importAuth") : provider === "grok_console" ? t("console.importFile") : t("accounts.importWebFile")}</DropdownMenuItem>
                    {hasProviderAccounts && provider === "grok_build" ? (
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
              <col style={{ width: provider === "grok_build" ? "30%" : "46%" }} />
              {provider === "grok_build" ? <col style={{ width: "16%" }} /> : null}
              <col style={{ width: "18%" }} />
              <col style={{ width: "4%" }} />
            </colgroup>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="px-2"><Checkbox checked={allPageSelected ? true : selectedOnPage.length > 0 ? "indeterminate" : false} onCheckedChange={(checked) => togglePage(checked === true)} aria-label={t("common.selectPage")} /></TableHead>
                <SortableTableHead field="name" sortBy={sort.field} sortOrder={sort.order} onSort={changeSort}>{t("accounts.account")}</SortableTableHead>
                <SortableTableHead field="type" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort} className="whitespace-nowrap">{t("accountType.label")}</SortableTableHead>
                <SortableTableHead field="status" sortBy={sort.field} sortOrder={sort.order} align="center" onSort={changeSort} className="whitespace-nowrap">{t("accounts.status")}</SortableTableHead>
                <TableHead className="whitespace-nowrap">{t("accounts.quota")}</TableHead>
                {provider === "grok_build" ? <TableHead className="whitespace-nowrap pl-4">{t("accountCredential.label")}</TableHead> : null}
                <SortableTableHead field="createdAt" sortBy={sort.field} sortOrder={sort.order} initialOrder="desc" onSort={changeSort} className="whitespace-nowrap">{t("accounts.createdAt")}</SortableTableHead>
                <TableActionHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {accountsQuery.isPending ? <TableLoadingRow colSpan={provider === "grok_build" ? 8 : 7} /> : result?.items.map((account) => {
                const accountDetail = account.email ?? account.userId ?? account.teamId;
                const showAccountDetail = accountDetail?.trim().toLocaleLowerCase() !== account.name.trim().toLocaleLowerCase();
                const linkedProviderLabel = account.linkedProvider === "grok_build" ? t("models.providerGrokBuild") : account.linkedProvider === "grok_web" ? t("models.providerGrokWeb") : t("console.name");
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
                    <TableCell className="text-center whitespace-nowrap">{provider === "grok_web" ? <WebAccountType tier={account.webTier} /> : provider === "grok_console" ? <AccountTypeText label={t("accountType.console")} variant="free" /> : <AccountType quota={account.quota} />}</TableCell>
                    <TableCell className="text-center whitespace-nowrap"><AccountStatus account={account} /></TableCell>
                    <TableCell>{provider === "grok_web" ? <WebQuota windows={account.quotaWindows ?? []} locale={i18n.language} tier={account.webTier} /> : provider === "grok_console" ? <ConsoleQuota windows={account.quotaWindows ?? []} locale={i18n.language} /> : <AccountQuota quota={account.quota} billing={account.billing} locale={i18n.language} />}</TableCell>
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
                          {provider === "grok_web" ? <DropdownMenuItem onClick={() => openBuildConversion([account.id])}><ArrowRight />{t("accounts.convertToBuild")}</DropdownMenuItem> : null}
                          {provider === "grok_web" ? <DropdownMenuItem onClick={() => openWebConsoleSync([account.id])}><ArrowRight />{t("webConsoleSync.action")}</DropdownMenuItem> : null}
                          {provider === "grok_build" ? <DropdownMenuItem onClick={() => tokenMutation.mutate(account.id)}><RotateCw />{t("accounts.refreshToken")}</DropdownMenuItem> : null}
                          <DropdownMenuItem onClick={() => provider === "grok_build" ? billingMutation.mutate(account.id) : quotaMutation.mutate(account.id)}><RefreshCw />{provider === "grok_build" ? t("accounts.refreshBilling") : t("accounts.refreshModeQuota")}</DropdownMenuItem>
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

      <AlertDialog open={syncAllOpen} onOpenChange={(open) => { if (!open) quotaSyncAbortRef.current?.abort(); setSyncAllOpen(open); }}>
        <AlertDialogContent>
          <AlertDialogHeader><AlertDialogTitle>{t("accounts.syncAllTitle")}</AlertDialogTitle><AlertDialogDescription>{t(provider === "grok_web" ? "accounts.syncAllWebDescription" : provider === "grok_console" ? "console.syncAllDescription" : "accounts.syncAllDescription")}</AlertDialogDescription></AlertDialogHeader>
          <AlertDialogFooter><AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel><AlertDialogAction disabled={quotaSyncMutation.isPending} onClick={() => quotaSyncMutation.mutate(provider)}>{quotaSyncMutation.isPending ? <><Spinner />{quotaSyncProgress ? <span className="tabular-nums">{quotaSyncProgress.completed} / {quotaSyncProgress.total}</span> : t("common.loading")}</> : t("accounts.syncAll")}</AlertDialogAction></AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={webConsoleSyncTargets !== null} onOpenChange={(open) => { if (!open) { webConsoleSyncAbortRef.current?.abort(); setWebConsoleSyncTargets(null); } }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t(webConsoleSyncTargets === "all" ? "webConsoleSync.allTitle" : "webConsoleSync.selectedTitle")}</AlertDialogTitle>
            <AlertDialogDescription>{t(webConsoleSyncTargets === "all" ? "webConsoleSync.allDescription" : "webConsoleSync.selectedDescription")}</AlertDialogDescription>
          </AlertDialogHeader>
          <div className="space-y-2">
            <p id="web-console-sync-strategy" className="text-xs font-medium">{t("webConsoleSync.strategyTitle")}</p>
            <div role="radiogroup" aria-labelledby="web-console-sync-strategy" className="grid grid-cols-2 rounded-md bg-muted p-1">
              <Button type="button" role="radio" aria-checked={webConsoleSyncStrategy === "missing"} variant={webConsoleSyncStrategy === "missing" ? "secondary" : "ghost"} size="sm" className="h-8 rounded-sm px-3 text-xs font-normal shadow-none" onClick={() => setWebConsoleSyncStrategy("missing")}>{t("webConsoleSync.missingStrategy")}</Button>
              <Button type="button" role="radio" aria-checked={webConsoleSyncStrategy === "all"} variant={webConsoleSyncStrategy === "all" ? "secondary" : "ghost"} size="sm" className="h-8 rounded-sm px-3 text-xs font-normal shadow-none" onClick={() => setWebConsoleSyncStrategy("all")}>{t("webConsoleSync.allStrategy")}</Button>
            </div>
            <p className="min-h-8 text-xs text-muted-foreground">{t(webConsoleSyncStrategy === "missing" ? "webConsoleSync.missingStrategyDescription" : "webConsoleSync.allStrategyDescription")}</p>
          </div>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction disabled={webConsoleSyncMutation.isPending || webConsoleSyncTargets === null || (Array.isArray(webConsoleSyncTargets) && webConsoleSyncTargets.length === 0)} onClick={(event) => { event.preventDefault(); if (webConsoleSyncTargets === "all") webConsoleSyncMutation.mutate({ all: true, strategy: webConsoleSyncStrategy }); else if (webConsoleSyncTargets) webConsoleSyncMutation.mutate({ ids: webConsoleSyncTargets, strategy: webConsoleSyncStrategy }); }}>
              {webConsoleSyncMutation.isPending ? <><Spinner />{webConsoleSyncProgress ? <span className="tabular-nums">{webConsoleSyncProgress.completed} / {webConsoleSyncProgress.total}</span> : t("common.loading")}</> : t(webConsoleSyncStrategy === "missing" ? "webConsoleSync.syncMissing" : "webConsoleSync.syncAll")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={conversionTargets !== null} onOpenChange={(open) => { if (!open) { conversionAbortRef.current?.abort(); setConversionTargets(null); } }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t(conversionTargets === "all" ? "accountBulk.convertAllToBuildTitle" : "accounts.convertToBuildTitle")}</AlertDialogTitle>
            <AlertDialogDescription>{t(conversionTargets === "all" ? "accountBulk.convertAllToBuildDescription" : "accountBulk.selectedDescription")}</AlertDialogDescription>
          </AlertDialogHeader>
          <div className="space-y-2">
            <p id="web-build-conversion-strategy" className="text-xs font-medium">{t("accountBulk.strategyTitle")}</p>
            <div role="radiogroup" aria-labelledby="web-build-conversion-strategy" className="grid grid-cols-2 rounded-md bg-muted p-1">
              <Button type="button" role="radio" aria-checked={conversionStrategy === "missing"} variant={conversionStrategy === "missing" ? "secondary" : "ghost"} size="sm" className="h-8 rounded-sm px-3 text-xs font-normal shadow-none" onClick={() => setConversionStrategy("missing")}>{t("accountBulk.missingStrategy")}</Button>
              <Button type="button" role="radio" aria-checked={conversionStrategy === "all"} variant={conversionStrategy === "all" ? "secondary" : "ghost"} size="sm" className="h-8 rounded-sm px-3 text-xs font-normal shadow-none" onClick={() => setConversionStrategy("all")}>{t("accountBulk.allStrategy")}</Button>
            </div>
            <p className="min-h-8 text-xs text-muted-foreground">{t(conversionStrategy === "missing" ? "accountBulk.missingStrategyDescription" : "accountBulk.allStrategyDescription")}</p>
          </div>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction disabled={conversionMutation.isPending || conversionTargets === null || (Array.isArray(conversionTargets) && conversionTargets.length === 0)} onClick={(event) => { event.preventDefault(); if (conversionTargets === "all") conversionMutation.mutate({ all: true, strategy: conversionStrategy }); else if (conversionTargets) conversionMutation.mutate({ ids: conversionTargets, strategy: conversionStrategy }); }}>
              {conversionMutation.isPending ? <><Spinner />{activeConversionProgress ? <span className="whitespace-nowrap tabular-nums">{t(activeConversionProgress.phase === "syncing" ? "accounts.syncingProgress" : "accounts.convertingProgress", activeConversionProgress)}</span> : t("common.loading")}</> : t(conversionStrategy === "missing" ? "accountBulk.convertMissing" : "accountBulk.convertAll")}
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
                  <CopyButton value={deviceSession.userCode} className="absolute right-1.5 top-1/2 size-8 -translate-y-1/2 rounded-md" onCopied={() => toast.success(t("common.copied"))} />
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
            <DialogTitle>{t(provider === "grok_console" ? "console.quickImportTitle" : "accounts.quickImportTitle")}</DialogTitle>
            <DialogDescription>{t(provider === "grok_console" ? "console.quickImportDescription" : "accounts.quickImportDescription")}</DialogDescription>
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
            {editing && editing.provider !== "grok_build" ? (
              <div className="space-y-2">
                <Label htmlFor="account-cloudflare-cookie">{t("settings.egress.cloudflareCookie")}</Label>
                <Textarea
                  id="account-cloudflare-cookie"
                  className="min-h-20 font-mono text-xs"
                  autoComplete="new-password"
                  spellCheck={false}
                  disabled={clearCloudflareCookies}
                  placeholder={editing?.cloudflareCookieConfigured ? t("settings.egress.keepConfigured") : "cf_clearance=..."}
                  {...form.register("cloudflareCookies")}
                />
                {editing?.cloudflareCookieConfigured ? (
                  <label className="flex items-center gap-2 text-xs text-muted-foreground">
                    <Checkbox checked={clearCloudflareCookies} onCheckedChange={(checked) => form.setValue("clearCloudflareCookies", checked === true)} />
                    {t("common.clear")}
                  </label>
                ) : null}
                {form.formState.errors.cloudflareCookies ? <p className="text-xs text-destructive">{form.formState.errors.cloudflareCookies.message}</p> : null}
              </div>
            ) : null}
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

function WebAccountType({ tier }: { tier?: AccountDTO["webTier"] }) {
  const { t } = useTranslation();
  const label = tier === "basic" ? t("accountType.free") : tier === "super" ? t("accountType.super") : tier === "heavy" ? t("accountType.heavy") : t("accountType.auto");
  return <AccountTypeText label={label} variant={tier === "basic" ? "free" : "default"} />;
}

function AccountType({ quota }: { quota: QuotaDTO }) {
  const { t } = useTranslation();
  if (quota.type === "unknown") {
    return <AccountTypeText label={t("accountType.pending")} title={t("accountType.pendingDescription")} variant="muted" />;
  }

  const isFree = quota.type === "free";
  const label = isFree ? t("accountType.free") : t("accountType.paid");
  return <AccountTypeText label={label} variant={isFree ? "free" : "default"} />;
}

function AccountTypeText({ label, title, variant }: { label: string; title?: string; variant: "default" | "free" | "muted" }) {
  if (variant === "muted") {
    return <span title={title ?? label} className="text-xs text-muted-foreground">{label}</span>;
  }
  return <span title={title ?? label} className={cn("max-w-32 truncate text-xs font-medium", variant === "free" ? "text-emerald-700 dark:text-emerald-300" : "text-primary")}>{label}</span>;
}

function AccountStatus({ account }: { account: AccountDTO }) {
  const { t } = useTranslation();
  if (!account.enabled) {
    return <Badge variant="outline" className="text-muted-foreground">{t("accounts.statusDisabled")}</Badge>;
  }
  if (account.authStatus === "reauthRequired") {
    return <Badge variant="destructive">{t("accounts.statusReauthRequired")}</Badge>;
  }
  if (account.provider === "grok_console" && account.quotaWindows?.some((window) => window.mode === "console" && window.remaining <= 0)) {
    return <Badge variant="secondary" className="bg-amber-500/10 text-amber-700 dark:text-amber-300">{t("accounts.waitingReset")}</Badge>;
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
