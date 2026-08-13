import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { AlertTriangle, Gauge, Power, PowerOff, RefreshCw, Search, Users } from "lucide-react";
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { updateAccountsEnabled } from "@/features/accounts/accounts-api";
import { getDegradeAccounts, type DegradeAccountDTO, type DegradeClass, type DegradeSummaryDTO, type DegradeWindow } from "@/features/quality-guard/quality-guard-api";
import { EmptyState, ErrorState } from "@/shared/components/data-state";
import { cn } from "@/shared/lib/cn";
import { formatCompactDateTime } from "@/shared/lib/format";

const MUTE_TOAST_ID = "quality-guard-degrade-mute";

export function DegradeAccountsPanel({ softTPS, hardTPS }: { softTPS?: number; hardTPS?: number }) {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const [period, setPeriod] = useState<DegradeWindow>("24h");
  const [search, setSearch] = useState("");
  const [status, setStatus] = useState<"all" | "on" | "off">("all");
  const [cls, setCls] = useState<"all" | DegradeClass>("all");
  const [hitsMin, setHitsMin] = useState(1);
  const [selected, setSelected] = useState<Set<string>>(() => new Set());

  const query = useQuery({
    queryKey: ["quality-guard-degrade-accounts", period, softTPS, hardTPS],
    queryFn: () => getDegradeAccounts({ window: period, softTPS, hardTPS }),
    refetchInterval: 15_000,
  });

  const data = query.data;
  const rows = useMemo(() => filterAccounts(data, { search, status, cls, hitsMin }), [data, search, status, cls, hitsMin]);
  const selectable = rows;
  const selectedRows = selectable.filter((account) => selected.has(account.id));
  const selectedEnabled = selectedRows.filter((account) => account.enabled);
  const selectedMuted = selectedRows.filter((account) => !account.enabled);
  const allSelected = selectable.length > 0 && selectedRows.length === selectable.length;

  const muteMutation = useMutation({
    mutationFn: (ids: string[]) => updateAccountsEnabled(ids, false, "grok_build"),
    onMutate: () => toast.loading(t("qualityGuard.degrade.muting"), { id: MUTE_TOAST_ID }),
    onSuccess: (_, ids) => {
      setSelected(new Set());
      void queryClient.invalidateQueries({ queryKey: ["quality-guard-degrade-accounts"] });
      void queryClient.invalidateQueries({ queryKey: ["accounts"] });
      toast.success(t("qualityGuard.degrade.muted", { count: ids.length }), { id: MUTE_TOAST_ID });
    },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("qualityGuard.degrade.muteFailed"), { id: MUTE_TOAST_ID }),
  });
  const unmuteMutation = useMutation({
    mutationFn: (ids: string[]) => updateAccountsEnabled(ids, true, "grok_build"),
    onMutate: () => toast.loading(t("qualityGuard.degrade.unmuting"), { id: MUTE_TOAST_ID }),
    onSuccess: (_, ids) => {
      setSelected(new Set());
      void queryClient.invalidateQueries({ queryKey: ["quality-guard-degrade-accounts"] });
      void queryClient.invalidateQueries({ queryKey: ["accounts"] });
      toast.success(t("qualityGuard.degrade.unmuted", { count: ids.length }), { id: MUTE_TOAST_ID });
    },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("qualityGuard.degrade.unmuteFailed"), { id: MUTE_TOAST_ID }),
  });
  const busy = muteMutation.isPending || unmuteMutation.isPending;

  const toggleAll = (checked: boolean) => {
    setSelected((current) => {
      const next = new Set(current);
      selectable.forEach((account) => {
        if (checked) next.add(account.id);
        else next.delete(account.id);
      });
      return next;
    });
  };

  if (query.isError && !data) return <ErrorState message={query.error.message} onRetry={() => void query.refetch()} />;
  if (!data) return null;

  return (
    <div className="space-y-4">
      <section className="grid overflow-hidden rounded-lg bg-card sm:grid-cols-2 xl:grid-cols-4" aria-label={t("qualityGuard.degrade.overview")}>
        <Metric icon={AlertTriangle} label={t("qualityGuard.degrade.hits")} value={String(data.totals.hits)} detail={t("qualityGuard.degrade.hitsHelp", { burst: data.totals.burst, soft: data.totals.soft, hard: data.totals.hard })} tone={data.totals.hits ? "bad" : "good"} />
        <Metric icon={Users} label={t("qualityGuard.degrade.accounts")} value={String(data.totals.accounts)} detail={t("qualityGuard.degrade.accountsHelp")} />
        <Metric icon={PowerOff} label={t("qualityGuard.degrade.stillEnabled")} value={String(data.totals.stillEnabled)} detail={t("qualityGuard.degrade.stillEnabledHelp")} tone={data.totals.stillEnabled ? "bad" : "good"} />
        <Metric icon={Gauge} label={t("qualityGuard.degrade.maxTPS")} value={Math.round(data.totals.maxTPS).toString()} detail={t("qualityGuard.degrade.maxTPSHelp")} tone={data.totals.maxTPS >= data.thresholds.hardTPS ? "bad" : undefined} />
      </section>

      <div className="grid items-stretch gap-3 xl:grid-cols-[minmax(0,1.4fr)_minmax(260px,0.8fr)]">
        <SeriesChart series={data.series} empty={t("qualityGuard.degrade.noHits")} title={t("qualityGuard.degrade.series")} />
        <NodeList nodes={data.nodes} empty={t("qualityGuard.degrade.noNodes")} title={t("qualityGuard.degrade.nodes")} />
      </div>

      <section className="overflow-hidden rounded-lg bg-card">
        <div className="flex flex-col gap-3 border-b px-4 py-4 sm:flex-row sm:items-start sm:justify-between sm:px-5">
          <div>
            <h2 className="text-sm font-medium">{t("qualityGuard.degrade.accountsTitle")}</h2>
            <p className="mt-1 text-xs text-muted-foreground">
              {hitsMin > 1
                ? t("qualityGuard.degrade.accountsHintFiltered", { shown: rows.length, total: data.accounts.length, min: hitsMin })
                : t("qualityGuard.degrade.accountsHint", { shown: rows.length, total: data.accounts.length })}
            </p>
          </div>
          <div className="flex flex-wrap items-center gap-1.5">
            <div className="relative">
              <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
              <Input className="h-8 w-44 pl-8" value={search} onChange={(event) => setSearch(event.target.value)} placeholder={t("qualityGuard.degrade.search")} />
            </div>
            <FilterSelect value={period} onChange={(value) => { setSelected(new Set()); setPeriod(value as DegradeWindow); }} items={[["1h", t("qualityGuard.degrade.windows.1h")], ["6h", t("qualityGuard.degrade.windows.6h")], ["24h", t("qualityGuard.degrade.windows.24h")], ["7d", t("qualityGuard.degrade.windows.7d")]]} />
            <FilterSelect value={status} onChange={(value) => setStatus(value as "all" | "on" | "off")} items={[["all", t("qualityGuard.degrade.statusAll")], ["on", t("qualityGuard.degrade.statusOn")], ["off", t("qualityGuard.degrade.statusOff")]]} />
            <FilterSelect value={cls} onChange={(value) => setCls(value as "all" | DegradeClass)} items={[["all", t("qualityGuard.degrade.classAll")], ["buffered_burst", "burst"], ["soft_tps", "soft"], ["hard_tps", "hard"]]} />
            <FilterSelect value={String(hitsMin)} onChange={(value) => setHitsMin(Number(value))} items={[["1", t("qualityGuard.degrade.hitsAll")], ["2", t("qualityGuard.degrade.hitsMin", { count: 2 })], ["3", t("qualityGuard.degrade.hitsMin", { count: 3 })], ["5", t("qualityGuard.degrade.hitsMin", { count: 5 })], ["10", t("qualityGuard.degrade.hitsMin", { count: 10 })]]} />
            <Button type="button" variant="secondary" size="sm" className="bg-destructive/10 text-destructive hover:bg-destructive/15 hover:text-destructive" disabled={selectedEnabled.length === 0 || busy} onClick={() => {
              if (!window.confirm(t("qualityGuard.degrade.muteConfirm", { count: selectedEnabled.length }))) return;
              muteMutation.mutate(selectedEnabled.map((account) => account.id));
            }}>
              <PowerOff />{selectedEnabled.length ? t("qualityGuard.degrade.muteSelectedCount", { count: selectedEnabled.length }) : t("qualityGuard.degrade.muteSelected")}
            </Button>
            <Button type="button" variant="secondary" size="sm" disabled={selectedMuted.length === 0 || busy} onClick={() => {
              if (!window.confirm(t("qualityGuard.degrade.unmuteConfirm", { count: selectedMuted.length }))) return;
              unmuteMutation.mutate(selectedMuted.map((account) => account.id));
            }}>
              <Power />{selectedMuted.length ? t("qualityGuard.degrade.unmuteSelectedCount", { count: selectedMuted.length }) : t("qualityGuard.degrade.unmuteSelected")}
            </Button>
            <Button type="button" variant="ghost" size="icon" className="size-8" onClick={() => void query.refetch()} disabled={query.isFetching} aria-label={t("common.refresh")}>
              <RefreshCw className={cn("size-4", query.isFetching && "animate-spin")} />
            </Button>
          </div>
        </div>
        <div className="overflow-x-auto">
          <Table className="min-w-[920px]">
            <TableHeader>
              <TableRow>
                <TableHead className="w-10 px-3"><Checkbox checked={allSelected ? true : selectedRows.length > 0 ? "indeterminate" : false} disabled={selectable.length === 0} onCheckedChange={(checked) => toggleAll(checked === true)} aria-label={t("common.selectPage")} /></TableHead>
                <TableHead>{t("qualityGuard.degrade.account")}</TableHead>
                <TableHead>{t("qualityGuard.degrade.status")}</TableHead>
                <TableHead className="text-right">{t("qualityGuard.degrade.hitCount")}</TableHead>
                <TableHead className="text-right">{t("qualityGuard.outputTPS")}</TableHead>
                <TableHead>{t("qualityGuard.degrade.class")}</TableHead>
                <TableHead>{t("qualityGuard.node")}</TableHead>
                <TableHead>{t("qualityGuard.lastObserved")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.length === 0 ? (
                <TableRow><TableCell colSpan={8}><EmptyState message={t("qualityGuard.degrade.noAccounts")} /></TableCell></TableRow>
              ) : rows.map((account) => (
                <TableRow key={account.id} className="cursor-pointer" onClick={() => {
                  setSelected((current) => {
                    const next = new Set(current);
                    if (next.has(account.id)) next.delete(account.id);
                    else next.add(account.id);
                    return next;
                  });
                }}>
                  <TableCell className="px-3" onClick={(event) => event.stopPropagation()}>
                    <label className="flex size-8 cursor-pointer items-center justify-center">
                      <Checkbox checked={selected.has(account.id)} onCheckedChange={(checked) => setSelected((current) => {
                        const next = new Set(current);
                        if (checked === true) next.add(account.id);
                        else next.delete(account.id);
                        return next;
                      })} />
                    </label>
                  </TableCell>
                  <TableCell>
                    <div className="font-mono text-xs text-muted-foreground">#{account.id}</div>
                    <div className="text-sm">{account.email || account.name || "-"}</div>
                  </TableCell>
                  <TableCell className="whitespace-nowrap">
                    {account.enabled ? <Badge variant="outline" className="whitespace-nowrap text-destructive">{t("qualityGuard.degrade.scheduling")}</Badge> : <Badge variant="outline" className="whitespace-nowrap text-emerald-600 dark:text-emerald-400">{t("qualityGuard.degrade.mutedStatus")}</Badge>}
                    {account.bfs ? <Badge variant="outline" className="ml-1 whitespace-nowrap text-muted-foreground">bfs {account.bfs}</Badge> : null}
                  </TableCell>
                  <TableCell className="text-right font-mono tabular-nums">{account.hits}</TableCell>
                  <TableCell className={cn("text-right font-mono tabular-nums", account.maxTPS >= data.thresholds.hardTPS ? "text-destructive" : account.maxTPS >= data.thresholds.softTPS ? "text-amber-600 dark:text-amber-400" : "")}>{account.maxTPS}</TableCell>
                  <TableCell className="text-xs text-muted-foreground">{classSummary(account.classes)}</TableCell>
                  <TableCell className="text-xs">{account.nodes.join(" · ") || "-"}</TableCell>
                  <TableCell className="font-mono text-xs">{account.last ? formatCompactDateTime(account.last, i18n.language) : "-"}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      </section>

      <section className="overflow-hidden rounded-lg bg-card">
        <div className="border-b px-4 py-4 sm:px-5">
          <h2 className="text-sm font-medium">{t("qualityGuard.degrade.events")}</h2>
          <p className="mt-1 text-xs text-muted-foreground">{t("qualityGuard.degrade.eventsHelp")}</p>
        </div>
        <div className="max-h-72 overflow-auto">
          {data.events.length === 0 ? <EmptyState message={t("qualityGuard.degrade.noEvents")} /> : data.events.map((event) => (
            <div key={event.id} className="grid grid-cols-[7.5rem_minmax(0,1fr)_auto] gap-3 border-b px-4 py-2 last:border-b-0 sm:px-5">
              <div className="font-mono text-[11px] text-muted-foreground">{formatCompactDateTime(event.createdAt, i18n.language)}</div>
              <div className="truncate text-xs">#{event.accountId ?? "-"} {event.accountName} · {event.nodeName} · out {event.outputTokens} · {event.requestId}</div>
              <div className="flex items-center gap-2">{classBadge(event.class)}<span className="font-mono text-xs tabular-nums">{event.tps}</span></div>
            </div>
          ))}
        </div>
      </section>
    </div>
  );
}

function filterAccounts(data: DegradeSummaryDTO | undefined, filters: { search: string; status: "all" | "on" | "off"; cls: "all" | DegradeClass; hitsMin: number }) {
  if (!data) return [];
  const search = filters.search.trim().toLowerCase();
  return data.accounts.filter((account) => {
    if (account.hits < filters.hitsMin) return false;
    if (filters.status === "on" && !account.enabled) return false;
    if (filters.status === "off" && account.enabled) return false;
    if (filters.cls !== "all" && (account.classes[filters.cls] ?? 0) <= 0) return false;
    if (search && !`${account.id} ${account.email} ${account.name}`.toLowerCase().includes(search)) return false;
    return true;
  });
}

function classSummary(classes: DegradeAccountDTO["classes"]) {
  return [
    classes.buffered_burst ? `burst ${classes.buffered_burst}` : "",
    classes.soft_tps ? `soft ${classes.soft_tps}` : "",
    classes.hard_tps ? `hard ${classes.hard_tps}` : "",
  ].filter(Boolean).join(" · ") || "-";
}

function classBadge(cls: DegradeClass) {
  if (cls === "hard_tps") return <Badge variant="outline" className="text-destructive">hard</Badge>;
  if (cls === "buffered_burst") return <Badge variant="outline" className="text-amber-600 dark:text-amber-400">burst</Badge>;
  return <Badge variant="outline" className="text-muted-foreground">soft</Badge>;
}

function FilterSelect({ value, onChange, items }: { value: string; onChange: (value: string) => void; items: string[][] }) {
  return (
    <Select value={value} onValueChange={onChange}>
      <SelectTrigger className="h-8 w-auto min-w-28"><SelectValue /></SelectTrigger>
      <SelectContent>{items.map(([itemValue, label]) => <SelectItem key={itemValue} value={itemValue}>{label}</SelectItem>)}</SelectContent>
    </Select>
  );
}

function Metric({ icon: Icon, label, value, detail, tone }: { icon: typeof AlertTriangle; label: string; value: string; detail: string; tone?: "good" | "bad" }) {
  return (
    <div className="flex min-h-24 items-center gap-3 border-b p-4 last:border-b-0 sm:[&:nth-child(odd)]:border-r xl:border-b-0 xl:border-r xl:last:border-r-0">
      <span className={cn("flex size-9 shrink-0 items-center justify-center rounded-md bg-secondary text-muted-foreground", tone === "good" && "text-emerald-600 dark:text-emerald-400", tone === "bad" && "text-destructive")}><Icon className="size-4" /></span>
      <div className="min-w-0">
        <p className="text-xs text-muted-foreground">{label}</p>
        <p className="mt-1 truncate text-lg font-medium tabular-nums">{value}</p>
        <p className="mt-1 truncate text-[11px] text-muted-foreground">{detail}</p>
      </div>
    </div>
  );
}

function SeriesChart({ series, empty, title }: { series: DegradeSummaryDTO["series"]; empty: string; title: string }) {
  const max = Math.max(1, ...series.map((item) => item.count));
  const labelStep = series.length > 12 ? Math.ceil(series.length / 8) : 1;
  return (
    <section className="flex h-full min-h-64 flex-col overflow-hidden rounded-lg bg-card">
      <div className="shrink-0 border-b px-4 py-4 sm:px-5"><h2 className="text-sm font-medium">{title}</h2></div>
      <div className="flex min-h-0 flex-1 items-stretch gap-1 px-4 pb-3 pt-2 sm:px-5">
        {series.length === 0 ? <p className="self-center text-xs text-muted-foreground">{empty}</p> : series.map((item, index) => {
          const height = item.count <= 0 ? 0 : Math.max(6, Math.round((item.count / max) * 100));
          const showLabel = index % labelStep === 0 || index === series.length - 1;
          return (
            <div key={`${item.label}-${index}`} className="flex min-w-0 flex-1 flex-col" title={`${item.label}: ${item.count}`}>
              <div className="relative min-h-0 flex-1">
                <div
                  className={cn("absolute inset-x-0 bottom-0 rounded-t-sm", item.severe > 0 && item.severe >= item.count * 0.5 ? "bg-destructive" : "bg-amber-500")}
                  style={{ height: `${height}%` }}
                />
              </div>
              <div className="mt-1 h-4 text-center text-[10px] tabular-nums text-muted-foreground">
                {showLabel ? shortSeriesLabel(item.label) : ""}
              </div>
            </div>
          );
        })}
      </div>
    </section>
  );
}

function shortSeriesLabel(label: string) {
  const hour = label.match(/(\d{1,2})(?::\d{2})?$/);
  return hour ? hour[1].padStart(2, "0") : label;
}

function NodeList({ nodes, empty, title }: { nodes: DegradeSummaryDTO["nodes"]; empty: string; title: string }) {
  const max = Math.max(1, ...nodes.map((node) => node.hits));
  return (
    <section className="flex h-full min-h-64 flex-col overflow-hidden rounded-lg bg-card">
      <div className="shrink-0 border-b px-4 py-4 sm:px-5"><h2 className="text-sm font-medium">{title}</h2></div>
      <div className="min-h-0 flex-1 space-y-2 overflow-auto p-4 sm:p-5">
        {nodes.length === 0 ? <p className="text-xs text-muted-foreground">{empty}</p> : nodes.map((node) => (
          <div key={node.name} className="grid grid-cols-[minmax(0,1fr)_auto_auto] items-center gap-3 text-xs">
            <div>
              <div>{node.name}</div>
              <div className="mt-1 h-1.5 overflow-hidden rounded-full bg-muted"><i className="block h-full bg-amber-500" style={{ width: `${Math.round(node.hits / max * 100)}%` }} /></div>
            </div>
            <span className="font-mono">{node.hits} / {node.accounts}</span>
            <span className="font-mono">{node.maxTPS}</span>
          </div>
        ))}
      </div>
    </section>
  );
}
