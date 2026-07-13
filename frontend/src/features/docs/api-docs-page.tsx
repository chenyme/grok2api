import { useQuery } from "@tanstack/react-query";
import { Braces, Check, Code2, Copy, Info, Link2 } from "lucide-react";
import { useState, type ReactNode } from "react";
import { Navigate, useParams } from "react-router-dom";
import { useTranslation } from "react-i18next";

import { Button } from "@/components/ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { listModels } from "@/entities/model/model-api";
import type { ModelRouteDTO } from "@/entities/model/types";
import { getSystemInfo } from "@/entities/system/system-api";
import { runtimeConfig } from "@/shared/config/runtime-config";
import { cn } from "@/shared/lib/cn";

type ExampleLanguage = "curl" | "python" | "javascript";
type ExampleView = "request" | "response";
type Method = "GET" | "POST";
type Capability = ModelRouteDTO["capability"];

type FieldDefinition = {
  name: string;
  required?: boolean;
  descriptionKey: string;
};

type EndpointDefinition = {
  key: string;
  category: string;
  title: string;
  method: Method;
  path: string;
  descriptionKey: string;
  capabilities: Capability[];
  fields: FieldDefinition[];
  noteKeys: string[];
  request: (model: string) => Record<string, unknown> | undefined;
  response: Record<string, unknown>;
};

const exampleLanguages: ExampleLanguage[] = ["curl", "python", "javascript"];

const endpoints: Record<string, EndpointDefinition> = {
  "chat/completions": {
    key: "chat/completions", category: "Chat", title: "Chat completions", method: "POST", path: "/chat/completions",
    descriptionKey: "docs.endpointChat", capabilities: ["chat", "responses"],
    fields: [
      { name: "model", required: true, descriptionKey: "docs.reference.fieldModel" },
      { name: "messages", required: true, descriptionKey: "docs.reference.fieldChatMessages" },
      { name: "stream", descriptionKey: "docs.reference.fieldStream" },
      { name: "max_completion_tokens", descriptionKey: "docs.reference.fieldMaxCompletionTokens" },
      { name: "tools", descriptionKey: "docs.reference.fieldTools" },
      { name: "tool_choice", descriptionKey: "docs.reference.fieldToolChoice" },
    ],
    noteKeys: ["docs.reference.noteChatImages", "docs.reference.noteChatTools"],
    request: (model) => ({ model, messages: [{ role: "system", content: "You are a concise assistant." }, { role: "user", content: "Explain HTTP streaming." }], stream: false }),
    response: { id: "chatcmpl_example", object: "chat.completion", model: "grok-chat-fast", choices: [{ index: 0, message: { role: "assistant", content: "HTTP streaming sends response data incrementally." }, finish_reason: "stop" }], usage: { prompt_tokens: 24, completion_tokens: 12, total_tokens: 36 } },
  },
  "chat/responses": {
    key: "chat/responses", category: "Chat", title: "Responses", method: "POST", path: "/responses",
    descriptionKey: "docs.endpointResponses", capabilities: ["chat", "responses"],
    fields: [
      { name: "model", required: true, descriptionKey: "docs.reference.fieldModel" },
      { name: "input", required: true, descriptionKey: "docs.reference.fieldInput" },
      { name: "instructions", descriptionKey: "docs.reference.fieldInstructions" },
      { name: "stream", descriptionKey: "docs.reference.fieldStream" },
      { name: "store", descriptionKey: "docs.reference.fieldStore" },
      { name: "previous_response_id", descriptionKey: "docs.reference.fieldPreviousResponse" },
      { name: "tools", descriptionKey: "docs.reference.fieldTools" },
    ],
    noteKeys: ["docs.reference.noteResponsesState", "docs.reference.noteResponsesCompact"],
    request: (model) => ({ model, input: "Explain HTTP streaming.", store: true, stream: false }),
    response: { id: "resp_example", object: "response", status: "completed", model: "grok-chat-fast", output: [{ type: "message", role: "assistant", status: "completed", content: [{ type: "output_text", text: "HTTP streaming sends response data incrementally." }] }], usage: { input_tokens: 18, output_tokens: 12, total_tokens: 30 } },
  },
  "chat/messages": {
    key: "chat/messages", category: "Chat", title: "Messages", method: "POST", path: "/messages",
    descriptionKey: "docs.endpointMessages", capabilities: ["chat", "responses"],
    fields: [
      { name: "model", required: true, descriptionKey: "docs.reference.fieldModel" },
      { name: "max_tokens", required: true, descriptionKey: "docs.reference.fieldMaxTokens" },
      { name: "messages", required: true, descriptionKey: "docs.reference.fieldMessages" },
      { name: "system", descriptionKey: "docs.reference.fieldSystem" },
      { name: "stream", descriptionKey: "docs.reference.fieldStream" },
      { name: "tools", descriptionKey: "docs.reference.fieldAnthropicTools" },
      { name: "tool_choice", descriptionKey: "docs.reference.fieldToolChoice" },
    ],
    noteKeys: ["docs.reference.noteMessagesEvents", "docs.reference.noteMessagesLimits"],
    request: (model) => ({ model, max_tokens: 1024, system: "You are a concise assistant.", messages: [{ role: "user", content: "Explain HTTP streaming." }], stream: false }),
    response: { id: "msg_example", type: "message", role: "assistant", model: "grok-chat-fast", content: [{ type: "text", text: "HTTP streaming sends response data incrementally." }], stop_reason: "end_turn", stop_sequence: null, usage: { input_tokens: 18, output_tokens: 12 } },
  },
  "image/generations": {
    key: "image/generations", category: "Image", title: "Image generations", method: "POST", path: "/images/generations",
    descriptionKey: "docs.endpointImageGeneration", capabilities: ["image"],
    fields: [
      { name: "model", required: true, descriptionKey: "docs.reference.fieldModel" },
      { name: "prompt", required: true, descriptionKey: "docs.reference.fieldPrompt" },
      { name: "n", descriptionKey: "docs.reference.fieldImageCount" },
      { name: "aspect_ratio", descriptionKey: "docs.reference.fieldAspectRatio" },
      { name: "resolution", descriptionKey: "docs.reference.fieldResolution" },
      { name: "response_format", descriptionKey: "docs.reference.fieldResponseFormat" },
      { name: "stream", descriptionKey: "docs.reference.fieldImageStream" },
    ],
    noteKeys: ["docs.reference.noteImageCount", "docs.reference.noteImageStorage"],
    request: (model) => ({ model, prompt: "A minimal red chair in a bright studio", n: 1, response_format: "url" }),
    response: { created: 1783860000, data: [{ url: "http://127.0.0.1:8000/v1/media/images/example" }] },
  },
  "image/edits": {
    key: "image/edits", category: "Image", title: "Image edits", method: "POST", path: "/images/edits",
    descriptionKey: "docs.endpointImageEdit", capabilities: ["image_edit"],
    fields: [
      { name: "model", required: true, descriptionKey: "docs.reference.fieldModel" },
      { name: "prompt", required: true, descriptionKey: "docs.reference.fieldPrompt" },
      { name: "image / images", required: true, descriptionKey: "docs.reference.fieldEditImages" },
      { name: "n", descriptionKey: "docs.reference.fieldImageCount" },
      { name: "response_format", descriptionKey: "docs.reference.fieldResponseFormat" },
    ],
    noteKeys: ["docs.reference.noteEditJSON", "docs.reference.noteEditSources", "docs.reference.noteImageStorage"],
    request: (model) => ({ model, prompt: "Change the chair to black", image: { url: "https://example.com/chair.png" }, n: 1, response_format: "url" }),
    response: { created: 1783860000, data: [{ url: "http://127.0.0.1:8000/v1/media/images/example" }] },
  },
  "video/generations": {
    key: "video/generations", category: "Video", title: "Video generations", method: "POST", path: "/videos/generations",
    descriptionKey: "docs.endpointVideoCreate", capabilities: ["video"],
    fields: [
      { name: "model", required: true, descriptionKey: "docs.reference.fieldModel" },
      { name: "prompt", descriptionKey: "docs.reference.fieldVideoPrompt" },
      { name: "duration", descriptionKey: "docs.reference.fieldDuration" },
      { name: "aspect_ratio", descriptionKey: "docs.reference.fieldAspectRatio" },
      { name: "resolution", descriptionKey: "docs.reference.fieldResolution" },
      { name: "image", descriptionKey: "docs.reference.fieldVideoImage" },
      { name: "reference_images", descriptionKey: "docs.reference.fieldReferenceImages" },
    ],
    noteKeys: ["docs.reference.noteVideoAsync", "docs.reference.noteVideoStrict"],
    request: (model) => ({ model, prompt: "A paper airplane flying over a city", duration: 8, aspect_ratio: "16:9", resolution: "720p" }),
    response: { request_id: "video_example" },
  },
  "video/get": {
    key: "video/get", category: "Video", title: "Get video", method: "GET", path: "/videos/{request_id}",
    descriptionKey: "docs.endpointVideoGet", capabilities: ["video"],
    fields: [{ name: "request_id", required: true, descriptionKey: "docs.reference.fieldRequestId" }],
    noteKeys: ["docs.reference.noteVideoPolling", "docs.reference.noteVideoStatus"],
    request: () => undefined,
    response: { status: "done", model: "grok-imagine-video", progress: 100, video: { url: "https://example.com/generated.mp4", duration: 8, respect_moderation: true } },
  },
};

export function ApiDocsPage() {
  const { t } = useTranslation();
  const { category, endpoint } = useParams();
  const definition = endpoints[`${category ?? ""}/${endpoint ?? ""}`];
  const [language, setLanguage] = useState<ExampleLanguage>("curl");
  const [exampleView, setExampleView] = useState<ExampleView>("request");
  const [selectedModel, setSelectedModel] = useState("");

  const systemQuery = useQuery({ queryKey: ["system-info"], queryFn: getSystemInfo, staleTime: Number.POSITIVE_INFINITY, retry: 1 });
  const modelsQuery = useQuery({ queryKey: ["docs", "available-models"], queryFn: () => listModels({ page: 1, pageSize: 100 }), staleTime: 30_000 });

  if (!definition) return <Navigate to="/docs/chat/completions" replace />;

  const publicApiBaseUrl = systemQuery.data?.publicApiBaseURL || runtimeConfig.publicApiBaseUrl;
  const baseUrl = `${publicApiBaseUrl.replace(/\/$/, "")}/v1`;
  const availableModels = (modelsQuery.data?.items ?? []).filter((model) => model.enabled && model.available && definition.capabilities.includes(model.capability));
  const selectedModelAvailable = availableModels.some((model) => model.publicId === selectedModel);
  const exampleModel = (selectedModelAvailable ? selectedModel : availableModels[0]?.publicId) || fallbackModel(definition.key);
  const examples = createExamples(definition, baseUrl, exampleModel);
  const responseExample = JSON.stringify(withExampleModel(definition.response, exampleModel), null, 2);

  return (
    <div className="w-full space-y-10">
      <header className="space-y-4">
        <div className="min-w-0">
          <h1 className="text-2xl font-medium text-foreground">{definition.title}</h1>
          <p className="mt-2 max-w-2xl text-sm leading-6 text-muted-foreground">{t(definition.descriptionKey)}</p>
        </div>
        <EndpointSignature method={definition.method} path={`/v1${definition.path}`} />
      </header>

      <div className="space-y-10">
        <DocsSection icon={<Link2 />} title={t("docs.reference.connection")}>
          <div className="grid gap-4 sm:grid-cols-2">
            <ConnectionItem label={t("docs.baseUrl")} value={baseUrl} />
            <ConnectionItem label={t("docs.authentication")} value={definition.key === "chat/messages" ? "x-api-key: g2a_..." : "Authorization: Bearer g2a_..."} />
            {definition.key === "chat/messages" ? <ConnectionItem label="anthropic-version" value="2023-06-01" /> : null}
          </div>
        </DocsSection>

        <DocsSection icon={<Braces />} title={definition.method === "GET" ? t("docs.reference.pathParameters") : t("docs.reference.requestBody")}>
          <div className="overflow-hidden rounded-md bg-card">
            <div className="hidden grid-cols-[minmax(120px,180px)_minmax(0,1fr)] gap-5 bg-secondary/35 px-4 py-2 text-xs text-muted-foreground sm:grid">
              <span>{t("docs.reference.parameter")}</span>
              <span>{t("docs.reference.description")}</span>
            </div>
            <div>{definition.fields.map((field, index) => <ParameterItem key={field.name} field={field} muted={index % 2 === 1} />)}</div>
          </div>
        </DocsSection>

        <DocsSection icon={<Code2 />} title={t("docs.reference.example")}>
          <ExamplePanel
            view={exampleView}
            onViewChange={setExampleView}
            language={language}
            onLanguageChange={setLanguage}
            code={exampleView === "request" ? examples[language] : responseExample}
            models={availableModels}
            selectedModel={exampleModel}
            onModelChange={setSelectedModel}
          />
        </DocsSection>

        {definition.noteKeys.length > 0 ? (
          <DocsSection icon={<Info />} title={t("docs.reference.notes")}>
            <ul className="space-y-2 rounded-md bg-secondary/35 px-4 py-3 text-xs leading-5 text-muted-foreground">
              {definition.noteKeys.map((key) => <li key={key} className="relative pl-3 before:absolute before:left-0 before:top-[0.55rem] before:size-1 before:rounded-full before:bg-muted-foreground/55">{t(key)}</li>)}
            </ul>
          </DocsSection>
        ) : null}
      </div>
    </div>
  );
}

function withExampleModel(response: Record<string, unknown>, model: string): Record<string, unknown> {
  return "model" in response ? { ...response, model } : response;
}

function fallbackModel(key: string): string {
  if (key.startsWith("image/")) return key === "image/edits" ? "grok-imagine-image-edit" : "grok-imagine-image";
  if (key.startsWith("video/")) return "grok-imagine-video";
  return "your-enabled-model";
}

function createExamples(definition: EndpointDefinition, baseUrl: string, model: string): Record<ExampleLanguage, string> {
  const request = definition.request(model);
  const url = `${baseUrl}${definition.path.replace("{request_id}", "video_example")}`;
  const messageHeaders = definition.key === "chat/messages";
  const curlHeaders = messageHeaders
    ? [
        '  -H "x-api-key: $GROK2API_API_KEY"',
        '  -H "anthropic-version: 2023-06-01"',
        '  -H "Content-Type: application/json"',
      ].join(" \\\n")
    : [
        '  -H "Authorization: Bearer $GROK2API_API_KEY"',
        '  -H "Content-Type: application/json"',
      ].join(" \\\n");
  const curlBody = request ? ` \\\n  -d '${JSON.stringify(request, null, 2)}'` : "";
  const headers = messageHeaders
    ? { "x-api-key": "g2a_your_api_key", "anthropic-version": "2023-06-01", "Content-Type": "application/json" }
    : { Authorization: "Bearer g2a_your_api_key", "Content-Type": "application/json" };
  const pythonImports = request ? "import json\nimport requests" : "import requests";
  const pythonPayload = request ? `\n\npayload = json.loads(r'''${JSON.stringify(request, null, 2)}''')` : "";
  const pythonBody = request ? ",\n    json=payload" : "";
  const javascriptBody = request ? `,\n  body: JSON.stringify(${JSON.stringify(request, null, 2)})` : "";
  return {
    curl: `export GROK2API_API_KEY="g2a_your_api_key"\n\ncurl -X ${definition.method} "${url}" \\\n${curlHeaders}${curlBody}`,
    python: `${pythonImports}${pythonPayload}\n\nresponse = requests.${definition.method.toLowerCase()}(\n    "${url}",\n    headers=${JSON.stringify(headers, null, 2)}${pythonBody}\n)\nresponse.raise_for_status()\nprint(response.json())`,
    javascript: `const response = await fetch("${url}", {\n  method: "${definition.method}",\n  headers: ${JSON.stringify(headers, null, 2)}${javascriptBody}\n});\n\nif (!response.ok) throw new Error(await response.text());\nconsole.log(await response.json());`,
  };
}

function MethodLabel({ method }: { method: Method }) {
  return <span className={cn("font-mono text-xs font-semibold", method === "GET" ? "text-emerald-600 dark:text-emerald-400" : "text-sky-600 dark:text-sky-400")}>{method}</span>;
}

function EndpointSignature({ method, path }: { method: Method; path: string }) {
  return (
    <div className="flex h-8 w-fit max-w-full items-center gap-2 rounded-md bg-card px-3">
      <MethodLabel method={method} />
      <code className="min-w-0 truncate text-xs" title={path}>{path}</code>
      <CopyButton value={path} />
    </div>
  );
}

function DocsSection({ icon, title, children }: { icon: ReactNode; title: string; children: ReactNode }) {
  return (
    <section className="space-y-3.5">
      <div className="flex items-center gap-2 text-sm font-medium [&_svg]:size-4 [&_svg]:text-muted-foreground">{icon}{title}</div>
      <div className="min-w-0">{children}</div>
    </section>
  );
}

function ConnectionItem({ label, value }: { label: string; value: string }) {
  return (
    <div className="min-w-0">
      <div className="mb-1.5 text-xs text-muted-foreground">{label}</div>
      <div className="flex h-8 min-w-0 items-center rounded-md bg-secondary/55 pl-3 pr-0.5">
        <code className="min-w-0 flex-1 truncate text-xs text-muted-foreground" title={value}>{value}</code>
        <CopyButton value={value} />
      </div>
    </div>
  );
}

function ParameterItem({ field, muted }: { field: FieldDefinition; muted: boolean }) {
  const { t } = useTranslation();
  return (
    <div className={cn("grid grid-cols-1 gap-1.5 px-4 py-3 sm:grid-cols-[minmax(120px,180px)_minmax(0,1fr)] sm:gap-5", muted && "bg-secondary/20")}>
      <div className="min-w-0">
        <code className="break-all text-xs font-medium text-foreground">
        {field.name}
        {field.required ? <span className="ml-1 text-destructive" title={t("docs.reference.required")}>*</span> : null}
        </code>
      </div>
      <div className="min-w-0 text-xs leading-5 text-muted-foreground">{t(field.descriptionKey)}</div>
    </div>
  );
}

function ExamplePanel({
  view,
  onViewChange,
  language,
  onLanguageChange,
  code,
  models,
  selectedModel,
  onModelChange,
}: {
  view: ExampleView;
  onViewChange: (view: ExampleView) => void;
  language: ExampleLanguage;
  onLanguageChange: (language: ExampleLanguage) => void;
  code: string;
  models: ModelRouteDTO[];
  selectedModel: string;
  onModelChange: (model: string) => void;
}) {
  const { t } = useTranslation();
  return (
    <div className="overflow-hidden rounded-lg bg-card">
      <div className="flex min-h-12 flex-wrap items-center gap-2 px-3 py-2">
        <Tabs value={view} onValueChange={(value) => onViewChange(value as ExampleView)}>
          <TabsList>
            <TabsTrigger value="request">{t("docs.reference.request")}</TabsTrigger>
            <TabsTrigger value="response">{t("docs.reference.response")}</TabsTrigger>
          </TabsList>
        </Tabs>

        <div className="flex min-w-0 flex-1 flex-wrap items-center justify-end gap-2">
          {view === "request" ? (
            <Select value={language} onValueChange={(value) => onLanguageChange(value as ExampleLanguage)}>
              <SelectTrigger className="h-8 w-28 bg-background text-xs" aria-label={t("docs.exampleLanguage")}><SelectValue /></SelectTrigger>
              <SelectContent>
                {exampleLanguages.map((item) => <SelectItem key={item} value={item}>{item === "javascript" ? "JavaScript" : item === "python" ? "Python" : "cURL"}</SelectItem>)}
              </SelectContent>
            </Select>
          ) : null}
          {models.length > 0 ? (
            <Select value={selectedModel} onValueChange={onModelChange}>
              <SelectTrigger className="h-8 w-[190px] max-w-full bg-background text-xs" aria-label={t("docs.reference.exampleModel")}><SelectValue /></SelectTrigger>
              <SelectContent>{models.map((model) => <SelectItem key={model.id} value={model.publicId}>{model.publicId}</SelectItem>)}</SelectContent>
            </Select>
          ) : null}
          <CopyButton value={code} />
        </div>
      </div>

      <pre className="max-h-[480px] overflow-auto bg-secondary/45 p-4 text-xs leading-5 text-foreground"><code>{code}</code></pre>
    </div>
  );
}

function CopyButton({ value, className }: { value: string; className?: string }) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);
  return (
    <Button type="button" variant="ghost" size="icon" className={cn("size-7 shrink-0 text-muted-foreground", className)} aria-label={copied ? t("common.copied") : t("common.copy")} title={copied ? t("common.copied") : t("common.copy")} onClick={() => { void navigator.clipboard.writeText(value); setCopied(true); window.setTimeout(() => setCopied(false), 1500); }}>
      {copied ? <Check /> : <Copy />}
    </Button>
  );
}
