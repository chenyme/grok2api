import { useMutation, useQuery } from "@tanstack/react-query";
import { ArrowUp, Bot, CheckCircle2, ExternalLink, ImageIcon, KeyRound, Loader2, LockKeyhole, MessageSquareText, RefreshCw, SlidersHorizontal, Trash2, Video } from "lucide-react";
import { useMemo, useState, type FormEvent, type KeyboardEvent } from "react";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Message, MessageAvatar, MessageContent } from "@/components/ui/message";
import { MessageScroller, MessageScrollerButton, MessageScrollerContent, MessageScrollerItem, MessageScrollerProvider, MessageScrollerViewport } from "@/components/ui/message-scroller";
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
import { PageHeader } from "@/shared/components/page-header";
import { cn } from "@/shared/lib/cn";

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
    chat: uniqueModelsByPublicID(permittedModels.filter((model) => model.capability === "chat" || model.capability === "responses")),
    image: uniqueModelsByPublicID(permittedModels.filter((model) => model.capability === "image")),
    video: uniqueModelsByPublicID(permittedModels.filter((model) => model.capability === "video")),
  }), [permittedModels]);
  const effectiveModels = useMemo<Record<CreativeMode, string>>(() => ({
    chat: modelGroups.chat.some((model) => model.publicId === selectedModels.chat) ? selectedModels.chat : modelGroups.chat[0]?.publicId ?? "",
    image: modelGroups.image.some((model) => model.publicId === selectedModels.image) ? selectedModels.image : modelGroups.image[0]?.publicId ?? "",
    video: modelGroups.video.some((model) => model.publicId === selectedModels.video) ? selectedModels.video : modelGroups.video[0]?.publicId ?? "",
  }), [modelGroups, selectedModels]);

  const secretMutation = useMutation({
    mutationFn: (id: string) => getClientKeySecret(id),
    onSuccess: ({ secret }, id) => {
      if (id !== effectiveKeyId) return;
      setSecretState({ keyId: id, secret });
      setKeyError("");
    },
    onError: (error) => setKeyError(error instanceof Error ? error.message : t("creativeConsole.errors.keyUnavailable")),
  });

  const apiKey = secretState?.keyId === effectiveKeyId ? secretState.secret : "";

  function panelProps(panelMode: CreativeMode): CreativePanelProps {
    return {
      apiKey,
      model: effectiveModels[panelMode],
      modelOptions: modelGroups[panelMode],
      onModelChange: (model) => setSelectedModels((current) => ({ ...current, [panelMode]: model })),
    };
  }

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
    <div className="flex h-[calc(100dvh-5rem)] min-h-[36rem] flex-col gap-8 overflow-hidden">
      <PageHeader title={t("creativeConsole.title")} description={t("creativeConsole.description")} />

      <section className="flex min-h-0 flex-1 flex-col overflow-hidden">
        <div className="flex min-h-12 shrink-0 flex-col gap-3 py-2 lg:flex-row lg:items-center lg:justify-between">
          <Tabs value={mode} onValueChange={(value) => setMode(value as CreativeMode)}>
            <TabsList className="h-9 w-full rounded-full bg-secondary/50 p-1 lg:w-auto">
              <TabsTrigger className="flex-1 gap-1.5 rounded-full px-3 lg:min-w-20 [&_svg]:size-3.5" value="chat"><MessageSquareText />{t("creativeConsole.modes.chat")}</TabsTrigger>
              <TabsTrigger className="flex-1 gap-1.5 rounded-full px-3 lg:min-w-20 [&_svg]:size-3.5" value="image"><ImageIcon />{t("creativeConsole.modes.image")}</TabsTrigger>
              <TabsTrigger className="flex-1 gap-1.5 rounded-full px-3 lg:min-w-20 [&_svg]:size-3.5" value="video"><Video />{t("creativeConsole.modes.video")}</TabsTrigger>
            </TabsList>
          </Tabs>

          <div className="flex min-w-0 items-center gap-2">
            <Select value={effectiveKeyId} onValueChange={changeKey} disabled={keysQuery.isPending || activeKeys.length === 0}>
              <SelectTrigger id="creative-key" className="min-w-0 flex-1 rounded-full border-transparent bg-secondary/50 lg:w-64 lg:flex-none" aria-label={t("creativeConsole.clientKey")}>
                <SelectValue placeholder={keysQuery.isPending ? t("common.loading") : t("creativeConsole.selectKey")} />
              </SelectTrigger>
              <SelectContent>
                {activeKeys.map((key) => <SelectItem key={key.id} value={key.id}>{key.name} · {key.prefix}</SelectItem>)}
              </SelectContent>
            </Select>
            <div className={cn("flex h-8 shrink-0 items-center gap-1.5 px-2 text-[11px]", apiKey ? "text-primary" : "text-muted-foreground")}>
              {apiKey ? <CheckCircle2 className="size-3.5" /> : <LockKeyhole className="size-3.5" />}
              <span className="hidden sm:inline">{apiKey ? t("creativeConsole.keyReady") : t("creativeConsole.keyLocked")}</span>
            </div>
            <Button type="button" variant="secondary" size="icon" aria-label={t("creativeConsole.loadKey")} onClick={unlockKey} disabled={!effectiveKeyId || Boolean(apiKey) || secretMutation.isPending}>
              {secretMutation.isPending ? <Spinner /> : <KeyRound />}
            </Button>
          </div>
        </div>

        <div className="shrink-0 space-y-2 px-3">
          {keysQuery.isError ? <RetryableError message={keysQuery.error.message} onRetry={() => void keysQuery.refetch()} /> : null}
          {!keysQuery.isPending && !keysQuery.isError && activeKeys.length === 0 ? <InlineError message={t("creativeConsole.errors.noKeys")} /> : null}
          {keyError ? <InlineError message={keyError} /> : null}
          {modelsQuery.isError ? <RetryableError message={modelsQuery.error.message} onRetry={() => void modelsQuery.refetch()} /> : null}
        </div>

        <div className="min-h-0 flex-1">
          <div className="h-full" hidden={mode !== "chat"}><ChatPanel {...panelProps("chat")} /></div>
          <div className="h-full" hidden={mode !== "image"}><ImagePanel {...panelProps("image")} /></div>
          <div className="h-full" hidden={mode !== "video"}><VideoPanel {...panelProps("video")} /></div>
        </div>
      </section>
    </div>
  );
}

type CreativePanelProps = {
  apiKey: string;
  model: string;
  modelOptions: ModelRouteDTO[];
  onModelChange: (model: string) => void;
};

function ChatPanel({ apiKey, model, modelOptions, onModelChange }: CreativePanelProps) {
  const { t } = useTranslation();
  const [systemPrompt, setSystemPrompt] = useState("");
  const [showSystemPrompt, setShowSystemPrompt] = useState(false);
  const [prompt, setPrompt] = useState("");
  const [messages, setMessages] = useState<ConversationMessage[]>([]);

  const mutation = useMutation({
    mutationFn: (request: ChatRequest) => createChatCompletion({
      apiKey: request.apiKey,
      model: request.model,
      messages: request.requestMessages,
    }),
    onSuccess: (content, request) => setMessages([
      ...messagesFromRequest(request.requestMessages),
      { id: createCreativeMessageId(), role: "assistant", content },
    ]),
  });

  function submit(event?: FormEvent): void {
    event?.preventDefault();
    const userText = prompt.trim();
    if (!apiKey || !model || !userText || mutation.isPending) return;
    const userMessage: ConversationMessage = { id: createCreativeMessageId(), role: "user", content: userText };
    const requestMessages: ChatMessage[] = [
      ...(systemPrompt.trim() ? [{ role: "system" as const, content: systemPrompt.trim() }] : []),
      ...messages.map(({ role, content }) => ({ role, content })),
      { role: "user", content: userText },
    ];
    setMessages((current) => [...current, userMessage]);
    setPrompt("");
    mutation.reset();
    mutation.mutate({ requestMessages, apiKey, model });
  }

  function handlePromptKeyDown(event: KeyboardEvent<HTMLTextAreaElement>): void {
    if (event.key === "Enter" && !event.shiftKey) {
      event.preventDefault();
      submit();
    }
  }

  return (
    <div className="flex h-full min-h-0 flex-col overflow-hidden">
      <MessageScrollerProvider autoScroll defaultScrollPosition="end">
        <MessageScroller className="min-h-0 flex-1">
          <MessageScrollerViewport aria-label={t("creativeConsole.messageList")}>
            <MessageScrollerContent className={cn("w-full px-3 py-6 sm:px-6", messages.length === 0 && !mutation.isPending && "justify-center")}>
              {messages.length === 0 && !mutation.isPending ? <WelcomeState title={t("creativeConsole.welcome")} /> : null}
              {messages.length > 0 || mutation.isPending ? (
                <div className="flex justify-end">
                  <Button type="button" variant="ghost" size="sm" onClick={() => setMessages([])} disabled={mutation.isPending}><Trash2 />{t("creativeConsole.clear")}</Button>
                </div>
              ) : null}
              {messages.map((message) => (
                <MessageScrollerItem key={message.id} messageId={message.id} scrollAnchor={message.role === "user"}>
                  <ChatMessageItem message={message} />
                </MessageScrollerItem>
              ))}
              {mutation.isPending ? (
                <MessageScrollerItem messageId="pending">
                  <ChatMessageItem message={{ id: "pending", role: "assistant", content: t("creativeConsole.thinking") }} loading />
                </MessageScrollerItem>
              ) : null}
            </MessageScrollerContent>
          </MessageScrollerViewport>
          <MessageScrollerButton aria-label={t("creativeConsole.scrollToLatest")} />
        </MessageScroller>
      </MessageScrollerProvider>

      <form className="w-full shrink-0 px-3 pb-2 sm:px-6 sm:pb-3" onSubmit={submit}>
        <div className="overflow-hidden rounded-2xl border border-border/70 bg-background shadow-sm transition-shadow focus-within:shadow-md">
          {showSystemPrompt ? (
            <div className="border-b border-border/50 bg-secondary/20 p-3">
              <Textarea id="chat-system" value={systemPrompt} onChange={(event) => setSystemPrompt(event.target.value)} placeholder={t("creativeConsole.systemPromptPlaceholder")} className="min-h-16 resize-none border-0 bg-transparent px-1 py-0 focus-visible:ring-0" />
            </div>
          ) : null}
          <Textarea id="chat-prompt" value={prompt} onChange={(event) => setPrompt(event.target.value)} onKeyDown={handlePromptKeyDown} placeholder={t("creativeConsole.chatPlaceholder")} className="min-h-24 resize-none border-0 bg-transparent px-4 py-3 text-sm focus-visible:ring-0" />
          <div className="flex items-center justify-between gap-3 px-3 pb-3">
            <div className="flex min-w-0 items-center gap-1">
              <Button type="button" variant="ghost" size="icon" className={cn(showSystemPrompt && "bg-secondary")} aria-label={t("creativeConsole.systemPrompt")} onClick={() => setShowSystemPrompt((value) => !value)}><SlidersHorizontal /></Button>
              <CompactModelSelect value={model} models={modelOptions} onChange={onModelChange} />
            </div>
            <Button type="submit" size="icon" aria-label={t("creativeConsole.send")} disabled={!apiKey || !model || !prompt.trim() || mutation.isPending}>
              {mutation.isPending ? <Loader2 className="animate-spin" /> : <ArrowUp />}
            </Button>
          </div>
        </div>
        {mutation.isError ? <div className="mt-1 px-2 text-[11px] text-destructive">{mutation.error.message}</div> : null}
      </form>
    </div>
  );
}

function ImagePanel({ apiKey, model, modelOptions, onModelChange }: CreativePanelProps) {
  const { t } = useTranslation();
  const [prompt, setPrompt] = useState("");
  const [count, setCount] = useState("1");
  const [aspectRatio, setAspectRatio] = useState("1:1");
  const [resolution, setResolution] = useState("1k");
  const [images, setImages] = useState<ImageResult[]>([]);

  const mutation = useMutation({
    mutationFn: (request: Parameters<typeof generateImage>[0]) => generateImage(request),
    onSuccess: setImages,
  });

  function submit(event: FormEvent): void {
    event.preventDefault();
    if (!apiKey || !model || !prompt.trim() || mutation.isPending) return;
    mutation.reset();
    mutation.mutate({ apiKey, model, prompt: prompt.trim(), count: Number(count), aspectRatio, resolution });
  }

  return (
    <div className="flex h-full min-h-0 flex-col overflow-hidden">
      <div className="min-h-0 flex-1 overflow-y-auto py-6">
        <div className="flex min-h-full w-full flex-col justify-center px-3 sm:px-6">
          {images.length === 0 && !mutation.isPending ? <WelcomeState title={t("creativeConsole.welcomeImage")} /> : null}
          {mutation.isPending ? <LoadingResult text={t("creativeConsole.generatingImage")} /> : null}
          {images.length > 0 ? (
            <div className="grid grid-cols-1 gap-4 md:grid-cols-2" aria-live="polite">
              {images.map((image, index) => (
                <figure key={`${image.url}-${index}`} className="group min-w-0 overflow-hidden">
                  <img src={image.url} alt={t("creativeConsole.generatedImageAlt", { index: index + 1 })} className="aspect-square w-full rounded-xl bg-muted object-contain" loading="lazy" />
                  <figcaption className="flex min-w-0 items-center justify-between gap-2 py-1.5">
                    <span className="truncate text-xs text-muted-foreground">{t("creativeConsole.imageNumber", { index: index + 1 })}</span>
                    <Button variant="ghost" size="icon" asChild><a href={image.url} target="_blank" rel="noreferrer" aria-label={t("creativeConsole.open")}><ExternalLink /></a></Button>
                  </figcaption>
                </figure>
              ))}
            </div>
          ) : null}
        </div>
      </div>

      <form className="w-full shrink-0 px-3 pb-2 sm:px-6 sm:pb-3" onSubmit={submit}>
        <div className="overflow-hidden rounded-2xl border border-border/70 bg-background shadow-sm transition-shadow focus-within:shadow-md">
          <Textarea id="image-prompt" value={prompt} onChange={(event) => setPrompt(event.target.value)} placeholder={t("creativeConsole.imagePlaceholder")} className="min-h-24 resize-none border-0 bg-transparent px-4 py-3 text-sm focus-visible:ring-0" />
          <div className="flex flex-wrap items-center justify-between gap-2 px-3 pb-3">
            <div className="flex min-w-0 flex-wrap items-center gap-1">
              <CompactModelSelect value={model} models={modelOptions} onChange={onModelChange} />
              <CompactSelect value={count} options={["1", "2", "3", "4"]} onChange={setCount} ariaLabel={t("creativeConsole.count")} suffix="×" />
              <CompactSelect value={aspectRatio} options={imageAspectRatios} onChange={setAspectRatio} ariaLabel={t("creativeConsole.aspectRatio")} />
              <CompactSelect value={resolution} options={imageResolutions} onChange={setResolution} ariaLabel={t("creativeConsole.resolution")} />
            </div>
            <Button type="submit" size="icon" aria-label={t("creativeConsole.generateImage")} disabled={!apiKey || !model || !prompt.trim() || mutation.isPending}>{mutation.isPending ? <Loader2 className="animate-spin" /> : <ArrowUp />}</Button>
          </div>
        </div>
        {mutation.isError ? <div className="mt-1 px-2 text-[11px] text-destructive">{mutation.error.message}</div> : null}
      </form>
    </div>
  );
}

function VideoPanel({ apiKey, model, modelOptions, onModelChange }: CreativePanelProps) {
  const { t } = useTranslation();
  const [prompt, setPrompt] = useState("");
  const [imageURL, setImageURL] = useState("");
  const [duration, setDuration] = useState("8");
  const [aspectRatio, setAspectRatio] = useState("16:9");
  const [resolution, setResolution] = useState("720p");
  const [showOptions, setShowOptions] = useState(false);
  const [job, setJob] = useState<{ requestId: string; apiKey: string } | null>(null);

  const createMutation = useMutation({
    mutationFn: (request: Parameters<typeof createVideo>[0]) => createVideo(request),
    onSuccess: (requestId, request) => setJob({ requestId, apiKey: request.apiKey }),
  });

  const statusQuery = useQuery({
    queryKey: ["creative-console", "video", job?.requestId],
    queryFn: ({ signal }) => getVideo({ apiKey: job!.apiKey, requestId: job!.requestId, signal }),
    enabled: Boolean(job),
    refetchInterval: (query) => query.state.data?.status === "pending" ? 3_000 : false,
    retry: 2,
  });

  function submit(event: FormEvent): void {
    event.preventDefault();
    if (!apiKey || !model || (!prompt.trim() && !imageURL.trim()) || !validDuration(duration) || createMutation.isPending) return;
    setJob(null);
    createMutation.reset();
    createMutation.mutate({
      apiKey,
      model,
      prompt: prompt.trim(),
      imageURL: imageURL.trim() || undefined,
      duration: Number(duration),
      aspectRatio,
      resolution,
    });
  }

  return (
    <div className="flex h-full min-h-0 flex-col overflow-hidden">
      <div className="min-h-0 flex-1 overflow-y-auto py-6">
        <div className="flex min-h-full w-full flex-col justify-center px-3 sm:px-6">
          {!job && !createMutation.isPending ? <WelcomeState title={t("creativeConsole.welcomeVideo")} /> : null}
          {createMutation.isPending ? <LoadingResult text={t("creativeConsole.submittingVideo")} /> : null}
          {job ? (
            <VideoResult
              requestId={job.requestId}
              status={statusQuery.data}
              loading={statusQuery.isPending || statusQuery.isFetching}
              error={statusQuery.isError ? statusQuery.error.message : ""}
              onRetry={() => void statusQuery.refetch()}
            />
          ) : null}
        </div>
      </div>

      <form className="w-full shrink-0 px-3 pb-2 sm:px-6 sm:pb-3" onSubmit={submit}>
        <div className="overflow-hidden rounded-2xl border border-border/70 bg-background shadow-sm transition-shadow focus-within:shadow-md">
          {showOptions ? (
            <div className="grid gap-2 border-b border-border/50 bg-secondary/20 p-3 sm:grid-cols-[minmax(0,1fr)_7rem_7rem_7rem]">
              <Input
                id="video-image"
                type="url"
                className="border-transparent bg-background/70 shadow-none"
                value={imageURL}
                onChange={(event) => setImageURL(event.target.value)}
                placeholder={t("creativeConsole.referenceImage")}
                aria-label={t("creativeConsole.referenceImage")}
              />
              <Input
                id="video-duration"
                type="number"
                className="border-transparent bg-background/70 shadow-none"
                min={1}
                max={15}
                step={1}
                value={duration}
                onChange={(event) => setDuration(event.target.value)}
                aria-label={t("creativeConsole.duration")}
              />
              <CompactSelect value={aspectRatio} options={videoAspectRatios} onChange={setAspectRatio} ariaLabel={t("creativeConsole.aspectRatio")} surfaced />
              <CompactSelect value={resolution} options={videoResolutions} onChange={setResolution} ariaLabel={t("creativeConsole.resolution")} surfaced />
            </div>
          ) : null}
          <Textarea id="video-prompt" value={prompt} onChange={(event) => setPrompt(event.target.value)} placeholder={t("creativeConsole.videoPlaceholder")} className="min-h-24 resize-none border-0 bg-transparent px-4 py-3 text-sm focus-visible:ring-0" />
          <div className="flex items-center justify-between gap-3 px-3 pb-3">
            <div className="flex min-w-0 items-center gap-1">
              <Button type="button" variant="ghost" size="icon" className={cn(showOptions && "bg-secondary")} aria-label={t("creativeConsole.videoOptions")} onClick={() => setShowOptions((value) => !value)}><SlidersHorizontal /></Button>
              <CompactModelSelect value={model} models={modelOptions} onChange={onModelChange} />
            </div>
            <Button type="submit" size="icon" aria-label={t("creativeConsole.generateVideo")} disabled={!apiKey || !model || (!prompt.trim() && !imageURL.trim()) || !validDuration(duration) || createMutation.isPending}>
              {createMutation.isPending ? <Loader2 className="animate-spin" /> : <ArrowUp />}
            </Button>
          </div>
        </div>
        {createMutation.isError ? <div className="mt-1 px-2 text-[11px] text-destructive">{createMutation.error.message}</div> : null}
      </form>
    </div>
  );
}

function VideoResult({ requestId, status, loading, error, onRetry }: { requestId: string; status?: VideoStatus; loading: boolean; error: string; onRetry: () => void }) {
  const { t } = useTranslation();
  const progress = status?.progress ?? 0;
  return (
    <div className="w-full space-y-4" aria-live="polite">
      <div className="grid gap-3 sm:grid-cols-2">
        <MetaItem label={t("creativeConsole.requestId")} value={requestId} mono />
        <MetaItem label={t("creativeConsole.status")} value={status ? t(`creativeConsole.videoStatus.${status.status}`) : t("common.loading")} />
      </div>
      <div className="space-y-2">
        <div className="flex items-center justify-between text-xs"><span className="text-muted-foreground">{t("creativeConsole.progress")}</span><span className="tabular-nums">{progress}%</span></div>
        <div className="h-1.5 overflow-hidden rounded-full bg-muted"><div className="h-full rounded-full bg-primary transition-[width]" style={{ width: `${progress}%` }} /></div>
      </div>
      {loading && status?.status !== "done" && status?.status !== "failed" ? <div className="flex items-center gap-2 text-xs text-muted-foreground"><Spinner />{t("creativeConsole.pollingVideo")}</div> : null}
      {error ? <RetryableError message={error} onRetry={onRetry} /> : null}
      {status?.status === "failed" ? <InlineError message={status.error?.message || t("creativeConsole.errors.videoFailed")} /> : null}
      {status?.status === "done" && status.video ? (
        <div className="space-y-3">
          <video src={status.video.url} controls preload="metadata" className="max-h-[60vh] w-full rounded-2xl bg-black shadow-sm" />
          <div className="flex flex-wrap items-center justify-between gap-2">
            <span className="text-xs text-muted-foreground">{status.video.duration ? t("creativeConsole.videoDuration", { count: status.video.duration }) : ""}</span>
            <Button variant="secondary" size="sm" asChild><a href={status.video.url} target="_blank" rel="noreferrer"><ExternalLink />{t("creativeConsole.openVideo")}</a></Button>
          </div>
        </div>
      ) : null}
    </div>
  );
}

function WelcomeState({ title }: { title: string }) {
  return (
    <div className="flex min-h-[24rem] items-center justify-center px-6 text-center">
      <h2 className="max-w-2xl text-2xl font-medium tracking-tight text-foreground/80 sm:text-3xl">{title}</h2>
    </div>
  );
}

function CompactModelSelect({ value, models, onChange }: { value: string; models: ModelRouteDTO[]; onChange: (model: string) => void }) {
  const { t } = useTranslation();
  return (
    <Select value={value} onValueChange={onChange} disabled={models.length === 0}>
      <SelectTrigger className="h-8 w-auto max-w-56 gap-1 border-0 bg-transparent px-2 shadow-none hover:bg-secondary/70 focus:bg-secondary/70 focus:ring-0" aria-label={t("creativeConsole.model")}>
        <SelectValue placeholder={models.length === 0 ? t("creativeConsole.noModels") : t("creativeConsole.selectModel")} />
      </SelectTrigger>
      <SelectContent>{models.map((item) => <SelectItem key={item.id} value={item.publicId}>{item.publicId}</SelectItem>)}</SelectContent>
    </Select>
  );
}

function CompactSelect({ value, options, onChange, ariaLabel, suffix, surfaced = false }: { value: string; options: readonly string[]; onChange: (value: string) => void; ariaLabel: string; suffix?: string; surfaced?: boolean }) {
  return (
    <Select value={value} onValueChange={onChange}>
      <SelectTrigger className={cn("h-8 w-auto gap-1 border-0 bg-transparent px-2 shadow-none hover:bg-secondary/70 focus:bg-secondary/70 focus:ring-0", surfaced && "w-full bg-background/70 hover:bg-background")} aria-label={ariaLabel}>
        <span className="flex min-w-0 items-center gap-0.5"><SelectValue />{suffix ? <span>{suffix}</span> : null}</span>
      </SelectTrigger>
      <SelectContent>{options.map((option) => <SelectItem key={option} value={option}>{option}{suffix}</SelectItem>)}</SelectContent>
    </Select>
  );
}

function messagesFromRequest(messages: ChatMessage[]): ConversationMessage[] {
  return messages
    .filter((message) => message.role !== "system")
    .map((message) => ({ ...message, id: createCreativeMessageId() }));
}

function ChatMessageItem({ message, loading = false }: { message: ConversationMessage; loading?: boolean }) {
  const isUser = message.role === "user";
  return (
    <Message align={isUser ? "end" : "start"}>
      {!isUser ? <MessageAvatar><Bot className="size-4" /></MessageAvatar> : null}
      <MessageContent>
        <div className={cn("whitespace-pre-wrap break-words text-sm leading-6", isUser ? "rounded-2xl rounded-br-md bg-secondary px-4 py-2.5" : "py-1")}>
          {loading ? <span className="flex items-center gap-2 text-muted-foreground"><Spinner />{message.content}</span> : message.content}
        </div>
      </MessageContent>
    </Message>
  );
}

function LoadingResult({ text }: { text: string }) {
  return <div className="flex min-h-[24rem] items-center justify-center gap-3 text-xs text-muted-foreground"><Spinner className="size-5" />{text}</div>;
}

function InlineError({ message }: { message: string }) {
  return <div role="alert" className="rounded-md bg-destructive/8 px-3 py-2 text-xs leading-5 text-destructive">{message}</div>;
}

function RetryableError({ message, onRetry }: { message: string; onRetry: () => void }) {
  const { t } = useTranslation();
  return (
    <div role="alert" className="flex flex-col gap-2 rounded-md bg-destructive/8 px-3 py-2 text-xs leading-5 text-destructive sm:flex-row sm:items-center sm:justify-between">
      <span>{message}</span>
      <Button type="button" variant="ghost" size="sm" className="self-start text-destructive hover:text-destructive sm:self-auto" onClick={onRetry}>
        <RefreshCw />{t("common.retry")}
      </Button>
    </div>
  );
}

function MetaItem({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) {
  return <div className="min-w-0 py-2"><div className="mb-1 text-[11px] text-muted-foreground">{label}</div><div className={cn("truncate text-xs", mono && "font-mono")} title={value}>{value}</div></div>;
}

function isUsableKey(key: ClientKeyDTO): boolean {
  if (!key.enabled) return false;
  return !key.expiresAt || new Date(key.expiresAt).getTime() > Date.now();
}

function validDuration(value: string): boolean {
  const duration = Number(value);
  return Number.isInteger(duration) && duration >= 1 && duration <= 15;
}

function uniqueModelsByPublicID(models: ModelRouteDTO[]): ModelRouteDTO[] {
  const seen = new Set<string>();
  return models.filter((model) => {
    if (seen.has(model.publicId)) return false;
    seen.add(model.publicId);
    return true;
  });
}

let fallbackMessageID = 0;

function createCreativeMessageId(): string {
  if (typeof globalThis.crypto?.randomUUID === "function") return globalThis.crypto.randomUUID();
  fallbackMessageID += 1;
  return `creative-${Date.now().toString(36)}-${fallbackMessageID.toString(36)}`;
}

async function listAllPaginatedItems<T>(loadPage: (page: number, pageSize: number) => Promise<{ items: T[]; total: number }>): Promise<T[]> {
  const items: T[] = [];
  for (let page = 1; page <= 50; page += 1) {
    const result = await loadPage(page, 100);
    items.push(...result.items);
    if (result.items.length === 0 || items.length >= result.total) break;
  }
  return items;
}
