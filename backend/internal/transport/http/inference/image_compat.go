package inference

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/gin-gonic/gin"
)

type conversationImageInput struct {
	prompt         string
	images         []string
	count          int
	size           string
	aspectRatio    string
	resolution     string
	quality        string
	responseFormat string
}

// tryImageConversation bridges an image model selected in a text-oriented
// client to the same audited image gateway used by /images/generations and
// /images/edits. Only the latest user turn supplies the prompt and references.
func (h *Handler) tryImageConversation(c *gin.Context, body []byte, model string, stream bool, key clientkey.Key, requestID string, protocol streamProtocol) bool {
	if h.models == nil || h.gateway == nil {
		return false
	}
	routes, err := h.models.GetByPublicIDCandidates(c.Request.Context(), model)
	if err != nil {
		return false
	}
	hasGeneration, hasEdit := false, false
	for _, route := range routes {
		switch route.Capability {
		case modeldomain.CapabilityImage:
			hasGeneration = true
		case modeldomain.CapabilityImageEdit:
			hasEdit = true
		}
	}
	if !hasGeneration && !hasEdit {
		return false
	}
	input, err := parseConversationImageInput(body, protocol)
	if err != nil {
		writeConversationImageError(c, protocol, http.StatusBadRequest, "invalid_request", err.Error())
		return true
	}
	if input.count < 1 || input.count > 10 {
		writeConversationImageError(c, protocol, http.StatusBadRequest, "invalid_parameter", "n 必须在 1 到 10 之间")
		return true
	}
	if strings.TrimSpace(input.prompt) == "" {
		writeConversationImageError(c, protocol, http.StatusBadRequest, "invalid_request", "图片模型需要当前用户消息中的提示词")
		return true
	}
	var result *gateway.Result
	if len(input.images) > 0 {
		if !hasEdit {
			writeConversationImageError(c, protocol, http.StatusBadRequest, "unsupported_operation", "此图片模型不支持参考图编辑")
			return true
		}
		if len(input.images) > 8 {
			writeConversationImageError(c, protocol, http.StatusBadRequest, "invalid_request", "参考图最多 8 张")
			return true
		}
		result, err = h.gateway.EditImage(c.Request.Context(), gateway.ImageEditInput{
			RequestID: requestID, ClientKey: key, PublicModel: model, Prompt: input.prompt,
			ImageURLs: input.images, Count: input.count, Size: input.size, AspectRatio: input.aspectRatio,
			Resolution: input.resolution, Quality: input.quality, ResponseFormat: input.responseFormat,
			Method: c.Request.Method, Path: c.Request.URL.Path, Headers: c.Request.Header.Clone(),
		})
	} else {
		if !hasGeneration {
			writeConversationImageError(c, protocol, http.StatusBadRequest, "invalid_request", "图片编辑模型需要当前用户消息中的参考图")
			return true
		}
		result, err = h.gateway.GenerateImage(c.Request.Context(), gateway.ImageGenerationInput{
			RequestID: requestID, ClientKey: key, PublicModel: model, Prompt: input.prompt,
			Count: input.count, Size: input.size, AspectRatio: input.aspectRatio,
			Resolution: input.resolution, Quality: input.quality, ResponseFormat: input.responseFormat,
			Method: c.Request.Method, Path: c.Request.URL.Path, Headers: c.Request.Header.Clone(),
		})
	}
	if err != nil {
		if protocol == streamProtocolAnthropic {
			writeGatewayAnthropicError(c, err)
		} else {
			writeGatewayError(c, err)
		}
		return true
	}
	h.writeConversationImageResult(c, result, model, stream, protocol)
	return true
}

func parseConversationImageInput(body []byte, protocol streamProtocol) (conversationImageInput, error) {
	input := conversationImageInput{count: 1, responseFormat: "url"}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return input, fmt.Errorf("图片请求 JSON 无效: %w", err)
	}
	if len(bytes.TrimSpace(root["tools"])) > 0 && !bytes.Equal(bytes.TrimSpace(root["tools"]), []byte("null")) && !bytes.Equal(bytes.TrimSpace(root["tools"]), []byte("[]")) {
		return input, fmt.Errorf("图片模型不支持 tools")
	}
	content := json.RawMessage(nil)
	switch protocol {
	case streamProtocolResponses:
		var raw string
		if json.Unmarshal(root["input"], &raw) == nil {
			content, _ = json.Marshal(raw)
		} else {
			content = latestUserContent(root["input"])
		}
	default:
		content = latestUserContent(root["messages"])
	}
	if len(content) == 0 {
		return input, fmt.Errorf("当前请求中缺少用户消息")
	}
	prompt, images, err := parseImageMessageContent(content)
	if err != nil {
		return input, err
	}
	input.prompt, input.images = prompt, images
	var options struct {
		Count          *int            `json:"n"`
		Size           string          `json:"size"`
		AspectRatio    string          `json:"aspect_ratio"`
		Resolution     string          `json:"resolution"`
		Quality        string          `json:"quality"`
		ResponseFormat json.RawMessage `json:"response_format"`
		ImageConfig    *struct {
			Count          *int   `json:"n"`
			AspectRatio    string `json:"aspect_ratio"`
			Resolution     string `json:"resolution"`
			ResponseFormat string `json:"response_format"`
		} `json:"image_config"`
	}
	if err := json.Unmarshal(body, &options); err != nil {
		return input, fmt.Errorf("图片参数无效: %w", err)
	}
	if options.Count != nil {
		input.count = *options.Count
	}
	input.size, input.aspectRatio = options.Size, options.AspectRatio
	input.resolution, input.quality = options.Resolution, options.Quality
	var topLevelFormat string
	_ = json.Unmarshal(options.ResponseFormat, &topLevelFormat)
	if value := strings.TrimSpace(topLevelFormat); value != "" {
		input.responseFormat = value
	}
	if options.ImageConfig != nil {
		if options.ImageConfig.Count != nil {
			input.count = *options.ImageConfig.Count
		}
		if value := strings.TrimSpace(options.ImageConfig.AspectRatio); value != "" {
			input.aspectRatio = value
		}
		if value := strings.TrimSpace(options.ImageConfig.Resolution); value != "" {
			input.resolution = value
		}
		if value := strings.TrimSpace(options.ImageConfig.ResponseFormat); value != "" {
			input.responseFormat = value
		}
	}
	return input, nil
}

func latestUserContent(raw json.RawMessage) json.RawMessage {
	var messages []struct {
		Role    string          `json:"role"`
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &messages) != nil {
		return nil
	}
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if strings.EqualFold(strings.TrimSpace(message.Role), "user") && (message.Type == "" || message.Type == "message") {
			return message.Content
		}
	}
	return nil
}

func parseImageMessageContent(raw json.RawMessage) (string, []string, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text), nil, nil
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return "", nil, fmt.Errorf("用户消息的 content 必须是文本或内容数组")
	}
	texts := make([]string, 0, len(parts))
	images := make([]string, 0, 1)
	for _, part := range parts {
		var kind string
		_ = json.Unmarshal(part["type"], &kind)
		switch kind {
		case "text", "input_text":
			var value string
			_ = json.Unmarshal(part["text"], &value)
			if value = strings.TrimSpace(value); value != "" {
				texts = append(texts, value)
			}
		case "image_url", "input_image", "image":
			value, err := imagePartURL(part)
			if err != nil {
				return "", nil, err
			}
			images = append(images, value)
		}
	}
	return strings.Join(texts, "\n"), images, nil
}

func imagePartURL(part map[string]json.RawMessage) (string, error) {
	var value string
	if json.Unmarshal(part["image_url"], &value) != nil {
		var object struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(part["image_url"], &object)
		value = object.URL
	}
	if value == "" {
		_ = json.Unmarshal(part["url"], &value)
	}
	if value == "" && len(part["source"]) > 0 {
		var source struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
			URL       string `json:"url"`
		}
		_ = json.Unmarshal(part["source"], &source)
		switch source.Type {
		case "url":
			value = source.URL
		case "base64":
			if strings.HasPrefix(source.MediaType, "image/") && source.Data != "" {
				value = "data:" + source.MediaType + ";base64," + source.Data
			}
		}
	}
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("参考图必须提供 image_url 或 base64 图片数据")
	}
	return strings.TrimSpace(value), nil
}

func writeConversationImageError(c *gin.Context, protocol streamProtocol, status int, code, message string) {
	if protocol == streamProtocolAnthropic {
		writeAnthropicError(c, status, anthropicUpstreamHTTPErrorType(status), message, code)
	} else {
		writeOpenAIError(c, status, code, message)
	}
}

func (h *Handler) writeConversationImageResult(c *gin.Context, result *gateway.Result, model string, stream bool, protocol streamProtocol) {
	if result == nil || result.Body == nil {
		writeConversationImageError(c, protocol, http.StatusBadGateway, "invalid_upstream_response", "图片服务返回空响应")
		return
	}
	data, readErr := io.ReadAll(io.LimitReader(result.Body, maxJSONResponseTransferBytes+1))
	if readErr != nil || len(data) > maxJSONResponseTransferBytes {
		result.Finalize(gateway.Usage{}, "", "image_response_read_failed")
		_ = result.Body.Close()
		writeConversationImageError(c, protocol, http.StatusBadGateway, "invalid_upstream_response", "读取图片响应失败或响应过大")
		return
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		code, message := gateway.ClassifyUpstreamHTTPError(result.StatusCode, data)
		result.Finalize(gateway.Usage{}, "", code)
		_ = result.Body.Close()
		writeConversationImageError(c, protocol, result.StatusCode, code, message)
		return
	}
	var payload struct {
		Data []struct {
			URL      string `json:"url"`
			B64JSON  string `json:"b64_json"`
			MIMEType string `json:"mime_type"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &payload) != nil || len(payload.Data) == 0 {
		result.Finalize(gateway.Usage{}, "", "invalid_image_response")
		_ = result.Body.Close()
		writeConversationImageError(c, protocol, http.StatusBadGateway, "invalid_upstream_response", "图片服务未返回有效图片")
		return
	}
	markdown := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		url := strings.TrimSpace(item.URL)
		if url == "" && item.B64JSON != "" {
			mimeType := strings.TrimSpace(item.MIMEType)
			if !strings.HasPrefix(mimeType, "image/") {
				mimeType = "image/png"
			}
			url = "data:" + mimeType + ";base64," + item.B64JSON
		}
		if strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://") ||
			strings.HasPrefix(url, "data:image/") || (strings.HasPrefix(url, "/") && !strings.HasPrefix(url, "//")) {
			markdown = append(markdown, "![生成图片]("+strings.ReplaceAll(url, ")", "%29")+")")
		}
	}
	if len(markdown) == 0 {
		result.Finalize(gateway.Usage{}, "", "invalid_image_response")
		_ = result.Body.Close()
		writeConversationImageError(c, protocol, http.StatusBadGateway, "invalid_upstream_response", "图片服务未返回有效图片地址")
		return
	}
	result.Finalize(gateway.Usage{}, "", "")
	_ = result.Body.Close()
	writeImageConversation(c, protocol, model, strings.Join(markdown, "\n\n"), stream)
}

func writeImageConversation(c *gin.Context, protocol streamProtocol, model, content string, stream bool) {
	id := fmt.Sprintf("resp_img_%d", time.Now().UnixNano())
	created := time.Now().Unix()
	if !stream {
		switch protocol {
		case streamProtocolChat:
			c.JSON(http.StatusOK, gin.H{"id": strings.Replace(id, "resp_", "chatcmpl_", 1), "object": "chat.completion", "created": created, "model": model,
				"choices": []any{gin.H{"index": 0, "message": gin.H{"role": "assistant", "content": content}, "finish_reason": "stop"}},
				"usage":   gin.H{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}})
		case streamProtocolAnthropic:
			c.JSON(http.StatusOK, gin.H{"id": strings.Replace(id, "resp_", "msg_", 1), "type": "message", "role": "assistant", "model": model,
				"content": []any{gin.H{"type": "text", "text": content}}, "stop_reason": "end_turn", "stop_sequence": nil,
				"usage": gin.H{"input_tokens": 0, "output_tokens": 0}})
		default:
			c.JSON(http.StatusOK, imageResponsesObject(id, model, content, created))
		}
		return
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Status(http.StatusOK)
	emit := func(event string, value any) {
		data, _ := json.Marshal(value)
		if event != "" {
			_, _ = fmt.Fprintf(c.Writer, "event: %s\n", event)
		}
		_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", data)
		c.Writer.Flush()
	}
	switch protocol {
	case streamProtocolChat:
		chatID := strings.Replace(id, "resp_", "chatcmpl_", 1)
		chunk := func(delta any, finish any) gin.H {
			return gin.H{"id": chatID, "object": "chat.completion.chunk", "created": created, "model": model,
				"choices": []any{gin.H{"index": 0, "delta": delta, "finish_reason": finish}}}
		}
		emit("", chunk(gin.H{"role": "assistant"}, nil))
		emit("", chunk(gin.H{"content": content}, nil))
		emit("", chunk(gin.H{}, "stop"))
		_, _ = io.WriteString(c.Writer, "data: [DONE]\n\n")
	case streamProtocolAnthropic:
		messageID := strings.Replace(id, "resp_", "msg_", 1)
		emit("message_start", gin.H{"type": "message_start", "message": gin.H{"id": messageID, "type": "message", "role": "assistant", "model": model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": gin.H{"input_tokens": 0, "output_tokens": 0}}})
		emit("content_block_start", gin.H{"type": "content_block_start", "index": 0, "content_block": gin.H{"type": "text", "text": ""}})
		emit("content_block_delta", gin.H{"type": "content_block_delta", "index": 0, "delta": gin.H{"type": "text_delta", "text": content}})
		emit("content_block_stop", gin.H{"type": "content_block_stop", "index": 0})
		emit("message_delta", gin.H{"type": "message_delta", "delta": gin.H{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": gin.H{"output_tokens": 0}})
		emit("message_stop", gin.H{"type": "message_stop"})
	default:
		response := imageResponsesObject(id, model, content, created)
		item := response["output"].([]any)[0]
		sequence := 0
		emitResponses := func(event string, value gin.H) {
			value["sequence_number"] = sequence
			sequence++
			emit(event, value)
		}
		emitResponses("response.created", gin.H{"type": "response.created", "response": gin.H{"id": id, "object": "response", "model": model, "created_at": created, "status": "in_progress", "output": []any{}}})
		emitResponses("response.output_item.added", gin.H{"type": "response.output_item.added", "output_index": 0, "item": gin.H{"id": "msg_" + id, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}})
		emitResponses("response.content_part.added", gin.H{"type": "response.content_part.added", "output_index": 0, "content_index": 0, "part": gin.H{"type": "output_text", "text": ""}})
		emitResponses("response.output_text.delta", gin.H{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "delta": content})
		emitResponses("response.output_text.done", gin.H{"type": "response.output_text.done", "output_index": 0, "content_index": 0, "text": content})
		emitResponses("response.content_part.done", gin.H{"type": "response.content_part.done", "output_index": 0, "content_index": 0, "part": gin.H{"type": "output_text", "text": content}})
		emitResponses("response.output_item.done", gin.H{"type": "response.output_item.done", "output_index": 0, "item": item})
		emitResponses("response.completed", gin.H{"type": "response.completed", "response": response})
	}
	c.Writer.Flush()
}

func imageResponsesObject(id, model, content string, created int64) gin.H {
	return gin.H{"id": id, "object": "response", "created_at": created, "model": model, "status": "completed",
		"output": []any{gin.H{"id": "msg_" + id, "type": "message", "role": "assistant", "status": "completed",
			"content": []any{gin.H{"type": "output_text", "text": content, "annotations": []any{}}}}},
		"usage": gin.H{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}}
}
