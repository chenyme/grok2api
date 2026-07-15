import { useMutation, useQuery } from "@tanstack/react-query";
import { Bot, ExternalLink, ImageIcon, KeyRound, Loader2, MessageSquareText, RefreshCw, Send, Trash2, UserRound, Video } from "lucide-react";
import { useEffect, useMemo, useRef, useState, type FormEvent, type KeyboardEvent, type ReactNode } from "react";
import { useTranslation } from "react-i18next";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { listModels } from "@/entities/model/model-api";
import type { ModelRouteDTO } from "@/entities/model/types";
import {
  createChatCompletion,
  createVideo,
  generateImage,
  getVideo,
  type ChatMessage,
  type ImageResult,
  type VideoStatus,
} from "@/features/creative-console/creative-console-api";
import { getClientKeySecret, listClientKeys, type ClientKeyDTO } from "@/features/client-keys/client-keys-api";
import { getSystemInfo } from "@/entities/system/system-api";
import { PageHeader } from "@/shared/components/page-header";
import { runtimeConfig } from "@/shared/config/runtime-config";
import { cn } from "@/shared/lib/cn";

import { listAllPaginatedItems } from "./creative-console-pagination";

type CreativeMode = "chat" | "image" | "video";
type ConversationMessage = ChatMessage & { id: string };

type SecretState = {
  keyId: string;
  secret: string;
};

type ChatRequest = {
  requestMessages: ChatMessage[];
  apiKey: string;
  model: string;
  publicApiBaseURL: string;
};

const imageAspectRatios = ["1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3"] as const;
const videoAspectRatios = ["1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3"] as const;
const imageResolutions = ["1k", "2k"] as const;
const videoResolutions = ["480p", "720p", "1080p"] as const;

export function CreativeConsolePage() {
  const { t } = useTranslation();
  const [mode, setMode] = useState<CreativeMode>("chat");
  const [selectedKeyId, setSelectedKeyId] = useState("");
  const [secretState, setSecretState] = useState<SecretState | null>(null);
  const [keyError, setKeyError] = useState("");
  const [selectedModels, setSelectedModels] = useState<Record<CreativeMode, string>>({ chat: "", image: "", video: "" });

  const keysQuery = useQuery({
    queryKey: ["creative-console", "client-keys"],
    queryFn: () => listAllPaginatedItems((page, pageSize) => listClientKeys({ page, pageSize, status: "active" })),
    staleTime: 30_000,
  });
  const modelsQuery = useQuery({
    queryKey: ["creative-console", "models"],
    queryFn: () => listAllPaginatedItems((page, pageSize) => listModels({ page, pageSize, status: "enabled" })),
    staleTime: 30_000,
  });
  const systemQuery = useQuery({
    queryKey: ["system-info"],
    queryFn: getSystemInfo,
    staleTime: Number.POSITIVE_INFINITY,
    retry: 1,
  });

  const activeKeys = useMemo(() => (keysQuery.data ?? []).filter(isUsableKey), [keysQuery.data]);
  const effectiveKeyId = activeKeys.some((key) => key.id === selectedKeyId) ? selectedKeyId : activeKeys[0]?.id ?? "";
  const selectedKey = activeKeys.find((key) => key.id === effectiveKeyId);
  const availableModels = useMemo(() => (modelsQuery.data ?? []).filter((model) => model.enabled && model.available), [modelsQuery.data]);
  const permittedModels = useMemo(() => {
    if (!selectedKey || selectedKey.allowedModelIds.length === 0) return availableModels;
    const allowedModelIds = new Set(selectedKey.allowedModelIds);
    return availableModels.filter((model) => allowedModelIds.has(model.id));
  }, [availableModels, selectedKey]);
  const modelGroups = useMemo(() => ({
    chat: permittedModels.filter((model) => model.capability === "chat" || model.capability === "responses"),
    image: permittedModels.filter((model) => model.capability === "image"),
    video: permittedModels.filter((model) => model.capability === "video"),
  }), [permittedModels]);
  const effectiveModels = useMemo<Record<CreativeMode, string>>(() => ({
    chat: modelGroups.chat.some((model) => model.publicId === selectedModels.chat) ? selectedModels.chat : modelGroups.chat[0]?.publicId ?? "",
    image: modelGroups.image.some((model) => model.publicId === selectedModels.image) ? selectedModels.image : modelGroups.image[0]?.publicId ?? "",
    video: modelGroups.video.some((model) => model.publicId === selectedModels.video) ? selectedModels.video : modelGroups.video[0]?.publicId ?? "",
  }), [modelGroups, selectedModels]);

  const secretMutation = useMutation({
    mutationFn: (id: string) => getClientKeySecret(id),
    onSuccess: ({ secret }, id) => {
      setSecretState({ keyId: id, secret });
      setKeyError("");
    },
    onError: (error) => setKeyError(error instanceof Error ? error.message : t("creativeConsole.errors.keyUnavailable")),
  });

  const publicApiBaseURL = systemQuery.data?.publicApiBaseURL || runtimeConfig.publicApiBaseUrl;
  const apiKey = secretState?.keyId === effectiveKeyId ? secretState.secret : "";

  const sharedProps: CreativePanelProps = {
    publicApiBaseURL,
    apiKey,
    model: effectiveModels[mode],
    modelOptions: modelGroups[mode],
    onModelChange: (model) => setSelectedModels((current) => ({ ...current, [mode]: model })),
  };

  function changeKey(id: string): void {
    setSelectedKeyId(id);
    setSecretState(null);
    setKeyError("");
  }

  function unlockKey(): void {
    if (!effectiveKeyId || secretMutation.isPending) return;
    secretMutation.mutate(effectiveKeyId);
  }

  return (
    <div className="space-y-8">
      <PageHeader title={t("creativeConsole.title")} description={t("creativeConsole.description")} />

      <section className="space-y-4 rounded-lg bg-card p-4 sm:p-5">
        <div className="flex flex-col gap-4 lg:flex-row lg:items-end">
          <Field className="min-w-0 flex-1" label={t("creativeConsole.clientKey")} htmlFor="creative-key">
            <Select value={effectiveKeyId} onValueChange={changeKey} disabled={keysQuery.isPending || activeKeys.length === 0}>
              <SelectTrigger id="creative-key" aria-label={t("creativeConsole.clientKey")}>
                <SelectValue placeholder={keysQuery.isPending ? t("common.loading") : t("creativeConsole.selectKey")} />
              </SelectTrigger>
              <SelectContent>
                {activeKeys.map((key) => <SelectItem key={key.id} value={key.id}>{key.name} · {key.prefix}</SelectItem>)}
              </SelectContent>
            </Select>
          </Field>
          <div className="flex flex-wrap items-center gap-2">
            <Badge variant={apiKey ? "default" : "secondary"}>
              <KeyRound />
              {apiKey ? t("creativeConsole.keyReady") : t("creativeConsole.keyLocked")}
            </Badge>
            <Button type="button" variant="secondary" size="sm" onClick={unlockKey} disabled={!effectiveKeyId || Boolean(apiKey) || secretMutation.isPending}>
              {secretMutation.isPending ? <Spinner /> : <KeyRound />}
              {apiKey ? t("creativeConsole.keyLoaded") : t("creativeConsole.loadKey")}
            </Button>
          </div>
        </div>
        {keysQuery.isError ? <RetryableError message={keysQuery.error.message} onRetry={() => void keysQuery.refetch()} /> : null}
        {!keysQuery.isPending && !keysQuery.isError && activeKeys.length === 0 ? <InlineError message={t("creativeConsole.errors.noKeys")} /> : null}
        {keyError ? <InlineError message={keyError} /> : null}
        <p className="text-xs leading-5 text-muted-foreground">{t("creativeConsole.keyNotice")}</p>
      </section>

      <Tabs value={mode} onValueChange={(value) => setMode(value as CreativeMode)}>
        <TabsList className="h-auto w-full rounded-md bg-muted p-0.5 sm:w-fit">
          <TabsTrigger className="flex-1 rounded-sm data-[state=active]:bg-background data-[state=active]:shadow-sm sm:flex-none" value="chat"><MessageSquareText />{t("creativeConsole.modes.chat")}</TabsTrigger>
          <TabsTrigger className="flex-1 rounded-sm data-[state=active]:bg-background data-[state=active]:shadow-sm sm:flex-none" value="image"><ImageIcon />{t("creativeConsole.modes.image")}</TabsTrigger>
          <TabsTrigger className="flex-1 rounded-sm data-[state=active]:bg-background data-[state=active]:shadow-sm sm:flex-none" value="video"><Video />{t("creativeConsole.modes.video")}</TabsTrigger>
        </TabsList>
      </Tabs>

      {modelsQuery.isError ? <RetryableError message={modelsQuery.error.message} onRetry={() => void modelsQuery.refetch()} /> : null}
      {mode === "chat" ? <ChatPanel {...sharedProps} /> : null}
      {mode === "image" ? <ImagePanel {...sharedProps} /> : null}
      {mode === "video" ? <VideoPanel {...sharedProps} /> : null}
    </div>
  );
}

type CreativePanelProps = {
  publicApiBaseURL: string;
  apiKey: string;
  model: string;
  modelOptions: ModelRouteDTO[];
  onModelChange: (model: string) => void;
};

function ChatPanel({ publicApiBaseURL, apiKey, model, modelOptions, onModelChange }: CreativePanelProps) {
  const { t } = useTranslation();
  const [systemPrompt, setSystemPrompt] = useState("");
  const [prompt, setPrompt] = useState("");
  const [messages, setMessages] = useState<ConversationMessage[]>([]);
  const listEndRef = useRef<HTMLDivElement>(null);

  const mutation = useMutation({
    mutationFn: (request: ChatRequest) => createChatCompletion({
      publicApiBaseURL: request.publicApiBaseURL,
      apiKey: request.apiKey,
      model: request.model,
      messages: request.requestMessages,
    }),
    onSuccess: (content, request) => setMessages([
      ...messagesFromRequest(request.requestMessages),
      { id: crypto.randomUUID(), role: "assistant", content },
    ]),
  });

  useEffect(() => {
    listEndRef.current?.scrollIntoView({ block: "nearest" });
  }, [messages]);

  function submit(event?: FormEvent): void {
    event?.preventDefault();
    const userText = prompt.trim();
    if (!apiKey || !model || !userText || mutation.isPending) return;
    const userMessage: ConversationMessage = { id: crypto.randomUUID(), role: "user", content: userText };
    const requestMessages: ChatMessage[] = [
      ...(systemPrompt.trim() ? [{ role: "system" as const, content: systemPrompt.trim() }] : []),
      ...messages.map(({ role, content }) => ({ role, content })),
      { role: "user", content: userText },
    ];
    setMessages((current) => [...current, userMessage]);
    setPrompt("");
    mutation.reset();
    mutation.mutate({ requestMessages, apiKey, model, publicApiBaseURL });
  }

  function handlePromptKeyDown(event: KeyboardEvent<HTMLTextAreaElement>): void {
    if (event.key === "Enter" && !event.shiftKey) {
      event.preventDefault();
      submit();
    }
  }

  return (
    <div className="grid min-w-0 gap-4 lg:grid-cols-[minmax(0,0.82fr)_minmax(0,1.18fr)]">
      <Panel title={t("creativeConsole.settings")}>
        <form className="space-y-4" onSubmit={submit}>
          <ModelField value={model} models={modelOptions} onChange={onModelChange} />
          <Field label={t("creativeConsole.systemPrompt")} htmlFor="chat-system">
            <Textarea id="chat-system" value={systemPrompt} onChange={(event) => setSystemPrompt(event.target.value)} placeholder={t("creativeConsole.systemPromptPlaceholder")} className="min-h-24" />
          </Field>
          <Field label={t("creativeConsole.prompt")} htmlFor="chat-prompt">
            <Textarea id="chat-prompt" value={prompt} onChange={(event) => setPrompt(event.target.value)} onKeyDown={handlePromptKeyDown} placeholder={t("creativeConsole.chatPlaceholder")} className="min-h-36" />
          </Field>
          <div className="flex flex-col-reverse gap-2 sm:flex-row sm:items-center sm:justify-between">
            <span className="text-[11px] text-muted-foreground">{t("creativeConsole.enterHint")}</span>
            <Button type="submit" className="w-full sm:w-auto" disabled={!apiKey || !model || !prompt.trim() || mutation.isPending}>
              {mutation.isPending ? <Loader2 className="animate-spin" /> : <Send />}
              {t("creativeConsole.send")}
            </Button>
          </div>
          {mutation.isError ? <InlineError message={mutation.error.message} /> : null}
        </form>
      </Panel>

      <Panel title={t("creativeConsole.conversation")} actions={messages.length > 0 ? (
        <Button type="button" variant="ghost" size="sm" onClick={() => setMessages([])} disabled={mutation.isPending}>
          <Trash2 />{t("creativeConsole.clear")}
        </Button>
      ) : undefined}>
        <div className="max-h-[65vh] min-h-96 space-y-3 overflow-y-auto pr-1" aria-live="polite">
          {messages.length === 0 && !mutation.isPending ? <EmptyResult icon={<MessageSquareText />} text={t("creativeConsole.emptyChat")} /> : null}
          {messages.map((message) => <ChatBubble key={message.id} message={message} />)}
          {mutation.isPending ? <ChatBubble message={{ id: "pending", role: "assistant", content: t("creativeConsole.thinking") }} loading /> : null}
          <div ref={listEndRef} />
        </div>
      </Panel>
    </div>
  );
}

function ImagePanel({ publicApiBaseURL, apiKey, model, modelOptions, onModelChange }: CreativePanelProps) {
  const { t } = useTranslation();
  const [prompt, setPrompt] = useState("");
  const [count, setCount] = useState("1");
  const [aspectRatio, setAspectRatio] = useState("1:1");
  const [resolution, setResolution] = useState("1k");
  const [images, setImages] = useState<ImageResult[]>([]);

  const mutation = useMutation({
    mutationFn: () => generateImage({ publicApiBaseURL, apiKey, model, prompt: prompt.trim(), count: Number(count), aspectRatio, resolution }),
    onSuccess: setImages,
  });

  function submit(event: FormEvent): void {
    event.preventDefault();
    if (!apiKey || !model || !prompt.trim() || mutation.isPending) return;
    mutation.reset();
    mutation.mutate();
  }

  return (
    <div className="grid min-w-0 gap-4 lg:grid-cols-[minmax(0,0.82fr)_minmax(0,1.18fr)]">
      <Panel title={t("creativeConsole.settings")}>
        <form className="space-y-4" onSubmit={submit}>
          <ModelField value={model} models={modelOptions} onChange={onModelChange} />
          <Field label={t("creativeConsole.prompt")} htmlFor="image-prompt">
            <Textarea id="image-prompt" value={prompt} onChange={(event) => setPrompt(event.target.value)} placeholder={t("creativeConsole.imagePlaceholder")} className="min-h-40" />
          </Field>
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
            <SelectField id="image-count" label={t("creativeConsole.count")} value={count} options={["1", "2", "3", "4"]} onChange={setCount} />
            <SelectField id="image-aspect" label={t("creativeConsole.aspectRatio")} value={aspectRatio} options={imageAspectRatios} onChange={setAspectRatio} />
            <SelectField id="image-resolution" label={t("creativeConsole.resolution")} value={resolution} options={imageResolutions} onChange={setResolution} />
          </div>
          <Button type="submit" className="w-full sm:w-auto" disabled={!apiKey || !model || !prompt.trim() || mutation.isPending}>
            {mutation.isPending ? <Loader2 className="animate-spin" /> : <ImageIcon />}
            {t("creativeConsole.generateImage")}
          </Button>
          {mutation.isError ? <InlineError message={mutation.error.message} /> : null}
        </form>
      </Panel>

      <Panel title={t("creativeConsole.result")}>
        {images.length === 0 && !mutation.isPending ? <EmptyResult icon={<ImageIcon />} text={t("creativeConsole.emptyImage")} /> : null}
        {mutation.isPending ? <LoadingResult text={t("creativeConsole.generatingImage")} /> : null}
        {images.length > 0 ? (
          <div className="grid grid-cols-1 gap-3 md:grid-cols-2" aria-live="polite">
            {images.map((image, index) => (
              <figure key={`${image.url}-${index}`} className="min-w-0 border bg-secondary/20 p-2">
                <img src={image.url} alt={t("creativeConsole.generatedImageAlt", { index: index + 1 })} className="aspect-square w-full bg-muted object-contain" loading="lazy" />
                <figcaption className="mt-2 flex min-w-0 items-center justify-between gap-2">
                  <span className="truncate text-xs text-muted-foreground">{t("creativeConsole.imageNumber", { index: index + 1 })}</span>
                  <Button variant="ghost" size="sm" asChild>
                    <a href={image.url} target="_blank" rel="noreferrer"><ExternalLink />{t("creativeConsole.open")}</a>
                  </Button>
                </figcaption>
              </figure>
            ))}
          </div>
        ) : null}
      </Panel>
    </div>
  );
}

function VideoPanel({ publicApiBaseURL, apiKey, model, modelOptions, onModelChange }: CreativePanelProps) {
  const { t } = useTranslation();
  const [prompt, setPrompt] = useState("");
  const [imageURL, setImageURL] = useState("");
  const [duration, setDuration] = useState("8");
  const [aspectRatio, setAspectRatio] = useState("16:9");
  const [resolution, setResolution] = useState("720p");
  const [job, setJob] = useState<{ requestId: string; apiKey: string; publicApiBaseURL: string } | null>(null);

  const createMutation = useMutation({
    mutationFn: () => createVideo({
      publicApiBaseURL,
      apiKey,
      model,
      prompt: prompt.trim(),
      imageURL: imageURL.trim() || undefined,
      duration: Number(duration),
      aspectRatio,
      resolution,
    }),
    onSuccess: (requestId) => setJob({ requestId, apiKey, publicApiBaseURL }),
  });

  const statusQuery = useQuery({
    queryKey: ["creative-console", "video", job?.requestId, job?.publicApiBaseURL],
    queryFn: ({ signal }) => getVideo({ publicApiBaseURL: job!.publicApiBaseURL, apiKey: job!.apiKey, requestId: job!.requestId, signal }),
    enabled: Boolean(job),
    refetchInterval: (query) => query.state.data?.status === "pending" ? 3_000 : false,
    retry: 2,
  });

  function submit(event: FormEvent): void {
    event.preventDefault();
    if (!apiKey || !model || (!prompt.trim() && !imageURL.trim()) || createMutation.isPending) return;
    setJob(null);
    createMutation.reset();
    createMutation.mutate();
  }

  return (
    <div className="grid min-w-0 gap-4 lg:grid-cols-[minmax(0,0.82fr)_minmax(0,1.18fr)]">
      <Panel title={t("creativeConsole.settings")}>
        <form className="space-y-4" onSubmit={submit}>
          <ModelField value={model} models={modelOptions} onChange={onModelChange} />
          <Field label={t("creativeConsole.prompt")} htmlFor="video-prompt">
            <Textarea id="video-prompt" value={prompt} onChange={(event) => setPrompt(event.target.value)} placeholder={t("creativeConsole.videoPlaceholder")} className="min-h-32" />
          </Field>
          <Field label={t("creativeConsole.referenceImage")} htmlFor="video-image">
            <Input id="video-image" type="url" value={imageURL} onChange={(event) => setImageURL(event.target.value)} placeholder="https://example.com/image.png" />
          </Field>
          <p className="text-[11px] leading-5 text-muted-foreground">{t("creativeConsole.videoInputHint")}</p>
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
            <Field label={t("creativeConsole.duration")} htmlFor="video-duration">
              <Input id="video-duration" type="number" min={1} max={15} step={1} value={duration} onChange={(event) => setDuration(event.target.value)} />
            </Field>
            <SelectField id="video-aspect" label={t("creativeConsole.aspectRatio")} value={aspectRatio} options={videoAspectRatios} onChange={setAspectRatio} />
            <SelectField id="video-resolution" label={t("creativeConsole.resolution")} value={resolution} options={videoResolutions} onChange={setResolution} />
          </div>
          <Button type="submit" className="w-full sm:w-auto" disabled={!apiKey || !model || (!prompt.trim() && !imageURL.trim()) || !validDuration(duration) || createMutation.isPending}>
            {createMutation.isPending ? <Loader2 className="animate-spin" /> : <Video />}
            {t("creativeConsole.generateVideo")}
          </Button>
          {createMutation.isError ? <InlineError message={createMutation.error.message} /> : null}
        </form>
      </Panel>

      <Panel title={t("creativeConsole.result")}>
        {!job && !createMutation.isPending ? <EmptyResult icon={<Video />} text={t("creativeConsole.emptyVideo")} /> : null}
        {createMutation.isPending ? <LoadingResult text={t("creativeConsole.submittingVideo")} /> : null}
        {job ? <VideoResult requestId={job.requestId} status={statusQuery.data} loading={statusQuery.isPending || statusQuery.isFetching} error={statusQuery.isError ? statusQuery.error.message : ""} onRetry={() => void statusQuery.refetch()} /> : null}
      </Panel>
    </div>
  );
}

function VideoResult({ requestId, status, loading, error, onRetry }: { requestId: string; status?: VideoStatus; loading: boolean; error: string; onRetry: () => void }) {
  const { t } = useTranslation();
  const progress = status?.progress ?? 0;
  return (
    <div className="space-y-4" aria-live="polite">
      <div className="grid gap-3 sm:grid-cols-2">
        <MetaItem label={t("creativeConsole.requestId")} value={requestId} mono />
        <MetaItem label={t("creativeConsole.status")} value={status ? t(`creativeConsole.videoStatus.${status.status}`) : t("common.loading")} />
      </div>
      <div className="space-y-2">
        <div className="flex items-center justify-between text-xs"><span className="text-muted-foreground">{t("creativeConsole.progress")}</span><span className="tabular-nums">{progress}%</span></div>
        <div className="h-2 overflow-hidden bg-muted"><div className="h-full bg-primary transition-[width]" style={{ width: `${progress}%` }} /></div>
      </div>
      {loading && status?.status !== "done" && status?.status !== "failed" ? <div className="flex items-center gap-2 text-xs text-muted-foreground"><Spinner />{t("creativeConsole.pollingVideo")}</div> : null}
      {error ? <RetryableError message={error} onRetry={onRetry} /> : null}
      {status?.status === "failed" ? <InlineError message={status.error?.message || t("creativeConsole.errors.videoFailed")} /> : null}
      {status?.status === "done" && status.video ? (
        <div className="space-y-3">
          <video src={status.video.url} controls preload="metadata" className="max-h-[65vh] w-full bg-black" />
          <div className="flex flex-wrap items-center justify-between gap-2">
            <span className="text-xs text-muted-foreground">{status.video.duration ? t("creativeConsole.videoDuration", { count: status.video.duration }) : ""}</span>
            <Button variant="secondary" size="sm" asChild><a href={status.video.url} target="_blank" rel="noreferrer"><ExternalLink />{t("creativeConsole.openVideo")}</a></Button>
          </div>
        </div>
      ) : null}
    </div>
  );
}

function ModelField({ value, models, onChange }: { value: string; models: ModelRouteDTO[]; onChange: (model: string) => void }) {
  const { t } = useTranslation();
  return (
    <Field label={t("creativeConsole.model")} htmlFor="creative-model">
      <Select value={value} onValueChange={onChange} disabled={models.length === 0}>
        <SelectTrigger id="creative-model" aria-label={t("creativeConsole.model")}>
          <SelectValue placeholder={models.length === 0 ? t("creativeConsole.noModels") : t("creativeConsole.selectModel")} />
        </SelectTrigger>
        <SelectContent>{models.map((item) => <SelectItem key={item.id} value={item.publicId}>{item.publicId}</SelectItem>)}</SelectContent>
      </Select>
      {models.length === 0 ? <p className="text-[11px] leading-5 text-destructive">{t("creativeConsole.errors.noModels")}</p> : null}
    </Field>
  );
}

function SelectField({ id, label, value, options, onChange }: { id: string; label: string; value: string; options: readonly string[]; onChange: (value: string) => void }) {
  return (
    <Field label={label} htmlFor={id}>
      <Select value={value} onValueChange={onChange}>
        <SelectTrigger id={id} aria-label={label}><SelectValue /></SelectTrigger>
        <SelectContent>{options.map((option) => <SelectItem key={option} value={option}>{option}</SelectItem>)}</SelectContent>
      </Select>
    </Field>
  );
}

function Panel({ title, actions, children }: { title: string; actions?: ReactNode; children: ReactNode }) {
  return (
    <section className="min-w-0 rounded-lg bg-card p-4 sm:p-5">
      <div className="mb-5 flex min-h-8 items-center justify-between gap-3 border-b pb-3"><h2 className="text-sm font-medium">{title}</h2>{actions}</div>
      {children}
    </section>
  );
}

function Field({ label, htmlFor, className, children }: { label: string; htmlFor: string; className?: string; children: ReactNode }) {
  return <div className={cn("space-y-2", className)}><Label htmlFor={htmlFor}>{label}</Label>{children}</div>;
}

function messagesFromRequest(messages: ChatMessage[]): ConversationMessage[] {
  return messages
    .filter((message) => message.role !== "system")
    .map((message) => ({ ...message, id: crypto.randomUUID() }));
}

function ChatBubble({ message, loading = false }: { message: ConversationMessage; loading?: boolean }) {
  const isUser = message.role === "user";
  return (
    <div className={cn("flex gap-3 rounded-md border p-3", isUser ? "bg-secondary/25" : "bg-background")}>
      <div className="flex size-7 shrink-0 items-center justify-center rounded-sm border bg-card">{isUser ? <UserRound className="size-4" /> : <Bot className="size-4" />}</div>
      <div className="min-w-0 flex-1 whitespace-pre-wrap break-words text-sm leading-6">{loading ? <span className="flex items-center gap-2 text-muted-foreground"><Spinner />{message.content}</span> : message.content}</div>
    </div>
  );
}

function EmptyResult({ icon, text }: { icon: ReactNode; text: string }) {
  return <div className="flex min-h-80 flex-col items-center justify-center gap-3 rounded-md border border-dashed px-6 text-center text-sm text-muted-foreground [&_svg]:size-6">{icon}<span>{text}</span></div>;
}

function LoadingResult({ text }: { text: string }) {
  return <div className="flex min-h-80 items-center justify-center gap-3 rounded-md border border-dashed text-sm text-muted-foreground"><Spinner className="size-5" />{text}</div>;
}

function InlineError({ message }: { message: string }) {
  return <div role="alert" className="rounded-md border border-destructive/35 bg-destructive/5 px-3 py-2 text-xs leading-5 text-destructive">{message}</div>;
}

function RetryableError({ message, onRetry }: { message: string; onRetry: () => void }) {
  const { t } = useTranslation();
  return (
    <div role="alert" className="flex flex-col gap-2 rounded-md border border-destructive/35 bg-destructive/5 px-3 py-2 text-xs leading-5 text-destructive sm:flex-row sm:items-center sm:justify-between">
      <span>{message}</span>
      <Button type="button" variant="ghost" size="sm" className="self-start text-destructive hover:text-destructive sm:self-auto" onClick={onRetry}>
        <RefreshCw />{t("common.retry")}
      </Button>
    </div>
  );
}

function MetaItem({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) {
  return <div className="min-w-0 rounded-md border bg-secondary/20 p-3"><div className="mb-1 text-[11px] text-muted-foreground">{label}</div><div className={cn("truncate text-xs", mono && "font-mono")} title={value}>{value}</div></div>;
}

function isUsableKey(key: ClientKeyDTO): boolean {
  if (!key.enabled) return false;
  return !key.expiresAt || new Date(key.expiresAt).getTime() > Date.now();
}

function validDuration(value: string): boolean {
  const duration = Number(value);
  return Number.isInteger(duration) && duration >= 1 && duration <= 15;
}
