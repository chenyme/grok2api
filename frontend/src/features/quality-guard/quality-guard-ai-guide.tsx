/* Hallmark · component: dialog · genre: modern-minimal · theme: existing system
 * states: default · hover · focus · active · disabled · loading · error · success
 * contrast: pass (40-41) · pre-emit critique: P5 H5 E5 S5 R5 V4
 */
import { Bot, Check, Copy, ShieldCheck } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { copyToClipboard } from "@/shared/clipboard";

export function QualityGuardAIGuide() {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const [copied, setCopied] = useState(false);
  const resetTimerRef = useRef<number | null>(null);
  const prompt = t("qualityGuardGuide.prompt");

  useEffect(() => () => {
    if (resetTimerRef.current !== null) window.clearTimeout(resetTimerRef.current);
  }, []);

  async function copyPrompt() {
    const ok = await copyToClipboard(prompt);
    if (!ok) {
      toast.error(t("common.copyFailed"));
      return;
    }
    if (resetTimerRef.current !== null) window.clearTimeout(resetTimerRef.current);
    setCopied(true);
    resetTimerRef.current = window.setTimeout(() => {
      setCopied(false);
      resetTimerRef.current = null;
    }, 1500);
  }

  return <>
    <Button type="button" variant="secondary" size="sm" onClick={() => setOpen(true)}>
      <Bot />
      {t("qualityGuardGuide.action")}
    </Button>
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogContent className="flex max-h-[calc(100svh-2rem)] min-h-0 flex-col gap-0 overflow-hidden p-0 sm:max-w-3xl">
        <DialogHeader className="shrink-0 border-b px-5 py-4 pr-12 text-left">
          <DialogTitle className="flex items-center gap-2 leading-5">
            <ShieldCheck className="size-4 text-emerald-600 dark:text-emerald-400" />
            {t("qualityGuardGuide.title")}
          </DialogTitle>
          <DialogDescription>{t("qualityGuardGuide.description")}</DialogDescription>
        </DialogHeader>

        <div className="min-h-0 overflow-y-auto px-5 py-4">
          <div className="mb-3 rounded-md border bg-muted/35 px-3 py-2.5 text-xs leading-5 text-muted-foreground">
            <p className="font-medium text-foreground">{t("qualityGuardGuide.beforeCopy")}</p>
            <p>{t("qualityGuardGuide.safety")}</p>
          </div>
          <pre className="whitespace-pre-wrap break-words rounded-md border bg-muted/45 p-3 font-mono text-[11px] leading-5 text-foreground sm:text-xs">{prompt}</pre>
        </div>

        <DialogFooter className="shrink-0 gap-2 border-t bg-muted/15 px-5 py-3.5 sm:justify-between sm:space-x-0">
          <span className="hidden text-xs text-muted-foreground sm:block">{t("qualityGuardGuide.footer")}</span>
          <Button type="button" size="sm" onClick={() => void copyPrompt()} aria-live="polite">
            {copied ? <Check /> : <Copy />}
            {t(copied ? "common.copied" : "qualityGuardGuide.copy")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  </>;
}
