package web

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/searchresult"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

const webResponseTTL = 30 * 24 * time.Hour

const maxDeferredSearchTextBytes = 8 << 20

const maxTrackedServerTools = 1024

var (
	errWebAntiBot    = errors.New("Grok Web anti-bot rejection")
	errWebUsageLimit = errors.New("Grok Web usage limit reached")
)

var (
	grokRenderPattern   = regexp.MustCompile(`(?s)<grok:render\s+card_id="([^"]+)"\s+card_type="([^"]+)"\s+type="([^"]+)"[^>]*>.*?</grok:render>`)
	grokToolNamePattern = regexp.MustCompile(`(?is)<xai:tool_name>\s*(.*?)\s*</xai:tool_name>`)
)

type openAIRequest struct {
	Model              string          `json:"model"`
	Stream             bool            `json:"stream"`
	Input              json.RawMessage `json:"input"`
	Instructions       string          `json:"instructions"`
	PreviousResponseID string          `json:"previous_response_id"`
	Messages           []chatMessage   `json:"messages"`
	Tools              json.RawMessage `json:"tools"`
	ToolChoice         json.RawMessage `json:"tool_choice"`
	ParallelToolCalls  *bool           `json:"parallel_tool_calls"`
	ImageConfig        *struct {
		Count          *int   `json:"n"`
		ResponseFormat string `json:"response_format"`
	} `json:"image_config"`
}

type chatMessage struct {
	Type       string          `json:"type"`
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  json.RawMessage `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	CallID     string          `json:"call_id"`
	Name       string          `json:"name"`
	Arguments  string          `json:"arguments"`
	Output     json.RawMessage `json:"output"`
}

type normalizedChatInput struct {
	Prompt string
	Images []string
}

type parsedChat struct {
	ResponseID     string
	ConversationID string
	ParentID       string
	Text           strings.Builder
	Reasoning      strings.Builder
	Images         []string
	SearchSources  []map[string]any
	Annotations    []map[string]any
	sourceKeys     map[string]struct{}
	serverToolKeys map[string]struct{}
	webSearchKeys  map[string]struct{}
	cardCache      map[string]map[string]any
	citationIndex  map[string]int
	lastCitation   int
	ServerTools    int64
	WebSearchTools int64
	InputTokens    int64
	ToolCalls      []parsedToolCall
	Tools          []any
	ToolChoice     any
	ParallelTools  bool
}

func (a *Adapter) ForwardResponse(ctx context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	if request.Method == http.MethodGet || request.Method == http.MethodDelete {
		return a.handleResponseResource(ctx, request)
	}
	if request.Path == "/responses/compact" {
		return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{
			"type": "invalid_request_error", "code": "unsupported_operation",
			"message": "Grok Web 模型不支持 /responses/compact",
		}}), nil
	}
	if request.Method != http.MethodPost {
		return jsonProviderResponse(http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"message": "method not allowed"}}), nil
	}
	var conversationOptions conversation.ResponseOptions
	if request.Operation == conversation.OperationMessages {
		converted, options, err := conversation.ConvertRequestWithOptions(request.Body, request.Model, request.Operation)
		if err != nil {
			return jsonProviderResponse(http.StatusBadRequest, map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": err.Error()}}), nil
		}
		request.Body = converted
		conversationOptions = options
	}

	var input openAIRequest
	if err := json.Unmarshal(request.Body, &input); err != nil {
		return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "请求 JSON 无效", "type": "invalid_request_error"}}), nil
	}
	normalized, err := normalizeOpenAIInput(input, request.Operation)
	if err != nil {
		return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error(), "type": "invalid_request_error"}}), nil
	}
	tools, err := parseToolConfiguration(input.Tools, input.ToolChoice)
	if err != nil {
		return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error(), "type": "invalid_request_error", "code": "invalid_tools"}}), nil
	}
	parallelTools := true
	if input.ParallelToolCalls != nil {
		parallelTools = *input.ParallelToolCalls
	}
	spec, ok := Resolve(request.Model)
	if ok && spec.ProtocolModel == "imagine-lite" && request.Operation == "chat" {
		if len(tools.ResponseTools) > 0 {
			return invalidImageRequest("grok-imagine-image 不支持 tools")
		}
		return a.forwardLiteChatCompletion(ctx, request, input, normalized, spec)
	}
	if !ok || spec.Capability != "chat" {
		return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "模型不支持文本对话", "type": "invalid_request_error"}}), nil
	}

	normalized.Prompt = injectToolPrompt(normalized.Prompt, tools)
	responseID := newWebID("resp")
	streaming := input.Stream || request.Streaming
	var parsed parsedChat
	var previous *inferencedomain.WebResponseState
	for attempt := 0; attempt < 2; attempt++ {
		upstream, lease, currentPrevious, statsigTarget, openErr := a.openChat(ctx, request.Credential, input.PreviousResponseID, spec, normalized)
		if openErr != nil {
			if errors.Is(openErr, errInvalidChatImage) {
				return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{
					"message": openErr.Error(), "type": "invalid_request_error", "code": "invalid_image_input",
				}}), nil
			}
			return nil, openErr
		}
		previous = currentPrevious
		if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
			if upstream.StatusCode == http.StatusForbidden {
				if attempt == 0 && a.invalidateSignedStatsig(http.MethodPost, statsigTarget) {
					a.releaseStatsigRetry(upstream, lease)
					continue
				}
			}
			return &provider.Response{
				StatusCode: upstream.StatusCode, Status: upstream.Status, Header: http.Header(upstream.Header),
				UpstreamURL: responseUpstreamURL(upstream),
				Body: &releaseBody{ReadCloser: upstream.Body, release: func() {
					a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, upstream.StatusCode, nil)
					lease.Release()
				}},
			}, nil
		}

		if streaming {
			prepared, preflightErr := preflightUpstream(upstream.Body)
			if preflightErr == nil {
				body := a.streamOpenAIResponse(ctx, prepared, lease, request.Credential, responseID, input.Model, request.Operation, normalized.Prompt, previous, tools, parallelTools, conversationOptions)
				return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: streamHeaders(), Body: body}, nil
			}
			if errors.Is(preflightErr, errWebAntiBot) && attempt == 0 && a.invalidateSignedStatsig(http.MethodPost, statsigTarget) {
				a.releaseStatsigRetry(upstream, lease)
				continue
			}
			_ = upstream.Body.Close()
			lease.Release()
			if errors.Is(preflightErr, errWebAntiBot) {
				a.feedbackAntiBot(ctx, lease, statsigTarget)
				return antiBotProviderResponse(), nil
			}
			return nil, preflightErr
		}

		currentParsed, consumeErr := consumeUpstream(upstream.Body, nil)
		_ = upstream.Body.Close()
		if errors.Is(consumeErr, errWebAntiBot) && attempt == 0 && a.invalidateSignedStatsig(http.MethodPost, statsigTarget) {
			lease.Release()
			continue
		}
		lease.Release()
		if consumeErr != nil {
			if errors.Is(consumeErr, errWebAntiBot) {
				a.feedbackAntiBot(ctx, lease, statsigTarget)
				return antiBotProviderResponse(), nil
			}
			a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, consumeErr)
			return nil, consumeErr
		}
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, http.StatusOK, nil)
		parsed = currentParsed
		break
	}
	parsed.InputTokens = estimateTokens(normalized.Prompt)
	parsed.Tools = tools.ResponseTools
	parsed.ToolChoice = tools.ResponseChoice
	parsed.ParallelTools = parallelTools
	applyParsedToolCalls(&parsed, tools)
	if err := a.archiveChatImages(ctx, request.Credential, &parsed); err != nil {
		return nil, err
	}
	payload := buildOpenAIResult(request.Operation, responseID, input.Model, parsed, false, conversationOptions)
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if request.Operation == conversation.OperationResponses {
		a.saveResponseState(context.WithoutCancel(ctx), request.Credential.ID, responseID, parsed, data)
	}
	return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: jsonHeaders(), Body: io.NopCloser(bytes.NewReader(data))}, nil
}

func (a *Adapter) releaseStatsigRetry(upstream *http.Response, lease *infraegress.Lease) {
	_ = upstream.Body.Close()
	lease.Release()
}

func (a *Adapter) feedbackAntiBot(ctx context.Context, lease *infraegress.Lease, statsigTarget string) {
	a.invalidateSignedStatsig(http.MethodPost, statsigTarget)
	a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, http.StatusForbidden, nil)
}

func preflightUpstream(source io.ReadCloser) (io.ReadCloser, error) {
	reader := bufio.NewReaderSize(source, 64<<10)
	var prefetched bytes.Buffer
	for prefetched.Len() <= 1<<20 {
		line, err := reader.ReadString('\n')
		if line != "" {
			prefetched.WriteString(line)
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "data:") {
				trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			}
			if strings.HasPrefix(trimmed, "{") {
				var root map[string]any
				if json.Unmarshal([]byte(trimmed), &root) == nil {
					if errorValue, ok := root["error"].(map[string]any); ok {
						return nil, webResponseError(errorValue)
					}
					if result, ok := root["result"].(map[string]any); ok && (result["conversation"] != nil || result["response"] != nil) {
						return &readerCloser{Reader: io.MultiReader(bytes.NewReader(prefetched.Bytes()), reader), closer: source}, nil
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && prefetched.Len() > 0 {
				return &readerCloser{Reader: bytes.NewReader(prefetched.Bytes()), closer: source}, nil
			}
			return nil, err
		}
	}
	return nil, fmt.Errorf("Grok Web 首个流事件超过安全检查上限")
}

func (a *Adapter) openChat(ctx context.Context, credential account.Credential, previousResponseID string, spec ModelSpec, input normalizedChatInput) (*http.Response, *infraegress.Lease, *inferencedomain.WebResponseState, string, error) {
	cfg := a.config()
	token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return nil, nil, nil, "", err
	}
	lease, err := a.egress.Acquire(ctx, domainegress.ScopeWeb, fmt.Sprintf("%d", credential.ID))
	if err != nil {
		return nil, nil, nil, "", err
	}
	mode := spec.Mode
	endpoint := cfg.BaseURL + "/rest/app-chat/conversations/new"
	var previous *inferencedomain.WebResponseState
	if previousResponseID != "" {
		state, stateErr := a.states.GetWebState(ctx, previousResponseID, time.Now().UTC())
		if stateErr != nil {
			lease.Release()
			if errors.Is(stateErr, repository.ErrNotFound) {
				return nil, nil, nil, "", fmt.Errorf("previous_response_id 不存在或已过期")
			}
			return nil, nil, nil, "", stateErr
		}
		if state.AccountID != credential.ID {
			lease.Release()
			return nil, nil, nil, "", fmt.Errorf("previous_response_id 绑定的账号不一致")
		}
		previous = &state
		endpoint = cfg.BaseURL + "/rest/app-chat/conversations/" + url.PathEscape(state.ConversationID) + "/responses"
	}
	attachments, err := a.prepareChatAttachments(ctx, cfg, lease, token, input.Images)
	if err != nil {
		lease.Release()
		return nil, nil, nil, "", err
	}
	payload := buildWebChatPayload(input.Prompt, mode, attachments)
	if previous != nil {
		payload["responseId"] = previous.UpstreamParentResponseID
	}
	data, _ := json.Marshal(payload)
	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.ChatTimeoutSeconds)*time.Second)
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		cancel()
		lease.Release()
		return nil, nil, nil, "", err
	}
	request.Header = buildHeaders(token, lease, "application/json")
	applyAppHeaders(request.Header, cfg.BaseURL, cfg.BaseURL+"/")
	a.applySignedStatsig(requestCtx, request, token, lease)
	response, err := lease.Do(request)
	if err != nil {
		cancel()
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, err)
		lease.Release()
		return nil, nil, nil, "", err
	}
	response.Body = &cancelBody{ReadCloser: response.Body, cancel: cancel}
	return response, lease, previous, endpoint, nil
}

func (a *Adapter) handleResponseResource(ctx context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	id := strings.TrimPrefix(request.Path, "/responses/")
	if before, _, ok := strings.Cut(id, "?"); ok {
		id = before
	}
	id, _ = url.PathUnescape(id)
	if request.Method == http.MethodDelete {
		if err := a.states.DeleteWebState(ctx, id); err != nil {
			return jsonProviderResponse(http.StatusNotFound, map[string]any{"error": map[string]any{"message": "Response 不存在", "type": "invalid_request_error"}}), nil
		}
		return jsonProviderResponse(http.StatusOK, map[string]any{"id": id, "object": "response.deleted", "deleted": true}), nil
	}
	state, err := a.states.GetWebState(ctx, id, time.Now().UTC())
	if err != nil {
		return jsonProviderResponse(http.StatusNotFound, map[string]any{"error": map[string]any{"message": "Response 不存在或已过期", "type": "invalid_request_error"}}), nil
	}
	return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: jsonHeaders(), Body: io.NopCloser(strings.NewReader(state.ResponseJSON))}, nil
}

func (a *Adapter) streamOpenAIResponse(ctx context.Context, source io.ReadCloser, lease *infraegress.Lease, credential account.Credential, responseID, model, operation, prompt string, previous *inferencedomain.WebResponseState, tools toolConfiguration, parallelTools bool, options conversation.ResponseOptions) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		defer source.Close()
		defer lease.Release()
		parsed := &parsedChat{
			ResponseID: responseID, InputTokens: estimateTokens(prompt), Tools: tools.ResponseTools,
			ToolChoice: tools.ResponseChoice, ParallelTools: parallelTools,
		}
		if previous != nil {
			parsed.ConversationID = previous.ConversationID
		}
		var clientText strings.Builder
		archivedImages := make(map[string]struct{})
		var sieve *toolStreamSieve
		if len(tools.Functions) > 0 && tools.Choice != "none" {
			sieve = newToolStreamSieve(tools.available)
		}
		messagesStream := newWebMessagesStream(writer, responseID, model, parsed.InputTokens, options)
		if operation != conversation.OperationMessages {
			writeStreamStart(writer, operation, responseID, model, parsed.InputTokens)
		}
		err := consumeUpstreamInto(source, parsed, func(kind, delta string) error {
			if len(parsed.ToolCalls) > 0 && kind != "reasoning" {
				return nil
			}
			if kind == "image" {
				rawURL := delta
				item, imageErr := a.imageDataItem(ctx, credential, imagineImageValue{URL: delta}, "url")
				if imageErr != nil {
					return imageErr
				}
				delta = liteImageMarkdown(item)
				if parsed.Text.Len() > 0 {
					delta = "\n\n" + delta
				}
				parsed.Text.WriteString(delta)
				archivedImages[rawURL] = struct{}{}
				kind = "text"
			}
			if kind == "text" && sieve != nil {
				result := sieve.Feed(delta)
				if result.SafeText != "" {
					clientText.WriteString(result.SafeText)
					if err := writeWebStreamDelta(writer, messagesStream, operation, responseID, model, kind, result.SafeText); err != nil {
						return err
					}
				}
				if result.Complete {
					if len(result.Calls) == 0 {
						clientText.WriteString(result.Raw)
						return writeWebStreamDelta(writer, messagesStream, operation, responseID, model, kind, result.Raw)
					}
					parsed.ToolCalls = result.Calls
					return writeWebStreamToolCalls(writer, messagesStream, operation, responseID, model, result.Calls)
				}
				return nil
			}
			if kind == "text" {
				clientText.WriteString(delta)
			}
			return writeWebStreamDelta(writer, messagesStream, operation, responseID, model, kind, delta)
		})
		if err != nil {
			a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, err)
			_ = writer.CloseWithError(err)
			return
		}
		if sieve != nil && len(parsed.ToolCalls) == 0 {
			result := sieve.Flush()
			if result.SafeText != "" {
				clientText.WriteString(result.SafeText)
				if err := writeWebStreamDelta(writer, messagesStream, operation, responseID, model, "text", result.SafeText); err != nil {
					_ = writer.CloseWithError(err)
					return
				}
			}
			if len(result.Calls) > 0 {
				parsed.ToolCalls = result.Calls
				if err := writeWebStreamToolCalls(writer, messagesStream, operation, responseID, model, result.Calls); err != nil {
					_ = writer.CloseWithError(err)
					return
				}
			}
		}
		if len(parsed.ToolCalls) == 0 {
			for _, rawURL := range parsed.Images {
				if _, exists := archivedImages[rawURL]; exists {
					continue
				}
				item, imageErr := a.imageDataItem(ctx, credential, imagineImageValue{URL: rawURL}, "url")
				if imageErr != nil {
					_ = writer.CloseWithError(imageErr)
					return
				}
				delta := liteImageMarkdown(item)
				if clientText.Len() > 0 {
					delta = "\n\n" + delta
				}
				clientText.WriteString(delta)
				if err := writeWebStreamDelta(writer, messagesStream, operation, responseID, model, "text", delta); err != nil {
					_ = writer.CloseWithError(err)
					return
				}
			}
		}
		parsed.Text.Reset()
		parsed.Text.WriteString(clientText.String())
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, http.StatusOK, nil)
		payload := buildOpenAIResult(operation, responseID, model, *parsed, false, options)
		data, _ := json.Marshal(payload)
		if operation == conversation.OperationResponses {
			a.saveResponseState(context.WithoutCancel(ctx), credential.ID, responseID, *parsed, data)
		}
		if operation == conversation.OperationMessages {
			if finishErr := messagesStream.Finish(*parsed, payload); finishErr != nil {
				_ = writer.CloseWithError(finishErr)
				return
			}
		} else {
			writeStreamDone(writer, operation, responseID, model, *parsed, payload)
		}
		_ = writer.Close()
	}()
	return reader
}

func (a *Adapter) saveResponseState(ctx context.Context, accountID uint64, responseID string, parsed parsedChat, data []byte) {
	if parsed.ConversationID == "" || parsed.ParentID == "" || a.states == nil {
		return
	}
	now := time.Now().UTC()
	_ = a.states.SaveWebState(ctx, inferencedomain.WebResponseState{
		ResponseID: responseID, AccountID: accountID, ConversationID: parsed.ConversationID,
		UpstreamParentResponseID: parsed.ParentID, ResponseJSON: string(data), Status: "completed",
		ExpiresAt: now.Add(webResponseTTL), CreatedAt: now, UpdatedAt: now,
	})
}

func normalizeOpenAIInput(input openAIRequest, operation string) (normalizedChatInput, error) {
	var messages []chatMessage
	if operation == "chat" {
		messages = input.Messages
	} else {
		if strings.TrimSpace(input.Instructions) != "" {
			content, _ := json.Marshal(input.Instructions)
			messages = append(messages, chatMessage{Role: "system", Content: content})
		}
		trimmed := bytes.TrimSpace(input.Input)
		if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
			return normalizedChatInput{}, errors.New("input 不能为空")
		}
		if trimmed[0] == '"' {
			var text string
			if json.Unmarshal(trimmed, &text) != nil {
				return normalizedChatInput{}, errors.New("input 格式无效")
			}
			content, _ := json.Marshal(text)
			messages = append(messages, chatMessage{Role: "user", Content: content})
		} else if err := json.Unmarshal(trimmed, &messages); err != nil {
			return normalizedChatInput{}, errors.New("input 必须是字符串或消息数组")
		}
	}
	if len(messages) == 0 {
		return normalizedChatInput{}, errors.New("messages 不能为空")
	}
	var builder strings.Builder
	images := make([]string, 0, 2)
	for _, message := range messages {
		typeName := strings.ToLower(strings.TrimSpace(message.Type))
		if typeName == "function_call" {
			if !toolNamePattern.MatchString(strings.TrimSpace(message.Name)) {
				return normalizedChatInput{}, errors.New("function_call.name 无效")
			}
			arguments := normalizeToolArguments(message.Arguments)
			if !json.Valid([]byte(arguments)) {
				return normalizedChatInput{}, errors.New("function_call.arguments 必须是有效 JSON")
			}
			builder.WriteString("[assistant]\n<tool_calls>\n  <tool_call>\n    <tool_name>")
			builder.WriteString(message.Name)
			builder.WriteString("</tool_name>\n    <parameters>")
			builder.WriteString(arguments)
			builder.WriteString("</parameters>\n  </tool_call>\n</tool_calls>\n\n")
			continue
		}
		if typeName == "function_call_output" {
			text, err := rawTextValue(message.Output)
			if err != nil {
				return normalizedChatInput{}, errors.New("function_call_output.output 必须是字符串或 JSON")
			}
			builder.WriteString("[tool result for ")
			builder.WriteString(strings.TrimSpace(message.CallID))
			builder.WriteString("]\n")
			builder.WriteString(text)
			builder.WriteString("\n\n")
			continue
		}
		text, messageImages, err := contentTextAndImages(message.Content)
		if err != nil {
			return normalizedChatInput{}, err
		}
		images = append(images, messageImages...)
		if len(message.ToolCalls) > 0 {
			xml := toolCallsToXML(message.ToolCalls)
			if text != "" && xml != "" {
				text += "\n" + xml
			} else if xml != "" {
				text = xml
			}
		}
		if message.ToolCallID != "" {
			text = "Tool result (" + message.ToolCallID + "): " + text
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		builder.WriteString("[")
		builder.WriteString(strings.ToLower(strings.TrimSpace(message.Role)))
		builder.WriteString("]\n")
		builder.WriteString(text)
		builder.WriteString("\n\n")
	}
	value := strings.TrimSpace(builder.String())
	if value == "" && len(images) == 0 {
		return normalizedChatInput{}, errors.New("消息中没有可发送的文本或图片")
	}
	if len(images) > maxChatImageAttachments {
		return normalizedChatInput{}, fmt.Errorf("单次对话最多支持 %d 张图片", maxChatImageAttachments)
	}
	return normalizedChatInput{Prompt: value, Images: images}, nil
}

func rawTextValue(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil
	}
	var text string
	if json.Unmarshal(trimmed, &text) == nil {
		return text, nil
	}
	if !json.Valid(trimmed) {
		return "", errors.New("invalid JSON")
	}
	return string(trimmed), nil
}

func contentTextAndImages(raw json.RawMessage) (string, []string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil, nil
	}
	if trimmed[0] == '"' {
		var value string
		if json.Unmarshal(trimmed, &value) != nil {
			return "", nil, errors.New("消息 content 字符串无效")
		}
		return value, nil, nil
	}
	var parts []map[string]any
	if json.Unmarshal(trimmed, &parts) != nil {
		return "", nil, errors.New("消息 content 必须是字符串或内容数组")
	}
	values := make([]string, 0, len(parts))
	images := make([]string, 0, 2)
	for _, part := range parts {
		typeName, _ := part["type"].(string)
		switch typeName {
		case "text", "input_text", "output_text":
			if text, _ := part["text"].(string); text != "" {
				values = append(values, text)
			}
		case "image_url", "input_image", "image":
			if value := extractImageURL(part); value != "" {
				images = append(images, value)
			} else if fileID, _ := part["file_id"].(string); fileID != "" {
				return "", nil, errors.New("Grok Web 对话暂不支持 input_image.file_id，请使用 image_url 或 Base64 data URI")
			} else {
				return "", nil, errors.New("图片内容缺少 image_url")
			}
		case "input_audio", "file", "input_file":
			return "", nil, fmt.Errorf("Grok Web 对话暂不支持 %s 内容", typeName)
		default:
			return "", nil, fmt.Errorf("Grok Web 对话暂不支持 content.type=%q", typeName)
		}
	}
	return strings.Join(values, "\n"), images, nil
}

func extractImageURL(part map[string]any) string {
	value := part["image_url"]
	if text, ok := value.(string); ok {
		return text
	}
	if object, ok := value.(map[string]any); ok {
		text, _ := object["url"].(string)
		return text
	}
	return ""
}

func buildWebChatPayload(message, mode string, attachments []string) map[string]any {
	if attachments == nil {
		attachments = []string{}
	}
	return map[string]any{
		"collectionIds": []any{}, "disabledConnectorIds": []any{},
		"deviceEnvInfo": map[string]any{"darkModeEnabled": false, "devicePixelRatio": 2, "screenHeight": 1328, "screenWidth": 2056, "viewportHeight": 1083, "viewportWidth": 2056},
		"disableMemory": true, "disableSearch": false, "disableSelfHarmShortCircuit": false,
		"disableTextFollowUps": false, "enableImageGeneration": true, "enableImageStreaming": true,
		"enableSideBySide": true, "fileAttachments": attachments, "forceConcise": false,
		"forceSideBySide": false, "imageAttachments": []any{}, "imageGenerationCount": 2,
		"isAsyncChat": false, "message": message, "modeId": mode, "responseMetadata": map[string]any{},
		"returnImageBytes": false, "returnRawGrokInXaiRequest": false,
		"sendFinalMetadata": true, "temporary": true,
	}
}

func consumeUpstream(source io.Reader, emit func(string, string) error) (parsedChat, error) {
	parsed := parsedChat{}
	err := consumeUpstreamInto(source, &parsed, emit)
	return parsed, err
}

func consumeUpstreamInto(source io.Reader, parsed *parsedChat, emit func(string, string) error) error {
	return consumeJSONObjects(source, 8<<20, func(data []byte) error {
		kind, delta, err := parseUpstreamFrame(data, parsed)
		if err != nil {
			return err
		}
		if delta != "" && emit != nil {
			return emit(kind, delta)
		}
		return nil
	})
}

func consumeJSONObjects(source io.Reader, maxObjectBytes int, consume func([]byte) error) error {
	reader := bufio.NewReaderSize(source, 64<<10)
	frame := make([]byte, 0, 64<<10)
	depth := 0
	inString := false
	escaped := false
	for {
		value, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if depth != 0 {
					return io.ErrUnexpectedEOF
				}
				return nil
			}
			return err
		}
		if depth == 0 {
			if value != '{' {
				continue
			}
			frame = frame[:0]
			depth = 1
			inString = false
			escaped = false
			frame = append(frame, value)
			continue
		}
		frame = append(frame, value)
		if len(frame) > maxObjectBytes {
			return fmt.Errorf("Grok Web 单个响应帧超过 %d MiB", maxObjectBytes>>20)
		}
		if inString {
			if escaped {
				escaped = false
			} else if value == '\\' {
				escaped = true
			} else if value == '"' {
				inString = false
			}
			continue
		}
		switch value {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				if err := consume(frame); err != nil {
					return err
				}
			}
		}
	}
}

func parseUpstreamFrame(data []byte, parsed *parsedChat) (string, string, error) {
	var root map[string]any
	if json.Unmarshal(data, &root) != nil {
		return "", "", nil
	}
	if errorValue, ok := root["error"].(map[string]any); ok {
		return "", "", webResponseError(errorValue)
	}
	result, _ := root["result"].(map[string]any)
	if conversation, _ := result["conversation"].(map[string]any); conversation != nil {
		parsed.ConversationID, _ = conversation["conversationId"].(string)
		return "", "", nil
	}
	response, _ := result["response"].(map[string]any)
	if response == nil {
		return "", "", nil
	}
	if errorValue, ok := response["error"].(map[string]any); ok {
		return "", "", webResponseError(errorValue)
	}
	for _, key := range []string{"cardAttachment", "cardAttachments"} {
		if rawURL := collectCardAttachment(parsed, response[key]); rawURL != "" {
			rawURL = absoluteAssetURL(rawURL)
			parsed.Images = appendUniqueString(parsed.Images, rawURL)
			return "image", rawURL, nil
		}
	}
	if userResponse, _ := response["userResponse"].(map[string]any); userResponse != nil {
		if id, _ := userResponse["responseId"].(string); id != "" {
			parsed.ParentID = id
		}
	}
	collectSearchSources(parsed, response)
	token, _ := response["token"].(string)
	thinking, _ := response["isThinking"].(bool)
	tag, _ := response["messageTag"].(string)
	if tag == "tool_usage_card" {
		collectServerTool(parsed, response)
	}
	if token != "" && thinking {
		parsed.Reasoning.WriteString(token)
		return "reasoning", token, nil
	}
	if token != "" && !thinking && (tag == "final" || tag == "") {
		cleaned := cleanChatToken(parsed, token)
		parsed.Text.WriteString(cleaned)
		return "text", cleaned, nil
	}
	if modelResponse, _ := response["modelResponse"].(map[string]any); modelResponse != nil {
		if first := collectModelResponseImages(parsed, modelResponse); first != "" {
			return "image", first, nil
		}
	}
	if imageResponse, _ := response["streamingImageGenerationResponse"].(map[string]any); imageResponse != nil {
		rawURL, _ := imageResponse["imageUrl"].(string)
		if rawURL == "" {
			rawURL, _ = imageResponse["url"].(string)
		}
		if rawURL != "" {
			completed, _ := imageResponse["isFinal"].(bool)
			if completed || imageResponse["progress"] == float64(100) {
				rawURL = absoluteAssetURL(rawURL)
				parsed.Images = appendUniqueString(parsed.Images, rawURL)
				return "image", rawURL, nil
			}
		}
	}
	return "", "", nil
}

func webResponseError(value map[string]any) error {
	message, _ := value["message"].(string)
	if message == "" {
		message = "Grok Web stream error"
	}
	code, _ := numberAsInt(value["code"])
	if code == 7 || strings.Contains(strings.ToLower(message), "anti-bot") {
		return fmt.Errorf("%w: %s", errWebAntiBot, message)
	}
	normalized := strings.ToLower(message)
	if strings.Contains(normalized, "usage limit") || strings.Contains(normalized, "usage quota") {
		return fmt.Errorf("%w: %s", errWebUsageLimit, message)
	}
	return errors.New(message)
}

func antiBotProviderResponse() *provider.Response {
	return jsonProviderResponse(http.StatusForbidden, map[string]any{"error": map[string]any{
		"message": "Grok Web 出口会话被上游反机器人规则拒绝，请检查代理、User-Agent 与 Cloudflare Cookie 是否来自同一浏览器会话",
		"type":    "upstream_error", "code": "anti_bot_rejected",
	}})
}

func collectModelResponseImages(parsed *parsedChat, modelResponse map[string]any) string {
	first := ""
	appendImage := func(value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		value = absoluteAssetURL(value)
		if containsString(parsed.Images, value) {
			return
		}
		parsed.Images = append(parsed.Images, value)
		if first == "" {
			first = value
		}
	}
	if urls, ok := modelResponse["generatedImageUrls"].([]any); ok {
		for _, raw := range urls {
			value, _ := raw.(string)
			appendImage(value)
		}
	}
	if cards, ok := modelResponse["cardAttachmentsJson"].([]any); ok {
		for _, raw := range cards {
			encoded, _ := raw.(string)
			var card map[string]any
			if encoded == "" || json.Unmarshal([]byte(encoded), &card) != nil {
				continue
			}
			appendImage(imageURLFromCardData(card))
		}
	}
	return first
}

func collectSearchSources(parsed *parsedChat, response map[string]any) {
	if parsed.sourceKeys == nil {
		parsed.sourceKeys = make(map[string]struct{})
	}
	if search, _ := response["webSearchResults"].(map[string]any); search != nil {
		if values, ok := search["results"].([]any); ok {
			for _, raw := range values {
				item, _ := raw.(map[string]any)
				value, _ := item["url"].(string)
				if value == "" {
					continue
				}
				title, _ := item["title"].(string)
				appendSearchSource(parsed, value, title, "web")
			}
		}
	}
	if search, _ := response["xSearchResults"].(map[string]any); search != nil {
		if values, ok := search["results"].([]any); ok {
			for _, raw := range values {
				item, _ := raw.(map[string]any)
				username, _ := item["username"].(string)
				postID, _ := item["postId"].(string)
				if username == "" || postID == "" {
					continue
				}
				title, _ := item["text"].(string)
				value := "https://x.com/" + url.PathEscape(username) + "/status/" + url.PathEscape(postID)
				appendSearchSource(parsed, value, title, "x_post")
			}
		}
	}
}

func appendSearchSource(parsed *parsedChat, value, title, sourceType string) {
	value, valid := searchresult.NormalizeURL(value)
	if !valid {
		return
	}
	if parsed.sourceKeys == nil {
		parsed.sourceKeys = make(map[string]struct{})
	}
	if _, exists := parsed.sourceKeys[value]; exists {
		return
	}
	if len(parsed.SearchSources) >= searchresult.MaxResults {
		return
	}
	parsed.sourceKeys[value] = struct{}{}
	title = searchresult.NormalizeTitle(title, value)
	parsed.SearchSources = append(parsed.SearchSources, map[string]any{"url": value, "title": title, "type": sourceType})
}

func collectServerTool(parsed *parsedChat, response map[string]any) {
	if parsed.serverToolKeys == nil {
		parsed.serverToolKeys = make(map[string]struct{})
	}
	key := serverToolKey(response)
	if _, exists := parsed.serverToolKeys[key]; !exists {
		if len(parsed.serverToolKeys) >= maxTrackedServerTools {
			return
		}
		parsed.serverToolKeys[key] = struct{}{}
		parsed.ServerTools++
	}
	if webServerToolName(response) != "web_search" {
		return
	}
	if parsed.webSearchKeys == nil {
		parsed.webSearchKeys = make(map[string]struct{})
	}
	if _, exists := parsed.webSearchKeys[key]; exists || len(parsed.webSearchKeys) >= maxTrackedServerTools {
		return
	}
	parsed.webSearchKeys[key] = struct{}{}
	parsed.WebSearchTools++
}

func serverToolKey(response map[string]any) string {
	key := firstString(response, "rolloutId", "responseId", "toolUsageCardId")
	step, hasStep := numberAsInt(response["messageStepId"])
	if key != "" {
		if hasStep {
			key += fmt.Sprintf(":%d", step)
		}
		return key
	}
	if token, _ := response["token"].(string); token != "" {
		sum := sha256.Sum256([]byte(token))
		return "token:" + hex.EncodeToString(sum[:8])
	}
	if hasStep {
		return fmt.Sprintf("step:%d", step)
	}
	return firstString(response, "messageTag")
}

func webServerToolName(response map[string]any) string {
	if name := strings.ToLower(strings.TrimSpace(firstString(response, "toolName", "tool_name"))); name != "" {
		return name
	}
	token, _ := response["token"].(string)
	match := grokToolNamePattern.FindStringSubmatch(token)
	if len(match) < 2 {
		return ""
	}
	name := strings.TrimSpace(match[1])
	name = strings.TrimPrefix(name, "<![CDATA[")
	name = strings.TrimSuffix(name, "]]>")
	return strings.ToLower(strings.TrimSpace(name))
}

func applyParsedToolCalls(parsed *parsedChat, configuration toolConfiguration) {
	if len(configuration.Functions) == 0 || configuration.Choice == "none" {
		return
	}
	result := parseToolCalls(parsed.Text.String(), configuration.available)
	if len(result.Calls) == 0 {
		return
	}
	cleaned := removeToolSyntax(parsed.Text.String(), result)
	parsed.Text.Reset()
	parsed.Text.WriteString(cleaned)
	parsed.ToolCalls = result.Calls
}

func (a *Adapter) archiveChatImages(ctx context.Context, credential account.Credential, parsed *parsedChat) error {
	for _, rawURL := range parsed.Images {
		item, err := a.imageDataItem(ctx, credential, imagineImageValue{URL: rawURL}, "url")
		if err != nil {
			return err
		}
		if parsed.Text.Len() > 0 {
			parsed.Text.WriteString("\n\n")
		}
		parsed.Text.WriteString(liteImageMarkdown(item))
	}
	return nil
}

func appendUniqueString(values []string, value string) []string {
	if containsString(values, value) {
		return values
	}
	return append(values, value)
}

func containsString(values []string, value string) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}

func collectCardAttachment(parsed *parsedChat, value any) string {
	if values, ok := value.([]any); ok {
		first := ""
		for _, item := range values {
			if rawURL := collectCardAttachment(parsed, item); first == "" && rawURL != "" {
				first = rawURL
			}
		}
		return first
	}
	data := cardAttachmentData(value)
	if data == nil {
		return ""
	}
	if id, _ := data["id"].(string); id != "" {
		if parsed.cardCache == nil {
			parsed.cardCache = make(map[string]map[string]any)
		}
		parsed.cardCache[id] = data
	}
	return imageURLFromCardData(data)
}

func cardAttachmentData(value any) map[string]any {
	card, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	if raw, ok := card["jsonData"].(map[string]any); ok {
		return raw
	}
	if raw, _ := card["jsonData"].(string); raw != "" {
		var data map[string]any
		if json.Unmarshal([]byte(raw), &data) == nil {
			return data
		}
	}
	if card["image_chunk"] != nil || card["imageChunk"] != nil {
		return card
	}
	return nil
}

func imageURLFromCardData(data map[string]any) string {
	chunk, _ := data["image_chunk"].(map[string]any)
	if chunk == nil {
		chunk, _ = data["imageChunk"].(map[string]any)
	}
	if chunk == nil {
		return ""
	}
	moderated, _ := chunk["moderated"].(bool)
	progress, _ := numberAsInt(chunk["progress"])
	if moderated || progress < 100 {
		return ""
	}
	imageURL, _ := chunk["imageUrl"].(string)
	if imageURL == "" {
		imageURL, _ = chunk["image_url"].(string)
	}
	return imageURL
}

func cleanChatToken(parsed *parsedChat, token string) string {
	if !strings.Contains(token, "<grok:render") {
		return token
	}
	matches := grokRenderPattern.FindAllStringSubmatchIndex(token, -1)
	if len(matches) == 0 {
		return token
	}
	var builder strings.Builder
	cursor := 0
	for _, match := range matches {
		builder.WriteString(token[cursor:match[0]])
		cardID := token[match[2]:match[3]]
		renderType := token[match[6]:match[7]]
		replacement, annotation := renderChatCard(parsed, cardID, renderType)
		if annotation != nil {
			start := parsed.Text.Len() + builder.Len()
			annotation["start_index"] = start
			annotation["end_index"] = start + len(replacement)
			parsed.Annotations = append(parsed.Annotations, annotation)
		}
		builder.WriteString(replacement)
		cursor = match[1]
	}
	builder.WriteString(token[cursor:])
	return builder.String()
}

func renderChatCard(parsed *parsedChat, cardID, renderType string) (string, map[string]any) {
	if parsed.cardCache == nil {
		return "", nil
	}
	card := parsed.cardCache[cardID]
	if card == nil {
		return "", nil
	}
	switch renderType {
	case "render_generated_image", "render_file":
		return "", nil
	case "render_searched_image":
		image, _ := card["image"].(map[string]any)
		if image == nil {
			return "", nil
		}
		title, _ := image["title"].(string)
		thumbnail := firstString(image, "thumbnail", "original")
		link, _ := image["link"].(string)
		if thumbnail == "" {
			return "", nil
		}
		if title == "" {
			title = "image"
		}
		if link != "" {
			return fmt.Sprintf("[![%s](%s)](%s)", title, thumbnail, link), nil
		}
		return fmt.Sprintf("![%s](%s)", title, thumbnail), nil
	case "render_inline_citation":
		value, _ := card["url"].(string)
		value, valid := searchresult.NormalizeURL(value)
		if !valid {
			return "", nil
		}
		if parsed.citationIndex == nil {
			parsed.citationIndex = make(map[string]int)
		}
		index, exists := parsed.citationIndex[value]
		if !exists {
			index = len(parsed.citationIndex) + 1
			parsed.citationIndex[value] = index
		}
		if parsed.lastCitation == index {
			return "", nil
		}
		parsed.lastCitation = index
		citation := fmt.Sprintf(" [[%d]](%s)", index, value)
		return citation, map[string]any{
			"type": "url_citation", "url": value, "title": searchSourceTitle(parsed.SearchSources, value),
		}
	default:
		return "", nil
	}
}

func searchSourceTitle(sources []map[string]any, rawURL string) string {
	if normalized, valid := searchresult.NormalizeURL(rawURL); valid {
		rawURL = normalized
	}
	for _, source := range sources {
		if value, _ := source["url"].(string); value == rawURL {
			if title, _ := source["title"].(string); title != "" {
				return title
			}
		}
	}
	return rawURL
}

func buildOpenAIResult(operation, responseID, model string, parsed parsedChat, streaming bool, responseOptions ...conversation.ResponseOptions) map[string]any {
	created := time.Now().Unix()
	options := conversation.ResponseOptions{}
	if len(responseOptions) > 0 {
		options = responseOptions[0]
	}
	inputTokens := parsed.InputTokens
	outputTokens := estimateTokens(parsed.Text.String()) + estimateTokens(parsed.Reasoning.String()) + estimateToolCallTokens(parsed.ToolCalls)
	if operation == "chat" {
		message := map[string]any{"role": "assistant", "content": parsed.Text.String(), "reasoning_content": parsed.Reasoning.String()}
		if len(parsed.Annotations) > 0 {
			message["annotations"] = chatAnnotations(parsed.Annotations)
		}
		finishReason := "stop"
		if len(parsed.ToolCalls) > 0 {
			finishReason = "tool_calls"
			if parsed.Text.Len() == 0 {
				message["content"] = nil
			}
			message["tool_calls"] = chatToolCalls(parsed.ToolCalls)
		}
		value := map[string]any{
			"id": strings.Replace(responseID, "resp_", "chatcmpl_", 1), "object": "chat.completion", "created": created, "model": model,
			"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finishReason}},
			"usage":   map[string]any{"prompt_tokens": inputTokens, "completion_tokens": outputTokens, "total_tokens": inputTokens + outputTokens},
		}
		if len(parsed.SearchSources) > 0 {
			value["search_sources"] = parsed.SearchSources
		}
		return value
	}
	if operation == conversation.OperationMessages {
		visibleText, stopSequence := applyWebStopSequences(parsed.Text.String(), options.StopSequences)
		emitWebSearch := shouldEmitWebMessagesSearch(parsed, options)
		// Zero initial capacity: tool/search counts come from untrusted upstream.
		content := make([]any, 0)
		if options.AnthropicThinking && parsed.Reasoning.Len() > 0 {
			content = append(content, map[string]any{"type": "thinking", "thinking": parsed.Reasoning.String()})
		}
		if emitWebSearch {
			content = append(content, webMessagesSearchBlocks(newWebID("srvtoolu"), parsed, options)...)
		}
		if visibleText != "" || len(parsed.ToolCalls) == 0 {
			content = append(content, map[string]any{"type": "text", "text": visibleText})
		}
		for _, call := range parsed.ToolCalls {
			var input any = map[string]any{}
			if json.Unmarshal([]byte(call.Arguments), &input) != nil {
				input = map[string]any{}
			}
			content = append(content, map[string]any{"type": "tool_use", "id": webAnthropicToolID(call.ID), "name": call.Name, "input": input})
		}
		stopReason := "end_turn"
		if len(parsed.ToolCalls) > 0 {
			stopReason = "tool_use"
		} else if stopSequence != "" {
			stopReason = "stop_sequence"
		}
		usage := map[string]any{"input_tokens": inputTokens, "output_tokens": outputTokens, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}
		if emitWebSearch {
			usage["server_tool_use"] = map[string]any{"web_search_requests": webMessagesSearchRequests(parsed)}
		}
		return map[string]any{
			"id": strings.Replace(responseID, "resp_", "msg_", 1), "type": "message", "role": "assistant", "model": model,
			"content": content, "stop_reason": stopReason, "stop_sequence": nullableWebString(stopSequence),
			"usage": usage,
		}
	}
	output := make([]any, 0, 2)
	if parsed.Reasoning.Len() > 0 {
		output = append(output, map[string]any{"id": newWebID("rs"), "type": "reasoning", "status": "completed", "summary": []any{map[string]any{"type": "summary_text", "text": parsed.Reasoning.String()}}})
	}
	if parsed.Text.Len() > 0 || len(parsed.ToolCalls) == 0 {
		annotations := parsed.Annotations
		if annotations == nil {
			annotations = []map[string]any{}
		}
		message := map[string]any{"id": newWebID("msg"), "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": parsed.Text.String(), "annotations": annotations, "logprobs": []any{}}}}
		if len(parsed.SearchSources) > 0 {
			message["search_sources"] = parsed.SearchSources
		}
		output = append(output, message)
	}
	for _, call := range parsed.ToolCalls {
		output = append(output, map[string]any{
			"id": newWebID("fc"), "type": "function_call", "status": "completed",
			"call_id": call.ID, "name": call.Name, "arguments": call.Arguments,
		})
	}
	tools := parsed.Tools
	if tools == nil {
		tools = []any{}
	}
	toolChoice := parsed.ToolChoice
	if toolChoice == nil {
		toolChoice = "auto"
	}
	return map[string]any{
		"id": responseID, "object": "response", "created_at": created, "completed_at": created, "status": "completed", "model": model,
		"output": output, "parallel_tool_calls": parsed.ParallelTools, "tools": tools, "tool_choice": toolChoice, "store": true,
		"usage": map[string]any{
			"input_tokens": inputTokens, "output_tokens": outputTokens, "total_tokens": inputTokens + outputTokens,
			"input_tokens_details":  map[string]any{"cached_tokens": 0},
			"output_tokens_details": map[string]any{"reasoning_tokens": estimateTokens(parsed.Reasoning.String())},
			"num_sources_used":      int64(len(parsed.SearchSources)), "num_server_side_tools_used": parsed.ServerTools,
		},
	}
}

func applyWebStopSequences(text string, sequences []string) (string, string) {
	matchAt := -1
	matched := ""
	for _, sequence := range sequences {
		if sequence == "" {
			continue
		}
		if index := strings.Index(text, sequence); index >= 0 && (matchAt < 0 || index < matchAt) {
			matchAt = index
			matched = sequence
		}
	}
	if matchAt < 0 {
		return text, ""
	}
	return text[:matchAt], matched
}

func nullableWebString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func webAnthropicToolID(value string) string {
	if strings.HasPrefix(value, "toolu_") {
		return value
	}
	return "toolu_" + value
}

func webMessagesSearchBlocks(id string, parsed parsedChat, options conversation.ResponseOptions) []any {
	use := map[string]any{
		"type": "server_tool_use", "id": id, "name": "web_search",
		"input": map[string]any{"query": options.AnthropicWebSearchQuery},
	}
	hits := make([]any, 0, searchresult.MaxResults)
	seen := make(map[string]struct{}, searchresult.MaxResults)
	for _, source := range parsed.SearchSources {
		if len(hits) >= searchresult.MaxResults {
			break
		}
		rawURL, _ := source["url"].(string)
		rawURL, valid := searchresult.NormalizeURL(rawURL)
		if !valid {
			continue
		}
		if _, exists := seen[rawURL]; exists {
			continue
		}
		seen[rawURL] = struct{}{}
		title, _ := source["title"].(string)
		title = searchresult.NormalizeTitle(title, rawURL)
		hits = append(hits, map[string]any{"type": "web_search_result", "title": title, "url": rawURL})
	}
	var content any = hits
	if parsed.WebSearchTools == 0 && len(hits) == 0 {
		content = map[string]any{"type": "web_search_tool_result_error", "error_code": "unavailable"}
	}
	result := map[string]any{"type": "web_search_tool_result", "tool_use_id": id, "content": content}
	return []any{use, result}
}

func shouldEmitWebMessagesSearch(parsed parsedChat, options conversation.ResponseOptions) bool {
	return options.AnthropicWebSearch && (options.AnthropicWebSearchRequired || parsed.WebSearchTools > 0 || len(parsed.SearchSources) > 0)
}

func webMessagesSearchRequests(parsed parsedChat) int64 {
	if parsed.WebSearchTools > 0 {
		return parsed.WebSearchTools
	}
	return 1
}

func chatToolCalls(calls []parsedToolCall) []any {
	values := make([]any, 0, len(calls))
	for _, call := range calls {
		values = append(values, map[string]any{
			"id": call.ID, "type": "function",
			"function": map[string]any{"name": call.Name, "arguments": call.Arguments},
		})
	}
	return values
}

func chatAnnotations(annotations []map[string]any) []any {
	values := make([]any, 0, len(annotations))
	for _, annotation := range annotations {
		values = append(values, map[string]any{
			"type": "url_citation",
			"url_citation": map[string]any{
				"url": annotation["url"], "title": annotation["title"],
				"start_index": annotation["start_index"], "end_index": annotation["end_index"],
			},
		})
	}
	return values
}

func estimateToolCallTokens(calls []parsedToolCall) int64 {
	var total int64
	for _, call := range calls {
		total += estimateTokens(call.Name) + estimateTokens(call.Arguments)
	}
	return total
}

type webMessagesStream struct {
	writer          io.Writer
	responseID      string
	model           string
	inputTokens     int64
	options         conversation.ResponseOptions
	started         bool
	thinkingStarted bool
	thinkingClosed  bool
	thinkingIndex   int
	textStarted     bool
	textClosed      bool
	textIndex       int
	nextIndex       int
	hasTools        bool
	webSearchID     string
	webSearchUse    bool
	pendingText     strings.Builder
	stopSequence    string
	stopFilter      *webStopFilter
}

func newWebMessagesStream(writer io.Writer, responseID, model string, inputTokens int64, options conversation.ResponseOptions) *webMessagesStream {
	return &webMessagesStream{
		writer: writer, responseID: responseID, model: model, inputTokens: inputTokens,
		options: options, stopFilter: newWebStopFilter(options.StopSequences),
	}
}

func (s *webMessagesStream) Start() error {
	if s.started {
		return nil
	}
	s.started = true
	if err := writeSSE(s.writer, "message_start", map[string]any{
		"type": "message_start", "message": map[string]any{
			"id": strings.Replace(s.responseID, "resp_", "msg_", 1), "type": "message", "role": "assistant", "model": s.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": s.inputTokens, "output_tokens": 0, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0},
		},
	}); err != nil {
		return err
	}
	if s.options.AnthropicWebSearchRequired && !s.options.AnthropicThinking {
		return s.startWebSearch()
	}
	return nil
}

func (s *webMessagesStream) Delta(kind, delta string) error {
	if err := s.Start(); err != nil {
		return err
	}
	if s.stopSequence != "" {
		return nil
	}
	if kind == "reasoning" {
		if !s.options.AnthropicThinking {
			return nil
		}
		if !s.thinkingStarted {
			s.thinkingStarted = true
			s.thinkingIndex = s.nextIndex
			s.nextIndex++
			if err := writeSSE(s.writer, "content_block_start", map[string]any{
				"type": "content_block_start", "index": s.thinkingIndex,
				"content_block": map[string]any{"type": "thinking", "thinking": ""},
			}); err != nil {
				return err
			}
		}
		return writeSSE(s.writer, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": s.thinkingIndex,
			"delta": map[string]any{"type": "thinking_delta", "thinking": delta},
		})
	}
	if kind != "text" {
		return nil
	}
	if s.options.AnthropicWebSearch {
		if err := s.closeThinking(); err != nil {
			return err
		}
		if s.options.AnthropicWebSearchRequired {
			if err := s.startWebSearch(); err != nil {
				return err
			}
		}
		return s.bufferSearchText(delta)
	}
	if err := s.startText(); err != nil {
		return err
	}
	emit, matched := s.stopFilter.Push(delta)
	if matched != "" {
		s.stopSequence = matched
	}
	if emit == "" {
		return nil
	}
	return s.writeTextDelta(emit)
}

func (s *webMessagesStream) bufferSearchText(delta string) error {
	pending := s.pendingText.Len()
	if pending >= maxDeferredSearchTextBytes || len(delta) > maxDeferredSearchTextBytes-pending {
		return fmt.Errorf("WebSearch 延迟文本缓冲超过 %d MiB", maxDeferredSearchTextBytes>>20)
	}
	s.pendingText.WriteString(delta)
	return nil
}

func (s *webMessagesStream) startText() error {
	if s.textStarted && !s.textClosed {
		return nil
	}
	if err := s.closeThinking(); err != nil {
		return err
	}
	s.textStarted = true
	s.textClosed = false
	s.textIndex = s.nextIndex
	s.nextIndex++
	return writeSSE(s.writer, "content_block_start", map[string]any{
		"type": "content_block_start", "index": s.textIndex,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
}

func (s *webMessagesStream) writeTextDelta(delta string) error {
	if delta == "" {
		return nil
	}
	return writeSSE(s.writer, "content_block_delta", map[string]any{
		"type": "content_block_delta", "index": s.textIndex,
		"delta": map[string]any{"type": "text_delta", "text": delta},
	})
}

func (s *webMessagesStream) Tools(calls []parsedToolCall) error {
	if err := s.Start(); err != nil {
		return err
	}
	if err := s.closeThinking(); err != nil {
		return err
	}
	if err := s.closeText(); err != nil {
		return err
	}
	for _, call := range calls {
		index := s.nextIndex
		s.nextIndex++
		id := webAnthropicToolID(call.ID)
		if err := writeSSE(s.writer, "content_block_start", map[string]any{
			"type": "content_block_start", "index": index,
			"content_block": map[string]any{"type": "tool_use", "id": id, "name": call.Name, "input": map[string]any{}},
		}); err != nil {
			return err
		}
		if err := writeSSE(s.writer, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": call.Arguments},
		}); err != nil {
			return err
		}
		if err := writeSSE(s.writer, "content_block_stop", map[string]any{"type": "content_block_stop", "index": index}); err != nil {
			return err
		}
		s.hasTools = true
	}
	return nil
}

func (s *webMessagesStream) startWebSearch() error {
	if s.webSearchUse {
		return nil
	}
	s.webSearchUse = true
	if s.webSearchID == "" {
		s.webSearchID = newWebID("srvtoolu")
	}
	index := s.nextIndex
	s.nextIndex++
	if err := writeSSE(s.writer, "content_block_start", map[string]any{
		"type": "content_block_start", "index": index,
		"content_block": map[string]any{"type": "server_tool_use", "id": s.webSearchID, "name": "web_search", "input": map[string]any{}},
	}); err != nil {
		return err
	}
	if query := s.options.AnthropicWebSearchQuery; query != "" {
		encoded, _ := json.Marshal(map[string]string{"query": query})
		if err := writeSSE(s.writer, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": string(encoded)},
		}); err != nil {
			return err
		}
	}
	return writeSSE(s.writer, "content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
}

func (s *webMessagesStream) finishWebSearch(parsed parsedChat) error {
	if !shouldEmitWebMessagesSearch(parsed, s.options) {
		return nil
	}
	if err := s.startWebSearch(); err != nil {
		return err
	}
	blocks := webMessagesSearchBlocks(s.webSearchID, parsed, s.options)
	result := blocks[1]
	index := s.nextIndex
	s.nextIndex++
	if err := writeSSE(s.writer, "content_block_start", map[string]any{
		"type": "content_block_start", "index": index, "content_block": result,
	}); err != nil {
		return err
	}
	return writeSSE(s.writer, "content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
}

func (s *webMessagesStream) Finish(parsed parsedChat, payload map[string]any) error {
	if err := s.Start(); err != nil {
		return err
	}
	if err := s.closeThinking(); err != nil {
		return err
	}
	if err := s.finishWebSearch(parsed); err != nil {
		return err
	}
	if s.options.AnthropicWebSearch && s.pendingText.Len() > 0 {
		if err := s.startText(); err != nil {
			return err
		}
		emit, matched := s.stopFilter.Push(s.pendingText.String())
		if matched != "" {
			s.stopSequence = matched
		}
		if err := s.writeTextDelta(emit); err != nil {
			return err
		}
	}
	if s.stopSequence == "" {
		if pending := s.stopFilter.Flush(); pending != "" {
			if err := s.startText(); err != nil {
				return err
			}
			if err := s.writeTextDelta(pending); err != nil {
				return err
			}
		}
	}
	if err := s.closeText(); err != nil {
		return err
	}
	stopReason := "end_turn"
	if s.hasTools || len(parsed.ToolCalls) > 0 {
		stopReason = "tool_use"
	} else if s.stopSequence != "" {
		stopReason = "stop_sequence"
	}
	usage, _ := payload["usage"].(map[string]any)
	finalUsage := map[string]any{"output_tokens": usage["output_tokens"]}
	if shouldEmitWebMessagesSearch(parsed, s.options) {
		finalUsage["server_tool_use"] = map[string]any{"web_search_requests": webMessagesSearchRequests(parsed)}
	}
	if err := writeSSE(s.writer, "message_delta", map[string]any{
		"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nullableWebString(s.stopSequence)},
		"usage": finalUsage,
	}); err != nil {
		return err
	}
	return writeSSE(s.writer, "message_stop", map[string]any{"type": "message_stop"})
}

func (s *webMessagesStream) closeThinking() error {
	if !s.thinkingStarted || s.thinkingClosed {
		return nil
	}
	s.thinkingClosed = true
	return writeSSE(s.writer, "content_block_stop", map[string]any{"type": "content_block_stop", "index": s.thinkingIndex})
}

func (s *webMessagesStream) closeText() error {
	if !s.textStarted || s.textClosed {
		return nil
	}
	s.textClosed = true
	return writeSSE(s.writer, "content_block_stop", map[string]any{"type": "content_block_stop", "index": s.textIndex})
}

func writeWebStreamDelta(writer io.Writer, stream *webMessagesStream, operation, responseID, model, kind, delta string) error {
	if operation == conversation.OperationMessages {
		return stream.Delta(kind, delta)
	}
	return writeStreamDelta(writer, operation, responseID, model, kind, delta)
}

func writeWebStreamToolCalls(writer io.Writer, stream *webMessagesStream, operation, responseID, model string, calls []parsedToolCall) error {
	if operation == conversation.OperationMessages {
		return stream.Tools(calls)
	}
	return writeStreamToolCalls(writer, operation, responseID, model, calls)
}

type webStopFilter struct {
	sequences []string
	pending   string
	matched   string
}

func newWebStopFilter(sequences []string) *webStopFilter {
	filtered := make([]string, 0, len(sequences))
	for _, sequence := range sequences {
		if sequence != "" {
			filtered = append(filtered, sequence)
		}
	}
	return &webStopFilter{sequences: filtered}
}

func (f *webStopFilter) Push(delta string) (string, string) {
	if f == nil || len(f.sequences) == 0 {
		return delta, ""
	}
	if f.matched != "" {
		return "", f.matched
	}
	f.pending += delta
	matchAt := -1
	matched := ""
	for _, sequence := range f.sequences {
		if index := strings.Index(f.pending, sequence); index >= 0 && (matchAt < 0 || index < matchAt) {
			matchAt = index
			matched = sequence
		}
	}
	if matchAt >= 0 {
		emit := f.pending[:matchAt]
		f.pending = ""
		f.matched = matched
		return emit, matched
	}
	hold := 0
	for _, sequence := range f.sequences {
		maxPrefix := min(len(sequence)-1, len(f.pending))
		for size := maxPrefix; size > hold; size-- {
			if strings.HasSuffix(f.pending, sequence[:size]) {
				hold = size
				break
			}
		}
	}
	emitAt := len(f.pending) - hold
	emit := f.pending[:emitAt]
	f.pending = f.pending[emitAt:]
	return emit, ""
}

func (f *webStopFilter) Flush() string {
	if f == nil || f.matched != "" {
		return ""
	}
	value := f.pending
	f.pending = ""
	return value
}

func writeStreamStart(writer io.Writer, operation, responseID, model string, inputTokens int64) {
	if operation == "chat" {
		chunk := map[string]any{"id": strings.Replace(responseID, "resp_", "chatcmpl_", 1), "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}}}
		writeSSE(writer, "", chunk)
		return
	}
	if operation == conversation.OperationMessages {
		writeSSE(writer, "message_start", map[string]any{
			"type": "message_start", "message": map[string]any{
				"id": strings.Replace(responseID, "resp_", "msg_", 1), "type": "message", "role": "assistant", "model": model,
				"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]any{"input_tokens": inputTokens, "output_tokens": 0, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0},
			},
		})
		writeSSE(writer, "content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		return
	}
	writeSSE(writer, "response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": responseID, "object": "response", "status": "in_progress", "model": model, "output": []any{}}})
}

func writeStreamDelta(writer io.Writer, operation, responseID, model, kind, delta string) error {
	if operation == "chat" {
		field := "content"
		if kind == "reasoning" {
			field = "reasoning_content"
		}
		chunk := map[string]any{"id": strings.Replace(responseID, "resp_", "chatcmpl_", 1), "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{field: delta}, "finish_reason": nil}}}
		return writeSSE(writer, "", chunk)
	}
	if operation == conversation.OperationMessages {
		if kind == "reasoning" {
			return nil
		}
		return writeSSE(writer, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": delta}})
	}
	event := "response.output_text.delta"
	if kind == "reasoning" {
		event = "response.reasoning_summary_text.delta"
	}
	return writeSSE(writer, event, map[string]any{"type": event, "response_id": responseID, "delta": delta})
}

func writeStreamToolCalls(writer io.Writer, operation, responseID, model string, calls []parsedToolCall) error {
	if operation == "chat" {
		for index, call := range calls {
			chunk := map[string]any{
				"id": strings.Replace(responseID, "resp_", "chatcmpl_", 1), "object": "chat.completion.chunk",
				"created": time.Now().Unix(), "model": model,
				"choices": []any{map[string]any{"index": 0, "finish_reason": nil, "delta": map[string]any{
					"tool_calls": []any{map[string]any{
						"index": index, "id": call.ID, "type": "function",
						"function": map[string]any{"name": call.Name, "arguments": call.Arguments},
					}},
				}}},
			}
			if err := writeSSE(writer, "", chunk); err != nil {
				return err
			}
		}
		return nil
	}
	if operation == conversation.OperationMessages {
		for index, call := range calls {
			contentIndex := index + 1
			if err := writeSSE(writer, "content_block_start", map[string]any{
				"type": "content_block_start", "index": contentIndex,
				"content_block": map[string]any{"type": "tool_use", "id": call.ID, "name": call.Name, "input": map[string]any{}},
			}); err != nil {
				return err
			}
			if err := writeSSE(writer, "content_block_delta", map[string]any{
				"type": "content_block_delta", "index": contentIndex,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": call.Arguments},
			}); err != nil {
				return err
			}
			if err := writeSSE(writer, "content_block_stop", map[string]any{"type": "content_block_stop", "index": contentIndex}); err != nil {
				return err
			}
		}
		return nil
	}
	for index, call := range calls {
		itemID := newWebID("fc")
		item := map[string]any{"id": itemID, "type": "function_call", "status": "in_progress", "call_id": call.ID, "name": call.Name, "arguments": ""}
		if err := writeSSE(writer, "response.output_item.added", map[string]any{
			"type": "response.output_item.added", "response_id": responseID, "output_index": index, "item": item,
		}); err != nil {
			return err
		}
		if err := writeSSE(writer, "response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "response_id": responseID,
			"item_id": itemID, "output_index": index, "delta": call.Arguments,
		}); err != nil {
			return err
		}
		if err := writeSSE(writer, "response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "response_id": responseID,
			"item_id": itemID, "output_index": index, "arguments": call.Arguments,
		}); err != nil {
			return err
		}
		item["status"] = "completed"
		item["arguments"] = call.Arguments
		if err := writeSSE(writer, "response.output_item.done", map[string]any{
			"type": "response.output_item.done", "response_id": responseID, "output_index": index, "item": item,
		}); err != nil {
			return err
		}
	}
	return nil
}

func writeStreamDone(writer io.Writer, operation, responseID, model string, parsed parsedChat, payload map[string]any) {
	if operation == "chat" {
		finishReason := "stop"
		if len(parsed.ToolCalls) > 0 {
			finishReason = "tool_calls"
		}
		chunk := map[string]any{"id": strings.Replace(responseID, "resp_", "chatcmpl_", 1), "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason}}, "usage": payload["usage"]}
		if len(parsed.Annotations) > 0 {
			chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["annotations"] = chatAnnotations(parsed.Annotations)
		}
		if sources := payload["search_sources"]; sources != nil {
			chunk["search_sources"] = sources
		}
		writeSSE(writer, "", chunk)
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
		return
	}
	if operation == conversation.OperationMessages {
		writeSSE(writer, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		stopReason := "end_turn"
		if len(parsed.ToolCalls) > 0 {
			stopReason = "tool_use"
		}
		usage, _ := payload["usage"].(map[string]any)
		writeSSE(writer, "message_delta", map[string]any{
			"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": usage["output_tokens"]},
		})
		writeSSE(writer, "message_stop", map[string]any{"type": "message_stop"})
		return
	}
	if parsed.Text.Len() > 0 {
		writeSSE(writer, "response.output_text.done", map[string]any{"type": "response.output_text.done", "response_id": responseID, "text": parsed.Text.String()})
	}
	writeSSE(writer, "response.completed", map[string]any{"type": "response.completed", "response": payload})
}

func writeSSE(writer io.Writer, event string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if event != "" {
		if _, err := fmt.Fprintf(writer, "event: %s\n", event); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(writer, "data: %s\n\n", data)
	return err
}

func estimateTokens(value string) int64 {
	count := utf8.RuneCountInString(value)
	if count == 0 {
		return 0
	}
	return int64((count + 3) / 4)
}

func newWebID(prefix string) string {
	value := make([]byte, 16)
	_, _ = rand.Read(value)
	return prefix + "_" + hex.EncodeToString(value)
}

func streamHeaders() http.Header {
	value := http.Header{}
	value.Set("Content-Type", "text/event-stream; charset=utf-8")
	value.Set("Cache-Control", "no-cache")
	value.Set("X-Accel-Buffering", "no")
	return value
}

func jsonHeaders() http.Header {
	value := http.Header{}
	value.Set("Content-Type", "application/json; charset=utf-8")
	return value
}

func jsonProviderResponse(status int, value any) *provider.Response {
	data, _ := json.Marshal(value)
	return &provider.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), Header: jsonHeaders(), Body: io.NopCloser(bytes.NewReader(data))}
}

type releaseBody struct {
	io.ReadCloser
	release func()
}

func (b *releaseBody) Close() error {
	err := b.ReadCloser.Close()
	if b.release != nil {
		b.release()
		b.release = nil
	}
	return err
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

type readerCloser struct {
	io.Reader
	closer io.Closer
}

func (r *readerCloser) Close() error { return r.closer.Close() }

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	if b.cancel != nil {
		b.cancel()
		b.cancel = nil
	}
	return err
}
