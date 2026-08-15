import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Pencil, Plus, Star, Trash2 } from "lucide-react";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { createProbeProfile, deleteProbeProfile, listProbeProfiles, updateProbeProfile, type ProbeProfile } from "@/features/quality-guard/quality-guard-api";

type Draft = {
  name: string;
  prompt: string;
  expectedText: string;
  matchMode: string;
  requireThinking: boolean;
};

const emptyDraft = (): Draft => ({ name: "", prompt: "", expectedText: "", matchMode: "last_line", requireThinking: false });

export function ProbeProfilesPanel() {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [editing, setEditing] = useState<ProbeProfile | null | undefined>(undefined);
  const [draft, setDraft] = useState<Draft>(emptyDraft());
  const [deleting, setDeleting] = useState<ProbeProfile | null>(null);
  const query = useQuery({ queryKey: ["quality-guard-profiles"], queryFn: listProbeProfiles });
  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: ["quality-guard-profiles"] });
    void queryClient.invalidateQueries({ queryKey: ["quality-guard"] });
  };
  const saveMutation = useMutation({
    mutationFn: async () => {
      const payload = {
        name: draft.name.trim(),
        prompt: draft.prompt.trim(),
        expectedText: draft.expectedText.trim(),
        matchMode: draft.matchMode,
        requireThinking: draft.requireThinking,
      };
      if (editing && !editing.built_in) {
        return updateProbeProfile(editing.id, payload);
      }
      return createProbeProfile(payload);
    },
    onSuccess: () => {
      toast.success(t("qualityGuard.profileSaved"));
      setEditing(undefined);
      invalidate();
    },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("errors.generic")),
  });
  const activateMutation = useMutation({
    mutationFn: (profile: ProbeProfile) => updateProbeProfile(profile.id, {
      name: profile.name,
      prompt: profile.prompt,
      expectedText: profile.expected_text ?? "",
      matchMode: profile.match_mode,
      requireThinking: profile.require_thinking,
      active: true,
    }),
    onSuccess: () => {
      toast.success(t("qualityGuard.profileSaved"));
      invalidate();
    },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("errors.generic")),
  });
  const deleteMutation = useMutation({
    mutationFn: (profile: ProbeProfile) => deleteProbeProfile(profile.id),
    onSuccess: () => {
      toast.success(t("qualityGuard.profileDeleted"));
      setDeleting(null);
      invalidate();
    },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("errors.generic")),
  });

  const items = query.data?.items ?? [];
  const activeId = query.data?.activeProfileId;
  const readonly = Boolean(editing?.built_in);
  const matchLabel = (mode: string) => {
    if (mode === "last_line") return t("qualityGuard.profileMatchLastLine");
    if (mode === "regex") return t("qualityGuard.profileMatchRegex");
    return t("qualityGuard.profileMatchContains");
  };

  return (
    <section className="overflow-hidden rounded-lg bg-card">
      <div className="flex flex-col gap-2 border-b px-4 py-4 sm:flex-row sm:items-center sm:justify-between sm:px-5">
        <div>
          <h2 className="text-sm font-medium">{t("qualityGuard.profilesTab")}</h2>
          <p className="mt-1 text-xs text-muted-foreground">{t("qualityGuard.profilesHelp")}</p>
        </div>
        <Button type="button" size="sm" onClick={() => { setEditing(null); setDraft(emptyDraft()); }}>
          <Plus />{t("qualityGuard.profileCreate")}
        </Button>
      </div>
      <div className="divide-y">
        {items.map((profile) => {
          const active = profile.id === activeId;
          const marker = profile.expected_text
            ? `${t("qualityGuard.profileExpected")} ${profile.expected_text} · ${matchLabel(profile.match_mode)}`
            : t("qualityGuard.profileNoExpected");
          return (
            <div key={profile.id} className="flex flex-col gap-3 px-4 py-3 sm:flex-row sm:items-center sm:justify-between sm:px-5">
              <div className="min-w-0">
                <div className="flex flex-wrap items-center gap-2">
                  <strong className="text-sm">{profile.name}</strong>
                  <Badge variant={profile.built_in ? "secondary" : "outline"}>{t(profile.built_in ? "qualityGuard.profileBuiltin" : "qualityGuard.profileCustom")}</Badge>
                  {active ? <Badge>{t("qualityGuard.profileActive")}</Badge> : null}
                </div>
                <p className="mt-1 truncate text-xs text-muted-foreground">{marker}</p>
              </div>
              <div className="flex flex-wrap gap-1.5">
                {!active ? <Button type="button" variant="secondary" size="sm" disabled={activateMutation.isPending} onClick={() => activateMutation.mutate(profile)}><Star />{t("qualityGuard.profileActivate")}</Button> : null}
                <Button type="button" variant="ghost" size="sm" onClick={() => {
                  setEditing(profile);
                  setDraft({ name: profile.name, prompt: profile.prompt, expectedText: profile.expected_text ?? "", matchMode: profile.match_mode || "contains", requireThinking: profile.require_thinking });
                }}><Pencil />{t(profile.built_in ? "qualityGuard.profileView" : "qualityGuard.profileEdit")}</Button>
                {!profile.built_in ? <Button type="button" variant="ghost" size="sm" className="text-destructive hover:text-destructive" onClick={() => setDeleting(profile)}><Trash2 />{t("common.delete")}</Button> : null}
              </div>
            </div>
          );
        })}
      </div>

      <Dialog open={editing !== undefined} onOpenChange={(open) => { if (!open && !saveMutation.isPending) setEditing(undefined); }}>
        <DialogContent className="max-h-[calc(100svh-2rem)] overflow-y-auto sm:max-w-[560px]">
          <DialogHeader>
            <DialogTitle>{editing ? t(readonly ? "qualityGuard.profileView" : "qualityGuard.profileEdit") : t("qualityGuard.profileCreate")}</DialogTitle>
            <DialogDescription>{t("qualityGuard.profilesHelp")}</DialogDescription>
          </DialogHeader>
          <form className="space-y-3.5" onSubmit={(event) => { event.preventDefault(); if (!readonly) saveMutation.mutate(); }}>
            <div className="space-y-2">
              <Label htmlFor="probe-profile-name">{t("qualityGuard.profileName")}</Label>
              <Input id="probe-profile-name" value={draft.name} disabled={readonly} onChange={(event) => setDraft({ ...draft, name: event.target.value })} />
            </div>
            <div className="space-y-2">
              <Label>{t("qualityGuard.profileMatch")}</Label>
              <Select value={draft.matchMode} disabled={readonly} onValueChange={(matchMode) => setDraft({ ...draft, matchMode })}>
                <SelectTrigger><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="last_line">{t("qualityGuard.profileMatchLastLine")}</SelectItem>
                  <SelectItem value="contains">{t("qualityGuard.profileMatchContains")}</SelectItem>
                  <SelectItem value="regex">{t("qualityGuard.profileMatchRegex")}</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-2">
              <Label htmlFor="probe-profile-expected">{t("qualityGuard.profileExpected")}</Label>
              <Input id="probe-profile-expected" value={draft.expectedText} disabled={readonly} onChange={(event) => setDraft({ ...draft, expectedText: event.target.value })} />
              <p className="text-xs text-muted-foreground">{t("qualityGuard.profileExpectedHelp")}</p>
            </div>
            <div className="space-y-2">
              <Label htmlFor="probe-profile-prompt">{t("qualityGuard.profilePrompt")}</Label>
              <Textarea id="probe-profile-prompt" value={draft.prompt} disabled={readonly} onChange={(event) => setDraft({ ...draft, prompt: event.target.value })} />
            </div>
            <div className="flex items-start justify-between gap-4 rounded-md border p-3">
              <div className="space-y-1">
                <Label htmlFor="probe-profile-thinking">{t("qualityGuard.profileRequireThinking")}</Label>
                <p className="text-xs text-muted-foreground">{t("qualityGuard.profileRequireThinkingHelp")}</p>
              </div>
              <Switch id="probe-profile-thinking" checked={draft.requireThinking} disabled={readonly} onCheckedChange={(requireThinking) => setDraft({ ...draft, requireThinking })} />
            </div>
            <DialogFooter>
              <Button type="button" variant="secondary" size="sm" onClick={() => setEditing(undefined)}>{t("common.cancel")}</Button>
              {readonly ? null : <Button type="submit" size="sm" disabled={!draft.name.trim() || !draft.prompt.trim() || saveMutation.isPending}>{saveMutation.isPending ? <Spinner /> : null}{t("common.save")}</Button>}
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      <AlertDialog open={deleting !== null} onOpenChange={(open) => { if (!open && !deleteMutation.isPending) setDeleting(null); }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("common.delete")}</AlertDialogTitle>
            <AlertDialogDescription>{t("qualityGuard.profileDeleteConfirm", { name: deleting?.name ?? "" })}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={deleteMutation.isPending}>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction className="bg-destructive text-white hover:bg-destructive/90" disabled={deleteMutation.isPending || !deleting} onClick={(event) => { event.preventDefault(); if (deleting) deleteMutation.mutate(deleting); }}>
              {deleteMutation.isPending ? <Spinner /> : null}{t("common.delete")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </section>
  );
}
