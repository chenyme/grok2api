import { RotateCcw } from "lucide-react";
import { type ReactNode } from "react";
import { Controller } from "react-hook-form";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { EgressNodes } from "@/features/settings/egress-nodes";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { isByteSizeUnit, isDurationUnit, type ByteSizeValue, type DurationValue } from "@/features/settings/settings-model";
import { useSettings } from "@/features/settings/use-settings";
import { ErrorState } from "@/shared/components/data-state";

export function SettingsPage() {
  const { t } = useTranslation();
  const { form, settingsQuery, updateMutation, reset } = useSettings();

  if (settingsQuery.isError) {
    return <ErrorState message={settingsQuery.error.message} onRetry={() => void settingsQuery.refetch()} />;
  }

  const snapshot = settingsQuery.data;
  const loading = settingsQuery.isPending;
  const statsigMode = form.watch("providerWeb.statsigMode");
  const statsigManualConfigured = form.watch("providerWeb.statsigManualConfigured");

  return (
    <form className="w-full space-y-8 [&_input]:border-transparent" onSubmit={form.handleSubmit((values) => updateMutation.mutate(values))}>
      <header className="flex flex-col gap-5 sm:flex-row sm:items-center sm:justify-between">
        <div className="min-w-0">
          <h1 className="text-xl font-medium">{t("settings.title")}</h1>
          <p className="sr-only">{t("settings.description")}</p>
        </div>
        <div className="flex shrink-0 flex-wrap items-center gap-2">
          <Tooltip>
            <TooltipTrigger asChild>
              <Button type="button" variant="ghost" size="icon" className="size-8" aria-label={t("common.reset")} disabled={loading || updateMutation.isPending || !form.formState.isDirty} onClick={reset}>
                <RotateCcw />
              </Button>
            </TooltipTrigger>
            <TooltipContent>{t("common.reset")}</TooltipContent>
          </Tooltip>
          <Button type="submit" size="sm" disabled={loading || updateMutation.isPending || !form.formState.isDirty}>
            {updateMutation.isPending ? <Spinner /> : null}{t("common.save")}
          </Button>
        </div>
      </header>

      {loading ? <div className="flex min-h-64 items-center justify-center"><Spinner /></div> : null}
      {snapshot ? (
        <Tabs defaultValue="providers" className="space-y-6">
          <TabsList>
            <TabsTrigger value="providers">{t("settings.groups.providers")}</TabsTrigger>
            <TabsTrigger value="delivery">{t("settings.groups.delivery")}</TabsTrigger>
            <TabsTrigger value="policies">{t("settings.groups.policies")}</TabsTrigger>
          </TabsList>

          <SettingsPane value="providers">
          <SettingsSection title={t("settings.provider.title")}>
            <div className="grid gap-x-4 gap-y-5 sm:grid-cols-2">
              <SettingsField controlId="provider-base-url" className="sm:col-span-2" label={t("settings.provider.baseURL")} error={form.formState.errors.providerBuild?.baseURL?.message}><Input id="provider-base-url" {...form.register("providerBuild.baseURL")} /></SettingsField>
              <SettingsField controlId="provider-client-version" label={t("settings.provider.clientVersion")} error={form.formState.errors.providerBuild?.clientVersion?.message}><Input id="provider-client-version" {...form.register("providerBuild.clientVersion")} /></SettingsField>
              <SettingsField controlId="provider-client-identifier" label={t("settings.provider.clientIdentifier")} error={form.formState.errors.providerBuild?.clientIdentifier?.message}><Input id="provider-client-identifier" {...form.register("providerBuild.clientIdentifier")} /></SettingsField>
              <SettingsField controlId="provider-token-auth" label={t("settings.provider.tokenAuth")} error={form.formState.errors.providerBuild?.tokenAuth?.message}><Input id="provider-token-auth" {...form.register("providerBuild.tokenAuth")} /></SettingsField>
              <SettingsField controlId="provider-user-agent" label={t("settings.provider.userAgent")} error={form.formState.errors.providerBuild?.userAgent?.message}><Input id="provider-user-agent" {...form.register("providerBuild.userAgent")} /></SettingsField>
            </div>
          </SettingsSection>

          <SettingsSection title={t("settings.web.title")}>
            <div className="grid gap-x-4 gap-y-5 sm:grid-cols-2">
              <SettingsField controlId="web-base-url" className="sm:col-span-2" label={t("settings.web.baseURL")} error={form.formState.errors.providerWeb?.baseURL?.message}><Input id="web-base-url" {...form.register("providerWeb.baseURL")} /></SettingsField>
              <SettingsField controlId="web-statsig-mode" className="sm:col-span-2" label={t("settings.web.statsigMode")} error={form.formState.errors.providerWeb?.statsigMode?.message}>
                <Controller control={form.control} name="providerWeb.statsigMode" render={({ field }) => (
                  <div id="web-statsig-mode" role="radiogroup" className="grid h-8 grid-cols-2 rounded-md bg-muted/55 p-0.5">
                    <Button type="button" role="radio" size="sm" variant={field.value === "manual" ? "secondary" : "ghost"} className="h-7 text-xs shadow-none" aria-checked={field.value === "manual"} onClick={() => field.onChange("manual")}>{t("settings.web.statsigManual")}</Button>
                    <Button type="button" role="radio" size="sm" variant={field.value === "url" ? "secondary" : "ghost"} className="h-7 text-xs shadow-none" aria-checked={field.value === "url"} onClick={() => field.onChange("url")}>{t("settings.web.statsigURL")}</Button>
                  </div>
                )} />
              </SettingsField>
              {statsigMode === "manual" ? (
                <SettingsField controlId="web-statsig-manual" className="sm:col-span-2" label={t("settings.web.statsigValue")} badge={statsigManualConfigured ? t("settings.web.statsigConfigured") : undefined} error={form.formState.errors.providerWeb?.statsigManualValue?.message}>
                  <Input id="web-statsig-manual" type="password" autoComplete="off" placeholder={statsigManualConfigured ? t("settings.web.statsigKeepConfigured") : t("settings.web.statsigValuePlaceholder")} {...form.register("providerWeb.statsigManualValue")} />
                </SettingsField>
              ) : (
                <SettingsField controlId="web-statsig-url" className="sm:col-span-2" label={t("settings.web.statsigSignerURL")} error={form.formState.errors.providerWeb?.statsigSignerURL?.message}>
                  <Input id="web-statsig-url" type="url" {...form.register("providerWeb.statsigSignerURL")} />
                </SettingsField>
              )}
              <SettingsField controlId="web-quota-timeout" label={t("settings.web.quotaTimeout")} error={form.formState.errors.providerWeb?.quotaTimeout?.message}><Controller control={form.control} name="providerWeb.quotaTimeout" render={({ field }) => <DurationInput id="web-quota-timeout" value={field.value} onChange={field.onChange} />} /></SettingsField>
              <SettingsField controlId="web-chat-timeout" label={t("settings.web.chatTimeout")} error={form.formState.errors.providerWeb?.chatTimeout?.message}><Controller control={form.control} name="providerWeb.chatTimeout" render={({ field }) => <DurationInput id="web-chat-timeout" value={field.value} onChange={field.onChange} />} /></SettingsField>
              <SettingsField controlId="web-image-timeout" label={t("settings.web.imageTimeout")} error={form.formState.errors.providerWeb?.imageTimeout?.message}><Controller control={form.control} name="providerWeb.imageTimeout" render={({ field }) => <DurationInput id="web-image-timeout" value={field.value} onChange={field.onChange} />} /></SettingsField>
              <SettingsField controlId="web-video-timeout" label={t("settings.web.videoTimeout")} error={form.formState.errors.providerWeb?.videoTimeout?.message}><Controller control={form.control} name="providerWeb.videoTimeout" render={({ field }) => <DurationInput id="web-video-timeout" value={field.value} onChange={field.onChange} />} /></SettingsField>
              <SettingsField controlId="web-media-concurrency" label={t("settings.web.mediaConcurrency")} badge={t("settings.restartRequired")} error={form.formState.errors.providerWeb?.mediaConcurrency?.message}><Input id="web-media-concurrency" type="number" min={1} max={64} {...form.register("providerWeb.mediaConcurrency", { valueAsNumber: true })} /></SettingsField>
              <SettingsField controlId="web-recovery-base" label={t("settings.web.recoveryBackoffBase")} error={form.formState.errors.providerWeb?.recoveryBackoffBase?.message}><Controller control={form.control} name="providerWeb.recoveryBackoffBase" render={({ field }) => <DurationInput id="web-recovery-base" value={field.value} onChange={field.onChange} />} /></SettingsField>
              <SettingsField controlId="web-recovery-max" label={t("settings.web.recoveryBackoffMax")} error={form.formState.errors.providerWeb?.recoveryBackoffMax?.message}><Controller control={form.control} name="providerWeb.recoveryBackoffMax" render={({ field }) => <DurationInput id="web-recovery-max" value={field.value} onChange={field.onChange} />} /></SettingsField>
              <SettingsField controlId="web-nsfw" label={t("settings.web.allowNSFW")}><Controller control={form.control} name="providerWeb.allowNSFW" render={({ field }) => <div className="flex h-8 items-center"><Switch id="web-nsfw" checked={field.value} onCheckedChange={field.onChange} /></div>} /></SettingsField>
            </div>
          </SettingsSection>
          </SettingsPane>

          <SettingsPane value="delivery">
          <SettingsSection title={t("settings.media.title")}>
            <div className="grid gap-x-4 gap-y-5 sm:grid-cols-2">
              <SettingsField controlId="media-max-image-size" label={t("settings.media.maxImageSize")} error={form.formState.errors.media?.maxImageSize?.message}>
                <Controller control={form.control} name="media.maxImageSize" render={({ field }) => <ByteSizeInput id="media-max-image-size" value={field.value} onChange={field.onChange} />} />
              </SettingsField>
              <SettingsField controlId="media-max-total-size" label={t("settings.media.maxTotalSize")} error={form.formState.errors.media?.maxTotalSize?.message}>
                <Controller control={form.control} name="media.maxTotalSize" render={({ field }) => <ByteSizeInput id="media-max-total-size" value={field.value} onChange={field.onChange} />} />
              </SettingsField>
              <SettingsField controlId="media-cleanup-threshold" label={t("settings.media.cleanupThresholdPercent")} error={form.formState.errors.media?.cleanupThresholdPercent?.message}>
                <div className="flex min-w-0">
                  <Input id="media-cleanup-threshold" type="number" min={50} max={95} className="min-w-0 rounded-r-none" {...form.register("media.cleanupThresholdPercent", { valueAsNumber: true })} />
                  <div className="-ml-px flex h-8 w-24 shrink-0 items-center justify-center rounded-r-md bg-secondary/55 text-xs text-muted-foreground">%</div>
                </div>
              </SettingsField>
              <SettingsField controlId="media-cleanup-interval" label={t("settings.media.cleanupInterval")} error={form.formState.errors.media?.cleanupInterval?.message}>
                <Controller control={form.control} name="media.cleanupInterval" render={({ field }) => <DurationInput id="media-cleanup-interval" value={field.value} onChange={field.onChange} />} />
              </SettingsField>
            </div>
          </SettingsSection>

          <SettingsSection title={t("settings.egress.title")} wide>
            <EgressNodes />
          </SettingsSection>
          </SettingsPane>

          <SettingsPane value="policies">
          <SettingsSection title={t("settings.batch.title")}>
            <div className="grid gap-x-4 gap-y-5 sm:grid-cols-2">
              <SettingsField controlId="batch-import-concurrency" label={t("settings.batch.importConcurrency")} error={form.formState.errors.batch?.importConcurrency?.message}><Input id="batch-import-concurrency" type="number" min={1} max={50} {...form.register("batch.importConcurrency", { valueAsNumber: true })} /></SettingsField>
              <SettingsField controlId="batch-conversion-concurrency" label={t("settings.batch.conversionConcurrency")} error={form.formState.errors.batch?.conversionConcurrency?.message}><Input id="batch-conversion-concurrency" type="number" min={1} max={50} {...form.register("batch.conversionConcurrency", { valueAsNumber: true })} /></SettingsField>
              <SettingsField controlId="batch-sync-concurrency" label={t("settings.batch.syncConcurrency")} error={form.formState.errors.batch?.syncConcurrency?.message}><Input id="batch-sync-concurrency" type="number" min={1} max={50} {...form.register("batch.syncConcurrency", { valueAsNumber: true })} /></SettingsField>
              <SettingsField controlId="batch-refresh-concurrency" label={t("settings.batch.refreshConcurrency")} error={form.formState.errors.batch?.refreshConcurrency?.message}><Input id="batch-refresh-concurrency" type="number" min={1} max={50} {...form.register("batch.refreshConcurrency", { valueAsNumber: true })} /></SettingsField>
              <SettingsField controlId="batch-random-delay" label={t("settings.batch.randomDelay")} error={form.formState.errors.batch?.randomDelay?.message}><Input id="batch-random-delay" type="number" min={0} max={5_000} step={10} {...form.register("batch.randomDelay", { valueAsNumber: true })} /></SettingsField>
            </div>
          </SettingsSection>

          <SettingsSection title={t("settings.routing.title")}>
            <div className="grid gap-x-4 gap-y-5 sm:grid-cols-2">
              <SettingsField controlId="routing-sticky-ttl" label={t("settings.routing.stickyTTL")} error={form.formState.errors.routing?.stickyTTL?.message}><Controller control={form.control} name="routing.stickyTTL" render={({ field }) => <DurationInput id="routing-sticky-ttl" value={field.value} onChange={field.onChange} />} /></SettingsField>
              <SettingsField controlId="routing-cooldown-base" label={t("settings.routing.cooldownBase")} error={form.formState.errors.routing?.cooldownBase?.message}><Controller control={form.control} name="routing.cooldownBase" render={({ field }) => <DurationInput id="routing-cooldown-base" value={field.value} onChange={field.onChange} />} /></SettingsField>
              <SettingsField controlId="routing-cooldown-max" label={t("settings.routing.cooldownMax")} error={form.formState.errors.routing?.cooldownMax?.message}><Controller control={form.control} name="routing.cooldownMax" render={({ field }) => <DurationInput id="routing-cooldown-max" value={field.value} onChange={field.onChange} />} /></SettingsField>
              <SettingsField controlId="routing-max-attempts" label={t("settings.routing.maxAttempts")} error={form.formState.errors.routing?.maxAttempts?.message}><Input id="routing-max-attempts" type="number" min={1} max={10} {...form.register("routing.maxAttempts", { valueAsNumber: true })} /></SettingsField>
            </div>
          </SettingsSection>

          <SettingsSection title={t("settings.audit.title")}>
            <div className="grid gap-x-4 gap-y-5 sm:grid-cols-2">
              <SettingsField controlId="audit-buffer-size" label={t("settings.audit.bufferSize")} badge={t("settings.restartRequired")} error={form.formState.errors.audit?.bufferSize?.message}><Input id="audit-buffer-size" type="number" min={1} max={262_144} {...form.register("audit.bufferSize", { valueAsNumber: true })} /></SettingsField>
              <SettingsField controlId="audit-batch-size" label={t("settings.audit.batchSize")} error={form.formState.errors.audit?.batchSize?.message}><Input id="audit-batch-size" type="number" min={1} max={4_096} {...form.register("audit.batchSize", { valueAsNumber: true })} /></SettingsField>
              <SettingsField controlId="audit-flush-interval" label={t("settings.audit.flushInterval")} error={form.formState.errors.audit?.flushInterval?.message}><Controller control={form.control} name="audit.flushInterval" render={({ field }) => <DurationInput id="audit-flush-interval" value={field.value} onChange={field.onChange} />} /></SettingsField>
            </div>
          </SettingsSection>

          <SettingsSection title={t("settings.clientKeys.title")}>
            <div className="grid gap-x-4 gap-y-5 sm:grid-cols-2">
              <SettingsField controlId="client-key-default-rpm" label={t("settings.clientKeys.rpmLimit")} error={form.formState.errors.clientKeyDefaults?.rpmLimit?.message}><Input id="client-key-default-rpm" type="number" min={1} max={100_000} {...form.register("clientKeyDefaults.rpmLimit", { valueAsNumber: true })} /></SettingsField>
              <SettingsField controlId="client-key-default-concurrency" label={t("settings.clientKeys.maxConcurrent")} error={form.formState.errors.clientKeyDefaults?.maxConcurrent?.message}><Input id="client-key-default-concurrency" type="number" min={1} max={1_024} {...form.register("clientKeyDefaults.maxConcurrent", { valueAsNumber: true })} /></SettingsField>
            </div>
          </SettingsSection>
          </SettingsPane>
        </Tabs>
      ) : null}
    </form>
  );
}

function ByteSizeInput({ id, value, onChange }: { id: string; value?: ByteSizeValue; onChange: (value: ByteSizeValue) => void }) {
  const { t } = useTranslation();
  const unit = value?.unit ?? "MiB";
  return (
    <div className="flex min-w-0">
      <Input
        id={id}
        type="number"
        min="0.001"
        step="any"
        className="min-w-0 rounded-r-none"
        value={Number.isFinite(value?.value) ? value?.value : ""}
        onChange={(event) => onChange({ value: event.target.value === "" ? Number.NaN : Number(event.target.value), unit })}
      />
      <Select value={unit} onValueChange={(nextUnit) => { if (isByteSizeUnit(nextUnit)) onChange({ value: value?.value ?? 1, unit: nextUnit }); }}>
        <SelectTrigger className="-ml-px w-24 shrink-0 rounded-l-none border-transparent bg-secondary/55" aria-label={t("settings.media.sizeUnit")}>
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="MiB">MiB</SelectItem>
          <SelectItem value="GiB">GiB</SelectItem>
        </SelectContent>
      </Select>
    </div>
  );
}

function DurationInput({ id, value, onChange }: { id: string; value?: DurationValue; onChange: (value: DurationValue) => void }) {
  const { t } = useTranslation();
  const unit = value?.unit ?? "s";
  return (
    <div className="flex min-w-0">
      <Input
        id={id}
        type="number"
        min="0.001"
        step="any"
        className="min-w-0 rounded-r-none"
        value={Number.isFinite(value?.value) ? value?.value : ""}
        onChange={(event) => onChange({ value: event.target.value === "" ? Number.NaN : Number(event.target.value), unit })}
      />
      <Select value={unit} onValueChange={(nextUnit) => { if (isDurationUnit(nextUnit)) onChange({ value: value?.value ?? 1, unit: nextUnit }); }}>
        <SelectTrigger className="-ml-px w-24 shrink-0 rounded-l-none border-transparent bg-secondary/55" aria-label={t("settings.durationUnit")}>
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="s">{t("settings.units.seconds")}</SelectItem>
          <SelectItem value="m">{t("settings.units.minutes")}</SelectItem>
          <SelectItem value="h">{t("settings.units.hours")}</SelectItem>
          <SelectItem value="d">{t("settings.units.days")}</SelectItem>
        </SelectContent>
      </Select>
    </div>
  );
}

function SettingsPane({ value, children }: { value: string; children: ReactNode }) {
  return (
    <TabsContent value={value} forceMount className="m-0 space-y-8 data-[state=inactive]:hidden">
      {children}
    </TabsContent>
  );
}

function SettingsSection({ title, wide = false, children }: { title: string; wide?: boolean; children: ReactNode }) {
  return (
    <section className="space-y-4">
      <h2 className="text-sm font-medium">{title}</h2>
      <div className={wide ? "min-w-0" : "min-w-0 max-w-[860px]"}>{children}</div>
    </section>
  );
}

function SettingsField({ controlId, label, badge, error, className, children }: { controlId: string; label: string; badge?: string; error?: string; className?: string; children: ReactNode }) {
  const { t } = useTranslation();
  return (
    <div className={className}>
      <div className="mb-1.5 flex min-h-5 items-center gap-2">
        <Label htmlFor={controlId} className="text-xs font-medium">{label}</Label>
        {badge ? <span className="text-[11px] font-normal text-muted-foreground">{badge}</span> : null}
      </div>
      {children}
      {error ? <p className="mt-1 text-xs text-destructive">{t("settings.invalidValue")}</p> : null}
    </div>
  );
}
