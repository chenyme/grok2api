import { useMutation, useQuery } from "@tanstack/react-query";
import { ArrowUp, BrainCircuit, Check, CheckCircle2, Clock3, ExternalLink, Globe, History, ImageIcon, ImagePlus, ImageUpscale, Images, Loader2, MessageSquareText, RefreshCw, Sparkle, SquarePen, Trash2, TriangleAlert, TvMinimal, Video, Wrench, X } from "lucide-react";
import { marked } from "marked";
import { useEffect, useMemo, useRef, useState, type FormEvent, type KeyboardEvent, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuLabel, DropdownMenuTrigger } from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Message, MessageContent } from "@/components/ui/message";
import { MessageScroller, MessageScrollerButton, MessageScrollerContent, MessageScrollerItem, MessageScrollerProvider, MessageScrollerViewport } from "@/components/ui/message-scroller";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { listModels } from "@/entities/model/model-api";
import type { ModelRouteDTO } from "@/entities/model/types";
import {
  createChatResponse,
  createVideo,
  generateImage,
  getVideo,
  type ChatMessage,
  type ChatStreamSnapshot,
  type ChatToolActivity,
  type ImageResult,
  type ReasoningEffort,
  type VideoStatus,
} from "@/features/creative-console/creative-console-api";
import { getClientKeySecret, listClientKeys, type ClientKeyDTO } from "@/features/client-keys/client-keys-api";
import { PageHeader } from "@/shared/components/page-header";
import { cn } from "@/shared/lib/cn";

type CreativeMode = "chat" | "image" | "video";
type ConversationMessage = ChatMessage & {
  id: string;
  reasoning?: string;
  tools?: ChatToolActivity[];
};

type SecretState = {
  keyId: string;
  secret: string;
};

type ChatRequest = {
  messages: ChatMessage[];
  promptCacheKey: string;
  reasoningEffort: ReasoningEffort;
  webSearch: boolean;
  xSearch: boolean;
  assistantMessageId: string;
  apiKey: string;
  model: string;
};

type ChatSession = {
  id: string;
  title: string;
  createdAt: number;
  updatedAt: number;
  model: string;
  promptCacheKey: string;
  reasoningEffort: ReasoningEffort;
  webSearch: boolean;
  xSearch: boolean;
  messages: ConversationMessage[];
};

const imageAspectRatios = ["1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3"] as const;
const videoAspectRatios = ["1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3"] as const;
const imageResolutions = ["1k", "2k"] as const;
const videoResolutions = ["480p", "720p", "1080p"] as const;
const videoDurations = ["6", "10", "15"] as const;
const chatHistoryStoragePrefix = "grok2api:creative-console:chat-history:";
const chatHistoryMaxSessions = 50;
const chatHistoryMaxBytes = 4 * 1024 * 1024;
const composerClassName = "overflow-hidden rounded-2xl bg-secondary/45 ring-1 ring-transparent transition-colors focus-within:bg-secondary/60 focus-within:ring-ring";

export function CreativeConsolePage() {
  const { t } = useTranslation();
  const [mode, setMode] = useState<CreativeMode>("chat");
  const [selectedKeyId, setSelectedKeyId] = useState("");
  const [secretState, setSecretState] = useState<SecretState | null>(null);
  const [keyError, setKeyError] = useState("");
  const [selectedModels, setSelectedModels] = useState<Record<CreativeMode, string>>({ chat: "", image: "", video: "" });
  const [chatToolbarElement, setChatToolbarElement] = useState<HTMLDivElement | null>(null);
  const requestedSecretKeyRef = useRef("");

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
    onError: (error, id) => {
      if (id !== effectiveKeyId) return;
      setKeyError(error instanceof Error ? error.message : t("creativeConsole.errors.keyUnavailable"));
    },
  });

  const loadSecret = secretMutation.mutate;
  useEffect(() => {
    if (!effectiveKeyId) {
      requestedSecretKeyRef.current = "";
      return;
    }
    if (requestedSecretKeyRef.current === effectiveKeyId) return;
    requestedSecretKeyRef.current = effectiveKeyId;
    loadSecret(effectiveKeyId);
  }, [effectiveKeyId, loadSecret]);

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

  return (
    <div className="flex h-[calc(100dvh-5rem)] min-h-[36rem] flex-col gap-5 overflow-hidden">
      <PageHeader title={t("creativeConsole.title")} description={t("creativeConsole.description")} />

      <aside className="flex shrink-0 flex-col gap-2 rounded-lg bg-secondary/45 px-4 py-2.5 text-xs leading-5 text-muted-foreground sm:flex-row sm:items-center sm:justify-between sm:gap-4">
        <div className="flex min-w-0 items-center gap-3">
          <Sparkle className="size-4 shrink-0 text-foreground/70" />
          <p>{t("creativeConsole.promotion", { product: "DEEIX Chat" })}</p>
        </div>
        <a className="inline-flex shrink-0 items-center gap-1.5 self-end font-medium text-foreground hover:underline sm:self-auto" href="https://github.com/DEEIX-AI/DEEIX-Chat" target="_blank" rel="noopener noreferrer">
          {t("creativeConsole.promotionAction")}<ExternalLink className="size-3.5" />
        </a>
      </aside>

      <section className="flex min-h-0 flex-1 flex-col overflow-hidden">
        <div className="flex min-h-9 shrink-0 flex-col gap-3 lg:flex-row lg:items-center lg:justify-between">
          <Tabs value={mode} onValueChange={(value) => setMode(value as CreativeMode)}>
            <TabsList className="h-9 w-full rounded-full bg-secondary/50 p-1 lg:w-auto">
              <TabsTrigger className="flex-1 gap-1.5 rounded-full px-3 lg:min-w-20 [&_svg]:size-3.5" value="chat"><MessageSquareText />{t("creativeConsole.modes.chat")}</TabsTrigger>
              <TabsTrigger className="flex-1 gap-1.5 rounded-full px-3 lg:min-w-20 [&_svg]:size-3.5" value="image"><ImageIcon />{t("creativeConsole.modes.image")}</TabsTrigger>
              <TabsTrigger className="flex-1 gap-1.5 rounded-full px-3 lg:min-w-20 [&_svg]:size-3.5" value="video"><Video />{t("creativeConsole.modes.video")}</TabsTrigger>
            </TabsList>
          </Tabs>

          <div className="flex min-w-0 items-center gap-2">
            <Select value={effectiveKeyId} onValueChange={changeKey} disabled={keysQuery.isPending || activeKeys.length === 0}>
              <SelectTrigger id="creative-key" className="min-w-0 flex-1 bg-secondary/55 lg:w-64 lg:flex-none" aria-label={t("creativeConsole.clientKey")}>
                <SelectValue placeholder={keysQuery.isPending ? t("common.loading") : t("creativeConsole.selectKey")} />
              </SelectTrigger>
              <SelectContent>
                {activeKeys.map((key) => <SelectItem key={key.id} value={key.id}>{key.name} · {key.prefix}</SelectItem>)}
              </SelectContent>
            </Select>
            <div ref={setChatToolbarElement} className={cn("items-center gap-1", mode === "chat" ? "flex" : "hidden")} />
          </div>
        </div>

        <div className="shrink-0 space-y-2 px-3">
          {keysQuery.isError ? <RetryableError message={keysQuery.error.message} onRetry={() => void keysQuery.refetch()} /> : null}
          {!keysQuery.isPending && !keysQuery.isError && activeKeys.length === 0 ? <InlineError message={t("creativeConsole.errors.noKeys")} /> : null}
          {keyError ? <InlineError message={keyError} /> : null}
          {modelsQuery.isError ? <RetryableError message={modelsQuery.error.message} onRetry={() => void modelsQuery.refetch()} /> : null}
        </div>

        <div className="min-h-0 flex-1">
          <div className="h-full" hidden={mode !== "chat"}><ChatPanel key={effectiveKeyId || "default"} storageScope={effectiveKeyId || "default"} toolbarElement={chatToolbarElement} {...panelProps("chat")} /></div>
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

function ChatPanel({ apiKey, model, modelOptions, onModelChange, storageScope, toolbarElement }: CreativePanelProps & { storageScope: string; toolbarElement: HTMLDivElement | null }) {
  const { t, i18n } = useTranslation();
  const [initialHistory] = useState(() => {
    const sessions = loadChatSessions(storageScope);
    return { sessions, active: sessions[0] ?? createBlankChatSession(model) };
  });
  const [sessions, setSessions] = useState<ChatSession[]>(initialHistory.sessions);
  const [sessionId, setSessionId] = useState(initialHistory.active.id);
  const [sessionCreatedAt, setSessionCreatedAt] = useState(initialHistory.active.createdAt);
  const [webSearch, setWebSearch] = useState(initialHistory.active.webSearch);
  const [xSearch, setXSearch] = useState(initialHistory.active.xSearch);
  const [reasoningEffort, setReasoningEffort] = useState<ReasoningEffort>(initialHistory.active.reasoningEffort);
  const [promptCacheKey, setPromptCacheKey] = useState(initialHistory.active.promptCacheKey);
  const [prompt, setPrompt] = useState("");
  const [messages, setMessages] = useState<ConversationMessage[]>(initialHistory.active.messages);
  const streamSnapshotRef = useRef<ChatStreamSnapshot>({ text: "", reasoning: "", tools: [] });
  const streamFrameRef = useRef<number | null>(null);
  const requestControllerRef = useRef<AbortController | null>(null);
  const restoredInitialModelRef = useRef(false);

  useEffect(() => {
    if (restoredInitialModelRef.current || modelOptions.length === 0) return;
    restoredInitialModelRef.current = true;
    if (initialHistory.active.model && modelOptions.some((option) => option.publicId === initialHistory.active.model)) {
      onModelChange(initialHistory.active.model);
    }
  }, [initialHistory.active.model, modelOptions, onModelChange]);

  useEffect(() => {
    if (messages.length === 0) return;
    const timer = window.setTimeout(() => {
      const session: ChatSession = {
        id: sessionId,
        title: createChatSessionTitle(messages),
        createdAt: sessionCreatedAt,
        updatedAt: currentTimestamp(),
        model,
        promptCacheKey,
        reasoningEffort,
        webSearch,
        xSearch,
        messages,
      };
      setSessions((current) => {
        const next = upsertChatSession(current, session);
        return persistChatSessions(storageScope, next);
      });
    }, 300);
    return () => window.clearTimeout(timer);
  }, [messages, model, promptCacheKey, reasoningEffort, sessionCreatedAt, sessionId, storageScope, webSearch, xSearch]);

  useEffect(() => () => {
    if (streamFrameRef.current !== null) cancelAnimationFrame(streamFrameRef.current);
    requestControllerRef.current?.abort();
  }, []);

  function renderStreamSnapshot(messageId: string): void {
    if (streamFrameRef.current !== null) return;
    streamFrameRef.current = requestAnimationFrame(() => {
      streamFrameRef.current = null;
      const snapshot = streamSnapshotRef.current;
      setMessages((current) => current.map((message) => message.id === messageId
        ? { ...message, content: snapshot.text, reasoning: snapshot.reasoning, tools: snapshot.tools }
        : message));
    });
  }

  const mutation = useMutation({
    mutationFn: (request: ChatRequest) => {
      streamSnapshotRef.current = { text: "", reasoning: "", tools: [] };
      const controller = new AbortController();
      requestControllerRef.current = controller;
      return createChatResponse({
        apiKey: request.apiKey,
        model: request.model,
        messages: request.messages,
        promptCacheKey: request.promptCacheKey || undefined,
        reasoningEffort: request.reasoningEffort,
        webSearch: request.webSearch,
        xSearch: request.xSearch,
        signal: controller.signal,
        onUpdate: (snapshot) => {
          streamSnapshotRef.current = snapshot;
          renderStreamSnapshot(request.assistantMessageId);
        },
      });
    },
    onSuccess: (result, request) => {
      if (streamFrameRef.current !== null) cancelAnimationFrame(streamFrameRef.current);
      streamFrameRef.current = null;
      setMessages((current) => current.map((message) => message.id === request.assistantMessageId
        ? { ...message, content: result.text, reasoning: result.reasoning, tools: result.tools }
        : message));
      requestControllerRef.current = null;
    },
    onError: (_error, request) => {
      if (streamFrameRef.current !== null) cancelAnimationFrame(streamFrameRef.current);
      streamFrameRef.current = null;
      const snapshot = streamSnapshotRef.current;
      setMessages((current) => current.flatMap((message) => {
        if (message.id !== request.assistantMessageId) return [message];
        if (!snapshot.text.trim() && !snapshot.reasoning.trim() && snapshot.tools.length === 0) return [];
        return [{ ...message, content: snapshot.text, reasoning: snapshot.reasoning, tools: snapshot.tools }];
      }));
      requestControllerRef.current = null;
    },
  });

  function submit(event?: FormEvent): void {
    event?.preventDefault();
    const userText = prompt.trim();
    if (!apiKey || !model || !userText || mutation.isPending) return;
    const userMessage: ConversationMessage = { id: createCreativeMessageId(), role: "user", content: userText };
    const assistantMessage: ConversationMessage = { id: createCreativeMessageId(), role: "assistant", content: "", reasoning: "", tools: [] };
    const requestMessages: ChatMessage[] = [
      ...messages.filter((message) => message.content.trim()).map(({ role, content }) => ({ role, content })),
      { role: "user", content: userText },
    ];
    setMessages((current) => [...current, userMessage, assistantMessage]);
    setPrompt("");
    mutation.reset();
    mutation.mutate({
      messages: requestMessages,
      promptCacheKey,
      reasoningEffort,
      webSearch,
      xSearch,
      assistantMessageId: assistantMessage.id,
      apiKey,
      model,
    });
  }

  function clearConversation(): void {
    setSessions((current) => {
      const next = current.filter((session) => session.id !== sessionId);
      return persistChatSessions(storageScope, next);
    });
    const blank = createBlankChatSession(model);
    setSessionId(blank.id);
    setSessionCreatedAt(blank.createdAt);
    setMessages([]);
    setPromptCacheKey(blank.promptCacheKey);
    setPrompt("");
    mutation.reset();
  }

  function startNewConversation(): void {
    if (mutation.isPending) return;
    setSessions((current) => {
      const next = messages.length > 0 ? upsertChatSession(current, {
        id: sessionId,
        title: createChatSessionTitle(messages),
        createdAt: sessionCreatedAt,
        updatedAt: currentTimestamp(),
        model,
        promptCacheKey,
        reasoningEffort,
        webSearch,
        xSearch,
        messages,
      }) : current;
      return persistChatSessions(storageScope, next);
    });
    const blank = createBlankChatSession(model);
    setSessionId(blank.id);
    setSessionCreatedAt(blank.createdAt);
    setMessages([]);
    setPromptCacheKey(blank.promptCacheKey);
    setReasoningEffort(blank.reasoningEffort);
    setWebSearch(blank.webSearch);
    setXSearch(blank.xSearch);
    setPrompt("");
    mutation.reset();
  }

  function switchConversation(targetId: string): void {
    if (mutation.isPending || targetId === sessionId) return;
    let availableSessions = sessions;
    if (messages.length > 0) {
      availableSessions = upsertChatSession(sessions, {
        id: sessionId,
        title: createChatSessionTitle(messages),
        createdAt: sessionCreatedAt,
        updatedAt: currentTimestamp(),
        model,
        promptCacheKey,
        reasoningEffort,
        webSearch,
        xSearch,
        messages,
      });
    }
    const target = availableSessions.find((session) => session.id === targetId);
    if (!target) return;
    availableSessions = persistChatSessions(storageScope, availableSessions);
    setSessions(availableSessions);
    setSessionId(target.id);
    setSessionCreatedAt(target.createdAt);
    setMessages(target.messages);
    setPromptCacheKey(target.promptCacheKey || createCreativeCacheKey());
    setReasoningEffort(target.reasoningEffort);
    setWebSearch(target.webSearch);
    setXSearch(target.xSearch);
    setPrompt("");
    mutation.reset();
    if (target.model && modelOptions.some((option) => option.publicId === target.model)) onModelChange(target.model);
  }

  function handlePromptKeyDown(event: KeyboardEvent<HTMLTextAreaElement>): void {
    if (event.key === "Enter" && !event.shiftKey) {
      event.preventDefault();
      submit();
    }
  }

  return (
    <div className="flex h-full min-h-0 flex-col overflow-hidden">
      {toolbarElement ? createPortal(<>
        <Tooltip>
          <TooltipTrigger asChild>
            <Button type="button" variant="ghost" size="icon" className="rounded-full" aria-label={t("creativeConsole.newConversation")} onClick={startNewConversation} disabled={mutation.isPending}>
              <SquarePen />
            </Button>
          </TooltipTrigger>
          <TooltipContent>{t("creativeConsole.newConversation")}</TooltipContent>
        </Tooltip>
        <Tooltip>
          <TooltipTrigger asChild>
            <Button type="button" variant="ghost" size="icon" className="rounded-full" aria-label={t("creativeConsole.clearCurrent")} onClick={clearConversation} disabled={messages.length === 0 || mutation.isPending}>
              <Trash2 />
            </Button>
          </TooltipTrigger>
          <TooltipContent>{t("creativeConsole.clearCurrent")}</TooltipContent>
        </Tooltip>
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button type="button" variant="ghost" size="icon" className="rounded-full" aria-label={t("creativeConsole.history")} disabled={mutation.isPending}>
              <History />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="w-80">
            <DropdownMenuLabel>{t("creativeConsole.history")}</DropdownMenuLabel>
            {sessions.length === 0 ? (
              <div className="px-2 py-5 text-center text-xs text-muted-foreground">{t("creativeConsole.noHistory")}</div>
            ) : sessions.map((session) => (
              <DropdownMenuItem key={session.id} className="min-h-12 gap-2" onSelect={() => switchConversation(session.id)}>
                <div className="min-w-0 flex-1">
                  <div className="truncate text-xs">{session.title}</div>
                  <div className="mt-0.5 truncate text-[10px] text-muted-foreground">{session.model || t("creativeConsole.model")} · {formatChatSessionTime(session.updatedAt, i18n.language)}</div>
                </div>
                {session.id === sessionId ? <Check className="text-muted-foreground" /> : null}
              </DropdownMenuItem>
            ))}
          </DropdownMenuContent>
        </DropdownMenu>
      </>, toolbarElement) : null}
      <MessageScrollerProvider autoScroll defaultScrollPosition="end">
        <MessageScroller className="min-h-0 flex-1">
          <MessageScrollerViewport aria-label={t("creativeConsole.messageList")}>
            <MessageScrollerContent className={cn("w-full px-3 py-6 sm:px-6", messages.length === 0 && !mutation.isPending && "justify-center")}>
              {messages.length === 0 && !mutation.isPending ? <WelcomeState title={t("creativeConsole.welcome")} /> : null}
              {messages.map((message) => (
                <MessageScrollerItem key={message.id} messageId={message.id} scrollAnchor={message.role === "user"}>
                  <ChatMessageItem message={message} loading={mutation.isPending && mutation.variables?.assistantMessageId === message.id} />
                </MessageScrollerItem>
              ))}
            </MessageScrollerContent>
          </MessageScrollerViewport>
          <MessageScrollerButton aria-label={t("creativeConsole.scrollToLatest")} />
        </MessageScroller>
      </MessageScrollerProvider>

      <form className="w-full shrink-0 px-3 pb-2 sm:px-6 sm:pb-3" onSubmit={submit}>
        <div className={composerClassName}>
          <Textarea id="chat-prompt" value={prompt} onChange={(event) => setPrompt(event.target.value)} onKeyDown={handlePromptKeyDown} placeholder={t("creativeConsole.chatPlaceholder")} className="min-h-24 resize-none border-0 bg-transparent px-4 py-3 text-sm focus-visible:ring-0" />
          <div className="flex items-center justify-between gap-3 px-3 pb-3">
            <div className="flex min-w-0 items-center gap-0.5 overflow-x-auto">
              <CompactModelSelect value={model} models={modelOptions} onChange={onModelChange} />
              <CompactIconSelect
                value={webSearch ? "on" : "off"}
                options={[{ value: "off", label: t("creativeConsole.webSearchOff") }, { value: "on", label: t("creativeConsole.webSearchOn") }]}
                onChange={(value) => setWebSearch(value === "on")}
                ariaLabel={t("creativeConsole.webSearch")}
                icon={<Globe />}
                active={webSearch}
              />
              <CompactIconSelect
                value={xSearch ? "on" : "off"}
                options={[{ value: "off", label: t("creativeConsole.xSearchOff") }, { value: "on", label: t("creativeConsole.xSearchOn") }]}
                onChange={(value) => setXSearch(value === "on")}
                ariaLabel={t("creativeConsole.xSearch")}
                icon={<XSocialIcon />}
                active={xSearch}
              />
              <CompactIconSelect
                value={reasoningEffort}
                options={(["auto", "none", "low", "medium", "high", "xhigh"] as ReasoningEffort[]).map((effort) => ({ value: effort, label: t(`creativeConsole.reasoning.${effort}`) }))}
                onChange={(value) => setReasoningEffort(value as ReasoningEffort)}
                ariaLabel={t("creativeConsole.reasoningEffort")}
                icon={<Sparkle />}
                active={reasoningEffort !== "auto" && reasoningEffort !== "none"}
              />
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
        <div className={composerClassName}>
          <Textarea id="image-prompt" value={prompt} onChange={(event) => setPrompt(event.target.value)} placeholder={t("creativeConsole.imagePlaceholder")} className="min-h-24 resize-none border-0 bg-transparent px-4 py-3 text-sm focus-visible:ring-0" />
          <div className="flex flex-wrap items-center justify-between gap-2 px-3 pb-3">
            <div className="flex min-w-0 flex-wrap items-center gap-1">
              <CompactModelSelect value={model} models={modelOptions} onChange={onModelChange} />
              <CompactSelect value={count} options={["1", "2", "3", "4"]} onChange={setCount} ariaLabel={t("creativeConsole.count")} suffix="×" icon={<Images />} />
              <CompactSelect value={aspectRatio} options={imageAspectRatios} onChange={setAspectRatio} ariaLabel={t("creativeConsole.aspectRatio")} icon={<TvMinimal />} />
              <CompactSelect value={resolution} options={imageResolutions} onChange={setResolution} ariaLabel={t("creativeConsole.resolution")} icon={<ImageUpscale />} />
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
  const [duration, setDuration] = useState("6");
  const [aspectRatio, setAspectRatio] = useState("16:9");
  const [resolution, setResolution] = useState("720p");
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
        <div className={composerClassName}>
          <Textarea id="video-prompt" value={prompt} onChange={(event) => setPrompt(event.target.value)} placeholder={t("creativeConsole.videoPlaceholder")} className="min-h-24 resize-none border-0 bg-transparent px-4 py-3 text-sm focus-visible:ring-0" />
          <div className="flex items-center justify-between gap-3 px-3 pb-3">
            <div className="flex min-w-0 items-center gap-1 overflow-x-auto">
              <CompactModelSelect value={model} models={modelOptions} onChange={onModelChange} />
              <Popover>
                <PopoverTrigger asChild>
                  <Button type="button" variant="ghost" size="sm" className={cn("h-8 gap-1.5 px-2 font-normal", imageURL && "bg-secondary/70 text-foreground")} aria-label={t("creativeConsole.referenceImage")}>
                    <ImagePlus />{imageURL ? t("creativeConsole.referenceImageAdded") : t("creativeConsole.referenceImageShort")}
                  </Button>
                </PopoverTrigger>
                <PopoverContent align="start" className="w-80 p-3">
                  <div className="mb-2 text-xs font-medium">{t("creativeConsole.referenceImage")}</div>
                  <div className="flex items-center gap-2">
                    <Input id="video-image" type="url" value={imageURL} onChange={(event) => setImageURL(event.target.value)} placeholder="https://..." aria-label={t("creativeConsole.referenceImage")} />
                    {imageURL ? <Button type="button" variant="ghost" size="icon" className="shrink-0" aria-label={t("creativeConsole.clearReferenceImage")} onClick={() => setImageURL("")}><X /></Button> : null}
                  </div>
                </PopoverContent>
              </Popover>
              <CompactSelect value={duration} options={videoDurations} onChange={setDuration} ariaLabel={t("creativeConsole.duration")} suffix="s" icon={<Clock3 />} />
              <CompactSelect value={aspectRatio} options={videoAspectRatios} onChange={setAspectRatio} ariaLabel={t("creativeConsole.aspectRatio")} icon={<TvMinimal />} />
              <CompactSelect value={resolution} options={videoResolutions} onChange={setResolution} ariaLabel={t("creativeConsole.resolution")} icon={<ImageUpscale />} />
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
    <div className="flex min-h-[20rem] items-center justify-center px-6 text-center">
      <h2 className="max-w-2xl text-xl font-medium tracking-tight text-muted-foreground sm:text-2xl">{title}</h2>
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

function CompactSelect({ value, options, onChange, ariaLabel, suffix, icon }: { value: string; options: readonly string[]; onChange: (value: string) => void; ariaLabel: string; suffix?: string; icon?: ReactNode }) {
  return (
    <Select value={value} onValueChange={onChange}>
      <SelectTrigger className="h-8 w-auto gap-1.5 border-0 bg-transparent px-2 shadow-none hover:bg-secondary/70 focus:bg-secondary/70 focus:ring-0 [&>svg]:size-3.5 [&>svg]:shrink-0" aria-label={ariaLabel}>
        {icon}<SelectValue />
      </SelectTrigger>
      <SelectContent>{options.map((option) => <SelectItem key={option} value={option}>{option}{suffix}</SelectItem>)}</SelectContent>
    </Select>
  );
}

function CompactIconSelect({ value, options, onChange, ariaLabel, icon, active = false }: { value: string; options: Array<{ value: string; label: string }>; onChange: (value: string) => void; ariaLabel: string; icon: ReactNode; active?: boolean }) {
  const selectedLabel = options.find((option) => option.value === value)?.label ?? ariaLabel;
  return (
    <Select value={value} onValueChange={onChange}>
      <Tooltip>
        <TooltipTrigger asChild>
          <SelectTrigger className={cn("h-8 w-auto min-w-8 gap-1 bg-transparent px-2 shadow-none hover:bg-secondary/70 focus:bg-secondary/70 focus:ring-0", active && "bg-secondary/70 text-foreground")} aria-label={`${ariaLabel}: ${selectedLabel}`}>
            <span className="flex items-center [&_svg]:size-3.5">{icon}</span>
          </SelectTrigger>
        </TooltipTrigger>
        <TooltipContent>{ariaLabel} · {selectedLabel}</TooltipContent>
      </Tooltip>
      <SelectContent>{options.map((option) => <SelectItem key={option.value} value={option.value}>{option.label}</SelectItem>)}</SelectContent>
    </Select>
  );
}

function XSocialIcon({ className }: { className?: string }) {
  return (
    <svg className={className} viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
      <path d="M18.244 2.25h3.308l-7.227 8.26 8.502 11.24H16.17l-5.214-6.817L4.99 21.75H1.68l7.73-8.835L1.254 2.25H8.08l4.713 6.231zm-1.161 17.52h1.833L7.084 4.126H5.117z" />
    </svg>
  );
}

function ChatMessageItem({ message, loading = false }: { message: ConversationMessage; loading?: boolean }) {
  const { t } = useTranslation();
  const isUser = message.role === "user";
  return (
    <Message align={isUser ? "end" : "start"}>
      <MessageContent className={cn(!isUser && "w-full max-w-full")}>
        {!isUser && message.reasoning ? (
          <div className="w-full rounded-xl bg-secondary/45 px-3 py-2.5 text-xs text-muted-foreground">
            <div className="mb-1.5 flex items-center gap-1.5 font-medium text-foreground/75"><BrainCircuit className="size-3.5" />{t("creativeConsole.thinkingProcess")}</div>
            <div className="whitespace-pre-wrap break-words leading-5">{message.reasoning}</div>
          </div>
        ) : null}
        {!isUser && message.tools?.length ? (
          <div className="flex w-full flex-col gap-1.5">
            {message.tools.map((tool) => <ToolActivityItem key={tool.id} tool={tool} />)}
          </div>
        ) : null}
        {message.content || isUser ? (
          isUser ? (
            <div className="whitespace-pre-wrap break-words rounded-2xl rounded-br-md bg-secondary px-4 py-2.5 text-sm leading-6">{message.content}</div>
          ) : <AssistantContent content={message.content} />
        ) : null}
        {loading ? <div className="flex items-center gap-2 py-1 text-xs text-muted-foreground"><Spinner />{t("creativeConsole.streaming")}</div> : null}
      </MessageContent>
    </Message>
  );
}

function AssistantContent({ content }: { content: string }) {
  const renderedHTML = useMemo(() => renderAssistantMarkup(content), [content]);
  if (!renderedHTML) return <div className="w-full whitespace-pre-wrap break-words py-1 text-sm leading-6">{content}</div>;
  return (
    <div
      className="w-full break-words py-1 text-sm leading-6 [&>:first-child]:mt-0 [&>:last-child]:mb-0 [&_a]:text-primary [&_a]:underline [&_a]:underline-offset-2 [&_blockquote]:my-3 [&_blockquote]:border-l-2 [&_blockquote]:border-border [&_blockquote]:pl-3 [&_code]:rounded [&_code]:bg-secondary [&_code]:px-1 [&_code]:py-0.5 [&_h1]:mb-3 [&_h1]:mt-5 [&_h1]:text-xl [&_h1]:font-semibold [&_h2]:mb-2 [&_h2]:mt-4 [&_h2]:text-lg [&_h2]:font-semibold [&_h3]:mb-2 [&_h3]:mt-3 [&_h3]:font-semibold [&_hr]:my-4 [&_hr]:border-border [&_img]:my-3 [&_img]:max-h-[32rem] [&_img]:max-w-full [&_img]:rounded-xl [&_li]:my-1 [&_ol]:my-3 [&_ol]:list-decimal [&_ol]:pl-6 [&_p]:my-2 [&_pre]:my-3 [&_pre]:overflow-x-auto [&_pre]:rounded-xl [&_pre]:bg-secondary [&_pre]:p-3 [&_pre_code]:bg-transparent [&_pre_code]:p-0 [&_table]:my-3 [&_table]:w-full [&_table]:border-collapse [&_td]:border-b [&_td]:border-border [&_td]:px-3 [&_td]:py-2 [&_th]:border-b [&_th]:border-border [&_th]:px-3 [&_th]:py-2 [&_th]:text-left [&_ul]:my-3 [&_ul]:list-disc [&_ul]:pl-6"
      dangerouslySetInnerHTML={{ __html: renderedHTML }}
    />
  );
}

function ToolActivityItem({ tool }: { tool: ChatToolActivity }) {
  const { t } = useTranslation();
  const isWebSearch = tool.name === "web_search" || tool.type === "web_search_call";
  const isXSearch = tool.name === "x_search" || tool.type === "x_search_call";
  const label = isWebSearch
    ? t("creativeConsole.toolNames.webSearch")
    : isXSearch
      ? t("creativeConsole.toolNames.xSearch")
      : tool.name;
  const statusLabel = t(`creativeConsole.toolStatus.${tool.status}`);
  return (
    <div className="flex min-w-0 items-start gap-2 rounded-xl bg-secondary/45 px-3 py-2.5 text-xs">
      <span className="mt-0.5 text-muted-foreground">
        {isWebSearch ? <Globe className="size-3.5" /> : isXSearch ? <XSocialIcon className="size-3.5" /> : <Wrench className="size-3.5" />}
      </span>
      <div className="min-w-0 flex-1">
        <div className="flex min-w-0 items-center gap-2">
          <span className="truncate font-medium">{t("creativeConsole.toolCall")} · {label}</span>
          <span className="ml-auto flex shrink-0 items-center gap-1 text-muted-foreground">
            {tool.status === "in_progress" ? <Loader2 className="size-3 animate-spin" /> : tool.status === "failed" ? <TriangleAlert className="size-3 text-destructive" /> : <CheckCircle2 className="size-3" />}
            {statusLabel}
          </span>
        </div>
        {tool.detail ? <div className="mt-1 line-clamp-2 break-all leading-5 text-muted-foreground" title={tool.detail}>{tool.detail}</div> : null}
      </div>
    </div>
  );
}

function LoadingResult({ text }: { text: string }) {
  return <div className="flex min-h-[20rem] items-center justify-center gap-3 text-xs text-muted-foreground"><Spinner className="size-5" />{text}</div>;
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

function createCreativeCacheKey(): string {
  return `creative-console-${createCreativeMessageId()}`;
}

function currentTimestamp(): number {
  return Date.now();
}

function createBlankChatSession(model: string): ChatSession {
  const now = Date.now();
  return {
    id: createCreativeMessageId(),
    title: "",
    createdAt: now,
    updatedAt: now,
    model,
    promptCacheKey: createCreativeCacheKey(),
    reasoningEffort: "auto",
    webSearch: false,
    xSearch: false,
    messages: [],
  };
}

function createChatSessionTitle(messages: ConversationMessage[]): string {
  const title = messages.find((message) => message.role === "user")?.content.replace(/\s+/g, " ").trim() ?? "";
  return title.length > 48 ? `${title.slice(0, 48)}…` : title || "Conversation";
}

function upsertChatSession(sessions: ChatSession[], session: ChatSession): ChatSession[] {
  return [session, ...sessions.filter((item) => item.id !== session.id)]
    .sort((left, right) => right.updatedAt - left.updatedAt)
    .slice(0, chatHistoryMaxSessions);
}

function chatHistoryStorageKey(scope: string): string {
  return `${chatHistoryStoragePrefix}${encodeURIComponent(scope)}`;
}

function loadChatSessions(scope: string): ChatSession[] {
  if (typeof window === "undefined") return [];
  try {
    const parsed: unknown = JSON.parse(window.localStorage.getItem(chatHistoryStorageKey(scope)) ?? "[]");
    if (!Array.isArray(parsed)) return [];
    return parsed.flatMap(parseChatSession).sort((left, right) => right.updatedAt - left.updatedAt).slice(0, chatHistoryMaxSessions);
  } catch {
    return [];
  }
}

function persistChatSessions(scope: string, sessions: ChatSession[]): ChatSession[] {
  if (typeof window === "undefined") return sessions;
  const retained = sessions.slice(0, chatHistoryMaxSessions);
  while (retained.length > 0) {
    try {
      const serialized = JSON.stringify(retained);
      if (serialized.length * 2 > chatHistoryMaxBytes) {
        retained.pop();
        continue;
      }
      window.localStorage.setItem(chatHistoryStorageKey(scope), serialized);
      return retained;
    } catch {
      retained.pop();
    }
  }
  try {
    window.localStorage.removeItem(chatHistoryStorageKey(scope));
  } catch {
    // Storage may be unavailable; the in-memory conversation remains usable.
  }
  return retained;
}

function parseChatSession(value: unknown): ChatSession[] {
  if (!isLocalRecord(value) || typeof value.id !== "string" || !Array.isArray(value.messages)) return [];
  const messages = value.messages.flatMap(parseConversationMessage);
  if (messages.length === 0) return [];
  const now = Date.now();
  const createdAt = finiteTimestamp(value.createdAt) ?? now;
  const updatedAt = finiteTimestamp(value.updatedAt) ?? createdAt;
  return [{
    id: value.id,
    title: typeof value.title === "string" && value.title.trim() ? value.title.trim() : createChatSessionTitle(messages),
    createdAt,
    updatedAt,
    model: typeof value.model === "string" ? value.model : "",
    promptCacheKey: typeof value.promptCacheKey === "string" && value.promptCacheKey ? value.promptCacheKey : createCreativeCacheKey(),
    reasoningEffort: isReasoningEffort(value.reasoningEffort) ? value.reasoningEffort : "auto",
    webSearch: value.webSearch === true,
    xSearch: value.xSearch === true,
    messages,
  }];
}

function parseConversationMessage(value: unknown): ConversationMessage[] {
  if (!isLocalRecord(value) || (value.role !== "user" && value.role !== "assistant") || typeof value.content !== "string") return [];
  return [{
    id: typeof value.id === "string" && value.id ? value.id : createCreativeMessageId(),
    role: value.role,
    content: value.content,
    reasoning: typeof value.reasoning === "string" ? value.reasoning : undefined,
    tools: Array.isArray(value.tools) ? value.tools.flatMap(parseChatToolActivity) : undefined,
  }];
}

function parseChatToolActivity(value: unknown): ChatToolActivity[] {
  if (!isLocalRecord(value) || typeof value.id !== "string" || typeof value.type !== "string" || typeof value.name !== "string") return [];
  const status = value.status === "completed" || value.status === "failed" || value.status === "in_progress" ? value.status : "completed";
  return [{ id: value.id, type: value.type, name: value.name, status, detail: typeof value.detail === "string" ? value.detail : "" }];
}

function isReasoningEffort(value: unknown): value is ReasoningEffort {
  return value === "auto" || value === "none" || value === "low" || value === "medium" || value === "high" || value === "xhigh";
}

function finiteTimestamp(value: unknown): number | null {
  return typeof value === "number" && Number.isFinite(value) && value > 0 ? value : null;
}

function isLocalRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function formatChatSessionTime(value: number, language: string): string {
  return new Intl.DateTimeFormat(language, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" }).format(new Date(value));
}

const safeAssistantHTMLTags = new Set([
  "a", "b", "blockquote", "br", "code", "del", "details", "div", "em", "h1", "h2", "h3", "h4", "h5", "h6",
  "hr", "i", "img", "kbd", "li", "mark", "ol", "p", "pre", "s", "span", "strong", "sub", "summary", "sup", "table",
  "tbody", "td", "tfoot", "th", "thead", "tr", "u", "ul",
]);
const discardedAssistantHTMLTags = new Set([
  "applet", "audio", "base", "button", "canvas", "embed", "form", "frame", "frameset", "iframe", "input", "link",
  "math", "meta", "object", "picture", "script", "select", "source", "style", "svg", "template", "textarea", "video",
]);

function renderAssistantMarkup(content: string): string {
  const rendered = marked.parse(content, { async: false, breaks: true, gfm: true });
  return sanitizeAssistantHTML(typeof rendered === "string" ? rendered : "");
}

function sanitizeAssistantHTML(content: string): string {
  if (typeof DOMParser === "undefined") return "";
  const source = content.trim();
  if (!/<\/?[a-z][^>]*>/i.test(source)) return "";
  const documentValue = new DOMParser().parseFromString(source, "text/html");
  const elements = Array.from(documentValue.body.querySelectorAll("*"));
  for (const element of elements) {
    if (!element.isConnected) continue;
    const tag = element.tagName.toLowerCase();
    if (discardedAssistantHTMLTags.has(tag)) {
      element.remove();
      continue;
    }
    if (!safeAssistantHTMLTags.has(tag)) {
      element.replaceWith(...Array.from(element.childNodes));
      continue;
    }
    const href = tag === "a" ? safeAssistantLink(element.getAttribute("href")) : "";
    const title = tag === "a" ? element.getAttribute("title")?.slice(0, 512) ?? "" : "";
    const imageSource = tag === "img" ? safeAssistantImage(element.getAttribute("src")) : "";
    const imageAlt = tag === "img" ? element.getAttribute("alt")?.slice(0, 512) ?? "" : "";
    const colSpan = tag === "td" || tag === "th" ? boundedTableSpan(element.getAttribute("colspan")) : "";
    const rowSpan = tag === "td" || tag === "th" ? boundedTableSpan(element.getAttribute("rowspan")) : "";
    const open = tag === "details" && element.hasAttribute("open");
    for (const attribute of Array.from(element.attributes)) element.removeAttribute(attribute.name);
    if (href) {
      element.setAttribute("href", href);
      element.setAttribute("target", "_blank");
      element.setAttribute("rel", "nofollow noopener noreferrer");
    }
    if (title) element.setAttribute("title", title);
    if (imageSource) {
      element.setAttribute("src", imageSource);
      element.setAttribute("alt", imageAlt);
      element.setAttribute("loading", "lazy");
      element.setAttribute("decoding", "async");
      element.setAttribute("referrerpolicy", "no-referrer");
    } else if (tag === "img") {
      element.remove();
      continue;
    }
    if (colSpan) element.setAttribute("colspan", colSpan);
    if (rowSpan) element.setAttribute("rowspan", rowSpan);
    if (open) element.setAttribute("open", "");
  }
  return documentValue.body.innerHTML;
}

function safeAssistantLink(value: string | null): string {
  const link = value?.trim() ?? "";
  if (!link) return "";
  try {
    const parsed = new URL(link);
    return parsed.protocol === "http:" || parsed.protocol === "https:" || parsed.protocol === "mailto:" ? parsed.toString() : "";
  } catch {
    return "";
  }
}

function safeAssistantImage(value: string | null): string {
  const source = value?.trim() ?? "";
  if (!source) return "";
  if (source.startsWith("/v1/media/images/")) return source;
  try {
    const parsed = new URL(source);
    return parsed.protocol === "https:" ? parsed.toString() : "";
  } catch {
    return "";
  }
}

function boundedTableSpan(value: string | null): string {
  const parsed = Number.parseInt(value ?? "", 10);
  return Number.isInteger(parsed) && parsed >= 1 && parsed <= 100 ? String(parsed) : "";
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
