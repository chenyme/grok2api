import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, Eye, EyeOff, MoreHorizontal, Pencil, Plus, Search, Trash2 } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Spinner } from "@/components/ui/spinner";
import { Table, TableActionCell, TableActionHead, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { createEgressProxyProfile, deleteEgressProxyProfile, getEgressProxyProfileURL, listEgressProxyProfiles, updateEgressProxyProfile, type EgressProxyProfileDTO } from "@/features/settings/settings-api";
import { ErrorState, TableLoadingRow } from "@/shared/components/data-state";
import { Pagination } from "@/shared/components/pagination";
import { useDebouncedValue } from "@/shared/hooks/use-debounced-value";
import { cn } from "@/shared/lib/cn";

type ProfileForm = { name: string; proxyURL: string };
const emptyForm: ProfileForm = { name: "", proxyURL: "" };

export function EgressProxyProfiles({ open, onOpenChange, startCreating = false, onCreated }: { open: boolean; onOpenChange: (open: boolean) => void; startCreating?: boolean; onCreated?: (profile: EgressProxyProfileDTO) => void }) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [editing, setEditing] = useState<EgressProxyProfileDTO | null | undefined>(startCreating ? null : undefined);
  const [deleting, setDeleting] = useState<EgressProxyProfileDTO | undefined>();
  const [form, setForm] = useState<ProfileForm>(emptyForm);
  const [proxyVisible, setProxyVisible] = useState(false);
  const [revealedProxyURL, setRevealedProxyURL] = useState("");
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [search, setSearch] = useState("");
  const debouncedSearch = useDebouncedValue(search);
  const query = useQuery({
    queryKey: ["egress-proxy-profiles", "page", page, pageSize, debouncedSearch],
    queryFn: () => listEgressProxyProfiles({ page, pageSize, search: debouncedSearch }),
    enabled: open && editing === undefined,
  });

  const save = useMutation({
    mutationFn: () => {
      const proxyURL = form.proxyURL.trim();
      const input = { name: form.name.trim(), proxyURL: proxyURL && proxyURL !== revealedProxyURL ? proxyURL : undefined };
      return editing ? updateEgressProxyProfile(editing.id, input) : createEgressProxyProfile({ ...input, proxyURL });
    },
    onSuccess: (profile) => {
      const created = editing === null;
      void queryClient.invalidateQueries({ queryKey: ["egress-proxy-profiles"] });
      void queryClient.invalidateQueries({ queryKey: ["egress-nodes"] });
      setEditing(undefined);
      toast.success(t("egressProxyProfiles.saved"));
      if (created) onCreated?.(profile);
    },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("settings.egress.operationFailed")),
  });
  const reveal = useMutation({
    mutationFn: () => {
      if (!editing) throw new Error(t("egressProxyProfiles.revealUnavailable"));
      return getEgressProxyProfileURL(editing.id);
    },
    onSuccess: ({ proxyURL }) => {
      setRevealedProxyURL(proxyURL);
      setProxyVisible(true);
      setForm((current) => ({ ...current, proxyURL }));
    },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("settings.egress.operationFailed")),
  });
  const remove = useMutation({
    mutationFn: (id: string) => deleteEgressProxyProfile(id),
    onSuccess: () => {
      if (page > 1 && query.data?.items.length === 1) setPage(page - 1);
      void queryClient.invalidateQueries({ queryKey: ["egress-proxy-profiles"] });
      setDeleting(undefined);
      toast.success(t("egressProxyProfiles.deleted"));
    },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("settings.egress.operationFailed")),
  });

  function openCreate() {
    setForm(emptyForm);
    setProxyVisible(false);
    setRevealedProxyURL("");
    setEditing(null);
  }

  function openEdit(value: EgressProxyProfileDTO) {
    setForm({ name: value.name, proxyURL: "" });
    setProxyVisible(false);
    setRevealedProxyURL("");
    setEditing(value);
  }

  const title = editing === undefined ? t("egressProxyProfiles.libraryTitle") : editing ? t("egressProxyProfiles.editTitle") : t("egressProxyProfiles.addTitle");
  const description = editing === undefined ? t("egressProxyProfiles.libraryDescription") : t("egressProxyProfiles.dialogDescription");

  return (
    <>
      <Dialog open={open} onOpenChange={onOpenChange}>
        <DialogContent className={cn(
          "max-h-[calc(100svh-2rem)]",
          editing === undefined
            ? "flex min-h-0 flex-col overflow-hidden sm:max-w-[720px]"
            : "overflow-y-auto sm:max-w-[520px]",
        )}>
          <DialogHeader className="shrink-0 pr-8">
            <DialogTitle>{title}</DialogTitle>
            <DialogDescription>{description}</DialogDescription>
          </DialogHeader>

          {editing === undefined ? (
            <div className="flex min-h-0 flex-col gap-3">
              <div className="flex shrink-0 flex-col gap-2 sm:flex-row sm:items-center">
                <div className="relative min-w-0 flex-1 sm:max-w-72">
                  <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
                  <Input type="search" autoComplete="off" data-1p-ignore data-lpignore="true" data-form-type="other" className="h-8 pl-8 text-xs" value={search} onChange={(event) => { setSearch(event.target.value); setPage(1); }} placeholder={t("egressProxyProfiles.search")} aria-label={t("egressProxyProfiles.search")} />
                </div>
                <Button type="button" size="sm" variant="secondary" className="sm:ml-auto" onClick={openCreate}><Plus />{t("egressProxyProfiles.add")}</Button>
              </div>
              <div className="max-h-[360px] min-h-0 overflow-auto rounded-md border">
                {query.isError ? <ErrorState message={query.error.message} onRetry={() => void query.refetch()} /> : null}
                {!query.isError ? <Table className="table-fixed">
                  <TableHeader><TableRow className="hover:bg-transparent"><TableHead className="w-[32%]">{t("egressProxyProfiles.name")}</TableHead><TableHead>{t("egressProxyProfiles.endpoint")}</TableHead><TableHead className="w-20 text-center">{t("egressProxyProfiles.nodes")}</TableHead><TableActionHead /></TableRow></TableHeader>
                  {query.isPending ? <TableBody><TableLoadingRow colSpan={4} /></TableBody> : null}
                  {!query.isPending && query.data.items.length === 0 ? <TableBody><TableRow><TableCell colSpan={4} className="h-24 text-center text-xs text-muted-foreground">{t(search ? "egressProxyProfiles.noMatches" : "egressProxyProfiles.emptyLibrary")}</TableCell></TableRow></TableBody> : null}
                  {!query.isPending ? <TableBody>{query.data.items.map((profile) => <TableRow key={profile.id}>
                    <TableCell><span className="block truncate text-xs font-medium" title={profile.name}>{profile.name}</span></TableCell>
                    <TableCell><div className="min-w-0" title={`${profile.proxyDisplay || ""} · ${profile.proxyFingerprint || ""}`}><p className="truncate text-xs font-medium">{profile.proxyDisplay}</p>{profile.proxyFingerprint ? <p className="font-mono text-[10px] text-muted-foreground">#{profile.proxyFingerprint}</p> : null}</div></TableCell>
                    <TableCell className="text-center text-xs tabular-nums">{profile.boundNodeCount}</TableCell>
                    <TableActionCell><DropdownMenu><DropdownMenuTrigger asChild><Button type="button" variant="ghost" size="icon" className="size-8" aria-label={t("common.actions")}><MoreHorizontal /></Button></DropdownMenuTrigger><DropdownMenuContent align="end"><DropdownMenuItem onClick={() => openEdit(profile)}><Pencil />{t("common.edit")}</DropdownMenuItem><DropdownMenuSeparator /><DropdownMenuItem className="text-destructive focus:text-destructive" disabled={profile.boundNodeCount > 0} onClick={() => setDeleting(profile)}><Trash2 />{profile.boundNodeCount > 0 ? t("egressProxyProfiles.deleteBlocked", { count: profile.boundNodeCount }) : t("common.delete")}</DropdownMenuItem></DropdownMenuContent></DropdownMenu></TableActionCell>
                  </TableRow>)}</TableBody> : null}
                </Table> : null}
              </div>
              {query.data && query.data.total > 0 ? <div className="shrink-0"><Pagination page={query.data.page} pageSize={query.data.pageSize} total={query.data.total} onPageChange={setPage} onPageSizeChange={(value) => { setPageSize(value); setPage(1); }} /></div> : null}
            </div>
          ) : (
            <div>
              <Button type="button" variant="ghost" size="sm" className="mb-3 -ml-2 text-muted-foreground" onClick={() => setEditing(undefined)}><ArrowLeft />{t("egressProxyProfiles.backToLibrary")}</Button>
              <form className="space-y-3.5" onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); save.mutate(); }}>
                <div className="space-y-2"><Label htmlFor="proxy-profile-name">{t("egressProxyProfiles.name")}</Label><Input id="proxy-profile-name" autoComplete="off" data-1p-ignore data-lpignore="true" data-form-type="other" value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} /></div>
                <div className="space-y-2"><Label htmlFor="proxy-profile-url">{t("settings.egress.proxyURL")}</Label><div className="flex gap-2"><Input id="proxy-profile-url" type={proxyVisible ? "text" : "password"} autoComplete="off" data-1p-ignore data-lpignore="true" data-form-type="other" placeholder={editing ? t("settings.egress.keepConfigured") : "socks5h://user:pass@host:port"} value={form.proxyURL} onChange={(event) => setForm({ ...form, proxyURL: event.target.value })} />{editing ? <Button type="button" variant="outline" size="icon" className="shrink-0" disabled={reveal.isPending} aria-label={t(proxyVisible ? "egressProxyProfiles.hide" : "egressProxyProfiles.reveal")} onClick={() => { if (revealedProxyURL) setProxyVisible((visible) => !visible); else reveal.mutate(); }}>{reveal.isPending ? <Spinner /> : proxyVisible ? <EyeOff /> : <Eye />}</Button> : null}</div><p className="whitespace-pre-line text-xs leading-5 text-muted-foreground">{t("settings.egress.proxyProtocols")}</p></div>
                <DialogFooter><Button type="button" variant="secondary" size="sm" onClick={() => setEditing(undefined)}>{t("common.cancel")}</Button><Button type="submit" size="sm" disabled={save.isPending || !form.name.trim() || (!editing && !form.proxyURL.trim())}>{save.isPending ? <Spinner /> : null}{t("common.save")}</Button></DialogFooter>
              </form>
            </div>
          )}
        </DialogContent>
      </Dialog>

      <AlertDialog open={Boolean(deleting)} onOpenChange={(next) => { if (!next) setDeleting(undefined); }}><AlertDialogContent><AlertDialogHeader><AlertDialogTitle>{t("egressProxyProfiles.deleteTitle")}</AlertDialogTitle><AlertDialogDescription>{t("egressProxyProfiles.deleteDescription", { name: deleting?.name })}</AlertDialogDescription></AlertDialogHeader><AlertDialogFooter><AlertDialogCancel disabled={remove.isPending}>{t("common.cancel")}</AlertDialogCancel><AlertDialogAction className="bg-destructive text-white hover:bg-destructive/90" disabled={remove.isPending} onClick={(event) => { event.preventDefault(); if (deleting) remove.mutate(deleting.id); }}>{remove.isPending ? <Spinner /> : null}{t("common.delete")}</AlertDialogAction></AlertDialogFooter></AlertDialogContent></AlertDialog>
    </>
  );
}
