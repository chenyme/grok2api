package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	fhttp "github.com/bogdanfinn/fhttp"
	fhttptest "github.com/bogdanfinn/fhttp/httptest"
	"github.com/bogdanfinn/websocket"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	providerstreamidle "github.com/chenyme/grok2api/backend/internal/infra/provider/streamidle"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/neterror"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestCatalogMatchesSupportedSurface(t *testing.T) {
	values := Catalog()
	requiredModels := []string{"grok-chat-fast", "grok-chat-auto", "grok-chat-expert", "grok-chat-heavy", "grok-imagine-image-lite", "grok-imagine-image-quality-lite", "grok-imagine-image-edit", "grok-imagine-video"}
	if len(values) != len(requiredModels) {
		t.Fatalf("catalog size = %d", len(values))
	}
	publicIDs := make(map[string]struct{}, len(values))
	routeKeys := make(map[string]struct{}, len(values))
	for _, value := range values {
		routeKey := value.PublicID + "|" + string(value.Capability)
		if _, exists := routeKeys[routeKey]; exists {
			t.Fatalf("duplicate public model capability: %s", routeKey)
		}
		routeKeys[routeKey] = struct{}{}
		publicIDs[value.PublicID] = struct{}{}
	}
	for _, required := range requiredModels {
		if _, exists := publicIDs[required]; !exists {
			t.Fatalf("missing supported model: %s", required)
		}
	}
	for _, required := range []string{
		"grok-imagine-image-lite|image",
		"grok-imagine-image-quality-lite|image",
		"grok-imagine-image-edit|image_edit",
	} {
		if _, exists := routeKeys[required]; !exists {
			t.Fatalf("missing supported route: %s", required)
		}
	}
	for _, removed := range []string{"grok-imagine-image", "grok-imagine-image-quality", "grok-imagine-image-2.0", "grok-imagine-image-quality-2.0", "grok-imagine-image-speed", "grok-imagine-image-pro"} {
		if _, exists := publicIDs[removed]; exists {
			t.Fatalf("obsolete image model remains: %s", removed)
		}
	}
}

func TestWebImagePublicNamesPreserveGatewayModels(t *testing.T) {
	tests := map[string]string{
		"grok-imagine-image":         "grok-imagine-image-lite",
		"grok-imagine-image-quality": "grok-imagine-image-quality-lite",
	}
	for upstreamModel, publicID := range tests {
		spec, ok := Resolve(upstreamModel)
		if !ok {
			t.Fatalf("missing upstream model %s", upstreamModel)
		}
		if spec.PublicID != publicID || spec.UpstreamModel != upstreamModel || spec.Capability != modeldomain.CapabilityImage {
			t.Fatalf("resolved %s as %#v", upstreamModel, spec)
		}
	}
	spec, ok := Resolve("imagine-image-edit")
	if !ok || spec.PublicID != "grok-imagine-image-edit" || spec.Capability != modeldomain.CapabilityImageEdit {
		t.Fatalf("edit upstream resolved as %#v ok=%v", spec, ok)
	}
}

func TestParseMediaPostResponsePreservesStatusAndPostID(t *testing.T) {
	postID, err := parseMediaPostResponse(&http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"post":{"id":"post_1","videos":[{"id":"post_1"}]}}`)),
	})
	if err != nil || postID != "post_1" {
		t.Fatalf("postID=%q err=%v", postID, err)
	}
	_, err = parseMediaPostResponse(&http.Response{
		StatusCode: http.StatusForbidden,
		Body:       io.NopCloser(strings.NewReader(`{"error":"challenge"}`)),
	})
	var upstreamErr *webMediaUpstreamError
	if !errors.As(err, &upstreamErr) || upstreamErr.status != http.StatusForbidden || !strings.Contains(err.Error(), "challenge") {
		t.Fatalf("error = %#v", err)
	}
}

func TestWebChatPricingUsesGrok45(t *testing.T) {
	registry := provider.NewRegistry(&Adapter{})
	for _, upstreamModel := range []string{"grok-chat-fast", "grok-chat-auto", "grok-chat-expert", "grok-chat-heavy"} {
		if got := registry.PricingModel(account.ProviderWeb, upstreamModel); got != "grok-4.5" {
			t.Fatalf("pricing model for %s = %q", upstreamModel, got)
		}
	}
	mediaModels := map[string]string{
		"grok-imagine-image": "grok-imagine-image", "grok-imagine-image-quality": "grok-imagine-image-quality",
		"imagine-image-edit": "grok-imagine-image-edit", "grok-imagine-video": "grok-imagine-video",
	}
	for upstreamModel, expected := range mediaModels {
		if got := registry.PricingModel(account.ProviderWeb, upstreamModel); got != expected {
			t.Fatalf("media pricing model for %s = %q", upstreamModel, got)
		}
	}
}

func TestNormalizeOpenAIInputSeparatesTextAndImages(t *testing.T) {
	dataURI := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	content, _ := json.Marshal([]any{
		map[string]any{"type": "text", "text": "描述这张图"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURI}},
	})
	value, err := normalizeOpenAIInput(openAIRequest{Messages: []chatMessage{{Role: "user", Content: content}}}, "chat")
	if err != nil {
		t.Fatal(err)
	}
	if value.Prompt != "[user]\n描述这张图" || !slices.Equal(value.Attachments, []chatAttachmentInput{{Source: dataURI, Image: true}}) {
		t.Fatalf("normalized input = %#v", value)
	}
}

func TestNormalizeLatestImageInputIgnoresHistoricalAssistantImage(t *testing.T) {
	historical, _ := json.Marshal([]any{
		map[string]any{"type": "text", "text": "first result"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/first.png"}},
	})
	latest, _ := json.Marshal("生成另一张蓝色的图片")
	value, err := normalizeLatestImageInput(openAIRequest{Messages: []chatMessage{
		{Role: "user", Content: json.RawMessage(`"生成第一张图片"`)},
		{Role: "assistant", Content: historical},
		{Role: "user", Content: latest},
	}}, conversation.OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	if value.Prompt != "生成另一张蓝色的图片" || len(value.Attachments) != 0 {
		t.Fatalf("normalized latest image input = %#v", value)
	}
}

func TestNormalizeLatestResponsesImageInputIgnoresHistory(t *testing.T) {
	input, _ := json.Marshal([]any{
		map[string]any{"type": "message", "role": "assistant", "content": []any{
			map[string]any{"type": "output_text", "text": "![image](https://example.com/first.png)"},
			map[string]any{"type": "input_image", "image_url": "https://example.com/first.png"},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "generate a different image"},
		}},
	})
	value, err := normalizeLatestImageInput(openAIRequest{Input: input}, conversation.OperationResponses)
	if err != nil {
		t.Fatal(err)
	}
	if value.Prompt != "generate a different image" || len(value.Attachments) != 0 {
		t.Fatalf("normalized latest responses input = %#v", value)
	}
}

func TestImageChatRejectsOnlyCurrentTurnAttachment(t *testing.T) {
	content, _ := json.Marshal([]any{
		map[string]any{"type": "text", "text": "make it blue"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/input.png"}},
	})
	response, err := (&Adapter{}).ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Method: http.MethodPost, Model: "grok-imagine-image", Operation: conversation.OperationChat,
		Body: []byte(fmt.Sprintf(`{"model":"grok-imagine-image-lite","messages":[{"role":"user","content":%s}]}`, content)),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "/v1/images/edits") {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
}

func TestQualityImageResponsesUsesImageCompatibilityValidation(t *testing.T) {
	response, err := (&Adapter{}).ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Method: http.MethodPost, Model: "grok-imagine-image-quality", Operation: conversation.OperationResponses,
		Body: []byte(`{"model":"grok-imagine-image-quality-lite","input":[{"role":"user","content":[{"type":"input_text","text":"draw"}]}],"image_config":{"n":0}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "image_config.n") || strings.Contains(string(body), "模型不支持文本对话") {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
}

func TestImageCompatibilityResponsesStreamUsesOutputStateMachine(t *testing.T) {
	parsed := parsedChat{ResponseID: "resp_image", InputTokens: 3}
	parsed.appendText("![image](https://example.com/generated.png)")
	body, err := buildImageCompatibilityStream(
		conversation.OperationResponses,
		parsed.ResponseID,
		"grok-imagine-image-lite",
		&parsed,
	)
	if err != nil {
		t.Fatal(err)
	}
	stream := string(body)
	for _, event := range []string{
		"response.created",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	} {
		if !strings.Contains(stream, "event: "+event+"\n") {
			t.Fatalf("stream missing %s: %s", event, stream)
		}
	}
	if strings.Contains(stream, "chat.completion.chunk") || strings.Contains(stream, "data: [DONE]") {
		t.Fatalf("Responses stream contains Chat Completions envelope: %s", stream)
	}
	if len(parsed.ResponseOutput) != 1 {
		t.Fatalf("response output = %#v", parsed.ResponseOutput)
	}
	item := parsed.ResponseOutput[0].(map[string]any)
	if item["type"] != "message" || item["status"] != "completed" {
		t.Fatalf("response output item = %#v", item)
	}
	itemID, _ := item["id"].(string)
	completedAt := strings.Index(stream, "event: response.completed\n")
	if itemID == "" || completedAt < 0 || !strings.Contains(stream[completedAt:], `"id":"`+itemID+`"`) {
		t.Fatalf("completed response does not reuse output item %q: %s", itemID, stream)
	}
}

func TestNormalizeResponsesInputImage(t *testing.T) {
	dataURI := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	input, _ := json.Marshal([]any{map[string]any{
		"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "what is this"},
			map[string]any{"type": "input_image", "image_url": dataURI},
		},
	}})
	value, err := normalizeOpenAIInput(openAIRequest{Input: input}, "responses")
	if err != nil {
		t.Fatal(err)
	}
	if value.Prompt != "[user]\nwhat is this" || !slices.Equal(value.Attachments, []chatAttachmentInput{{Source: dataURI, Image: true}}) {
		t.Fatalf("normalized responses input = %#v", value)
	}
}

func TestNormalizeResponsesInputFile(t *testing.T) {
	input, _ := json.Marshal([]any{map[string]any{
		"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_file", "file_url": "https://example.com/a.pdf", "filename": "report.pdf"},
		},
	}})
	value, err := normalizeOpenAIInput(openAIRequest{Input: input}, "responses")
	if err != nil || !slices.Equal(value.Attachments, []chatAttachmentInput{{Source: "https://example.com/a.pdf", Filename: "report.pdf"}}) {
		t.Fatalf("normalized=%#v error=%v", value, err)
	}
}

func TestNormalizeChatNestedFileData(t *testing.T) {
	content, _ := json.Marshal([]any{map[string]any{
		"type": "file", "file": map[string]any{
			"filename": "notes.txt", "file_data": "data:text/plain;base64,aGVsbG8=",
		},
	}})
	value, err := normalizeOpenAIInput(openAIRequest{Messages: []chatMessage{{Role: "user", Content: content}}}, "chat")
	if err != nil || !slices.Equal(value.Attachments, []chatAttachmentInput{{Source: "data:text/plain;base64,aGVsbG8=", Filename: "notes.txt"}}) {
		t.Fatalf("normalized=%#v error=%v", value, err)
	}
	if _, err := parseChatFileDataURI(value.Attachments[0].Source, value.Attachments[0].Filename, 1<<20); err != nil {
		t.Fatal(err)
	}
	badContent, _ := json.Marshal([]any{map[string]any{"type": "input_file", "file_id": "file_external"}})
	if _, err := normalizeOpenAIInput(openAIRequest{Messages: []chatMessage{{Role: "user", Content: badContent}}}, "chat"); err == nil || !strings.Contains(err.Error(), "file_id") {
		t.Fatalf("file_id error=%v", err)
	}
}

func TestParseChatImageDataURIValidatesContent(t *testing.T) {
	value := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	image, err := parseChatImageDataURI(value, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if image.MIMEType != "image/png" || image.Filename != "image.png" || len(image.Data) == 0 {
		t.Fatalf("image = %#v", image)
	}
	if _, err := parseChatImageDataURI("data:image/png;base64,bm90IGFuIGltYWdl", 1<<20); err == nil {
		t.Fatal("non-image data URI was accepted")
	}
}

func TestParseChatFileDataURIValidatesAndSanitizesFile(t *testing.T) {
	pdf := "data:application/pdf;base64,JVBERi0xLjQKJSVFT0Y="
	file, err := parseChatFileDataURI(pdf, "../report.pdf", 1<<20)
	if err != nil || file.MIMEType != "application/pdf" || file.Filename != "report.pdf" || len(file.Data) == 0 {
		t.Fatalf("file=%#v err=%v", file, err)
	}
	if _, err := parseChatFileDataURI("data:application/octet-stream;base64,AAEC", "payload.bin", 1<<20); err == nil {
		t.Fatal("unsupported binary file was accepted")
	}
}

func TestRemoteChatImageURLBlocksPrivateNetworks(t *testing.T) {
	for _, value := range []string{"http://example.com/image.png", "https://127.0.0.1/image.png", "https://169.254.169.254/latest/meta-data", "https://[::1]/image.png", "https://[::ffff:127.0.0.1]/image.png"} {
		if _, err := validateRemoteImageURL(context.Background(), value); err == nil {
			t.Fatalf("unsafe image URL accepted: %s", value)
		}
	}
	if value, err := validateRemoteImageURL(context.Background(), "https://8.8.8.8/image.png"); err != nil || value.originalURL.Hostname() != "8.8.8.8" || value.fetchURL.Hostname() != "8.8.8.8" {
		t.Fatalf("public image URL rejected: value=%v err=%v", value, err)
	}
}

func TestRemoteChatImageHeadersNeverLeakCredentials(t *testing.T) {
	headers := remoteImageHeaders("test-agent")
	if headers.Get("User-Agent") != "test-agent" || headers.Get("Cookie") != "" || headers.Get("Authorization") != "" {
		t.Fatalf("remote image headers = %#v", headers)
	}
	fileHeaders := remoteFileHeaders("test-agent")
	if fileHeaders.Get("User-Agent") != "test-agent" || fileHeaders.Get("Cookie") != "" || fileHeaders.Get("Authorization") != "" {
		t.Fatalf("remote file headers = %#v", fileHeaders)
	}
}

func TestChatImageUploadFeedsFileMetadataIntoConversation(t *testing.T) {
	dataURI := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	var uploadUserAgent string
	server := fhttptest.NewServer(fhttp.HandlerFunc(func(writer fhttp.ResponseWriter, request *fhttp.Request) {
		switch request.URL.Path {
		case "/http/upload-file-v2/direct":
			uploadUserAgent = request.Header.Get("User-Agent")
			if !strings.Contains(request.Header.Get("Cookie"), "sso=test-sso") {
				t.Errorf("upload cookie = %q", request.Header.Get("Cookie"))
			}
			if err := request.ParseMultipartForm(2 << 20); err != nil {
				t.Errorf("multipart: %v", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			file, header, err := request.FormFile("file")
			if err != nil {
				t.Errorf("file part: %v", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			defer file.Close()
			content, _ := io.ReadAll(file)
			if header.Filename != "image.png" || header.Header.Get("Content-Type") != "image/png" || len(content) == 0 || request.FormValue("file_source") != "" {
				t.Errorf("upload filename=%q content-type=%q bytes=%d source=%q", header.Filename, header.Header.Get("Content-Type"), len(content), request.FormValue("file_source"))
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"uploadId":"upload_1","fileMetadata":{"fileMetadataId":"file_meta_1","fileUri":"users/test/file_meta_1/content"}}`)
		case "/rest/app-chat/upload-file":
			t.Error("不应调用旧版 Base64 上传接口")
			writer.WriteHeader(http.StatusInternalServerError)
		case "/ws/mgw/":
			if request.Header.Get("User-Agent") != uploadUserAgent {
				t.Errorf("chat user-agent %q differs from upload %q", request.Header.Get("User-Agent"), uploadUserAgent)
			}
			connection, err := (&websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(writer, request, nil)
			if err != nil {
				t.Errorf("upgrade: %v", err)
				return
			}
			defer connection.Close()
			var initial map[string]any
			if err := connection.ReadJSON(&initial); err != nil {
				t.Errorf("read session.create: %v", err)
				return
			}
			initialEvent := initial["event"].(map[string]any)
			initialID := initialEvent["event_id"].(string)
			_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "session.created", "client_event_id": initialID}})
			_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "conversation.attached", "conversation": map[string]any{"id": "conv_1"}}})
			var item map[string]any
			if err := connection.ReadJSON(&item); err != nil {
				t.Errorf("read conversation.item.create: %v", err)
				return
			}
			itemEvent := item["event"].(map[string]any)
			attachments, _ := itemEvent["file_attachment_ids"].([]any)
			if len(attachments) != 1 || attachments[0] != "file_meta_1" {
				t.Errorf("file_attachment_ids = %#v", itemEvent["file_attachment_ids"])
			}
			var create map[string]any
			if err := connection.ReadJSON(&create); err != nil || create["event"].(map[string]any)["type"] != "response.create" {
				t.Errorf("read response.create: value=%#v err=%v", create, err)
				return
			}
			_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "response.chunk", "chunk": map[string]any{"text": map[string]any{"text": "seen", "channel": "CHANNEL_ASSISTANT_RESPONSE"}}}})
			_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "response.done", "response": map[string]any{"id": "parent_1", "status": "completed"}}})
		default:
			fhttp.NotFound(writer, request)
		}
	}))
	defer server.Close()

	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{BaseURL: server.URL}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)
	content, _ := json.Marshal([]any{
		map[string]any{"type": "text", "text": "inspect"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURI}},
	})
	body, _ := json.Marshal(map[string]any{
		"model": "grok-chat-fast", "messages": []any{map[string]any{"role": "user", "content": json.RawMessage(content)}},
	})
	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 1, UserID: "497f19f8-49d4-458a-bee4-43ec3dcaf8ca", EncryptedAccessToken: encrypted}, Method: http.MethodPost,
		Path: "/responses", Body: body, Model: "grok-chat-fast", Operation: "chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	result, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK || !bytes.Contains(result, []byte(`"content":"seen"`)) {
		t.Fatalf("status=%d body=%s err=%v", response.StatusCode, result, err)
	}
}

func TestForwardMessagesWebSearchEndToEnd(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		name := "non_stream"
		if streaming {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			var upstreamMessage string
			server := fhttptest.NewServer(fhttp.HandlerFunc(func(writer fhttp.ResponseWriter, request *fhttp.Request) {
				if request.URL.Path != "/ws/mgw/" {
					fhttp.NotFound(writer, request)
					return
				}
				connection, err := (&websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(writer, request, nil)
				if err != nil {
					t.Errorf("upgrade: %v", err)
					return
				}
				defer connection.Close()
				var initial map[string]any
				if err := connection.ReadJSON(&initial); err != nil {
					t.Errorf("read session.create: %v", err)
					return
				}
				initialID := initial["event"].(map[string]any)["event_id"].(string)
				_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "session.created", "client_event_id": initialID}})
				_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "conversation.attached", "conversation": map[string]any{"id": "conv_1"}}})
				var item map[string]any
				if err := connection.ReadJSON(&item); err != nil {
					t.Errorf("read conversation.item.create: %v", err)
					return
				}
				itemValue := item["event"].(map[string]any)["item"].(map[string]any)
				chunks := itemValue["x_grok"].(map[string]any)["input_chunks"].([]any)
				upstreamMessage, _ = chunks[len(chunks)-1].(map[string]any)["text"].(map[string]any)["text"].(string)
				var create map[string]any
				if err := connection.ReadJSON(&create); err != nil {
					t.Errorf("read response.create: %v", err)
					return
				}
				_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "response.search.result", "result": map[string]any{"url": "https://doc.rust-lang.org", "title": "The Rust Book"}}})
				_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "response.chunk", "chunk": map[string]any{"text": map[string]any{"text": "Here you go.", "channel": "CHANNEL_ASSISTANT_RESPONSE"}}}})
				_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "response.done", "response": map[string]any{"id": "parent_1", "status": "completed"}}})
			}))
			defer server.Close()

			cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
			if err != nil {
				t.Fatal(err)
			}
			encrypted, err := cipher.Encrypt("test-sso")
			if err != nil {
				t.Fatal(err)
			}
			adapter := NewAdapter(Config{BaseURL: server.URL, StatsigMode: "manual"}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)
			body, _ := json.Marshal(map[string]any{
				"model": "public", "max_tokens": 256, "stream": streaming,
				"messages":    []any{map[string]any{"role": "user", "content": "Perform a web search for the query: rust tutorials"}},
				"tools":       []any{map[string]any{"type": "web_search_20250305", "name": "web_search", "max_uses": 8}},
				"tool_choice": map[string]any{"type": "tool", "name": "web_search"},
			})
			response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
				Credential: account.Credential{ID: 1, UserID: "497f19f8-49d4-458a-bee4-43ec3dcaf8ca", EncryptedAccessToken: encrypted}, Method: http.MethodPost,
				Path: "/responses", Body: body, Model: "grok-chat-fast", Operation: conversation.OperationMessages,
				Streaming: streaming,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			result, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(upstreamMessage, "rust tutorials") || strings.Contains(upstreamMessage, "You MUST call at least one available tool") {
				t.Fatalf("upstream message = %q", upstreamMessage)
			}

			if streaming {
				text := string(result)
				useAt := strings.Index(text, `"type":"server_tool_use"`)
				resultAt := strings.Index(text, `"type":"web_search_tool_result"`)
				textAt := strings.Index(text, `"content_block":{"text":"","type":"text"}`)
				if response.StatusCode != http.StatusOK || useAt < 0 || resultAt < 0 || textAt < 0 || !(useAt < resultAt && resultAt < textAt) {
					t.Fatalf("stream status=%d body=%s", response.StatusCode, text)
				}
				if !strings.Contains(text, "rust tutorials") || !strings.Contains(text, `"web_search_requests":1`) || !strings.Contains(text, "The Rust Book") {
					t.Fatalf("stream missing query, usage, or hit: %s", text)
				}
				return
			}

			var payload map[string]any
			if err := json.Unmarshal(result, &payload); err != nil {
				t.Fatal(err)
			}
			content := payload["content"].([]any)
			if response.StatusCode != http.StatusOK || len(content) != 3 || content[0].(map[string]any)["type"] != "server_tool_use" || content[1].(map[string]any)["type"] != "web_search_tool_result" || content[2].(map[string]any)["text"] != "Here you go." {
				t.Fatalf("non-stream status=%d payload=%#v", response.StatusCode, payload)
			}
			use := content[0].(map[string]any)
			if use["input"].(map[string]any)["query"] != "rust tutorials" || content[1].(map[string]any)["tool_use_id"] != use["id"] {
				t.Fatalf("web search linkage = %#v", content)
			}
			hits := content[1].(map[string]any)["content"].([]any)
			if len(hits) != 1 || hits[0].(map[string]any)["title"] != "The Rust Book" || payload["stop_reason"] != "end_turn" {
				t.Fatalf("web search result = %#v", payload)
			}
			usage := payload["usage"].(map[string]any)["server_tool_use"].(map[string]any)
			if usage["web_search_requests"] != float64(1) {
				t.Fatalf("web search usage = %#v", usage)
			}
		})
	}
}

func TestOpenChatScopesStreamIdleTimeoutToTextStreams(t *testing.T) {
	server := fhttptest.NewServer(fhttp.HandlerFunc(func(writer fhttp.ResponseWriter, request *fhttp.Request) {
		if request.URL.Path != "/ws/mgw/" {
			fhttp.NotFound(writer, request)
			return
		}
		connection, err := (&websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{
		BaseURL: server.URL, StatsigMode: "manual", ChatTimeoutSeconds: 5, StreamIdleTimeoutSeconds: 1,
	}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)
	credential := account.Credential{ID: 1, Provider: account.ProviderWeb, UserID: "497f19f8-49d4-458a-bee4-43ec3dcaf8ca", EncryptedAccessToken: encrypted}
	spec, ok := Resolve("grok-chat-fast")
	if !ok {
		t.Fatal("grok-chat-fast spec not found")
	}

	for _, enforceStreamIdle := range []bool{false, true} {
		upstream, lease, _, _, openErr := adapter.openChat(context.Background(), credential, "", spec, normalizedChatInput{Prompt: "hello"}, gatewayOpenOptions{enforceStreamIdle: enforceStreamIdle})
		if openErr != nil {
			t.Fatal(openErr)
		}
		_, wrapped := upstream.Body.(*providerstreamidle.ReadCloser)
		if wrapped != enforceStreamIdle {
			t.Fatalf("stream-idle wrapper present = %t, enforce = %t", wrapped, enforceStreamIdle)
		}
		_ = upstream.Body.Close()
		lease.Release()
	}
}

func TestWebNonStreamingResponseStillProtectsGatewayStream(t *testing.T) {
	server := fhttptest.NewServer(fhttp.HandlerFunc(func(writer fhttp.ResponseWriter, request *fhttp.Request) {
		if request.URL.Path != "/ws/mgw/" {
			fhttp.NotFound(writer, request)
			return
		}
		connection, err := (&websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{
		BaseURL: server.URL, StatsigMode: "manual", ChatTimeoutSeconds: 5, StreamIdleTimeoutSeconds: 1,
	}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)
	body := []byte(`{"model":"grok-chat-fast","input":"hello","stream":false}`)
	_, err = adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 1, Provider: account.ProviderWeb, UserID: "497f19f8-49d4-458a-bee4-43ec3dcaf8ca", EncryptedAccessToken: encrypted},
		Method:     http.MethodPost, Path: "/responses", Model: "grok-chat-fast", Operation: conversation.OperationResponses,
		Body: body, Streaming: false,
	})
	if !errors.Is(err, neterror.ErrUpstreamStreamIdleTimeout) {
		t.Fatalf("ForwardResponse() error = %v, want ErrUpstreamStreamIdleTimeout", err)
	}
}

type egressRepositoryStub struct{}

func (egressRepositoryStub) ListEgressNodes(context.Context, egressdomain.Scope, repository.SortQuery) ([]egressdomain.Node, error) {
	return nil, nil
}

func (egressRepositoryStub) GetEgressNode(context.Context, uint64) (egressdomain.Node, error) {
	return egressdomain.Node{}, errors.New("not found")
}

func (egressRepositoryStub) CreateEgressNode(context.Context, egressdomain.Node) (egressdomain.Node, error) {
	return egressdomain.Node{}, errors.New("unsupported")
}

func (egressRepositoryStub) UpdateEgressNode(context.Context, egressdomain.Node) (egressdomain.Node, error) {
	return egressdomain.Node{}, errors.New("unsupported")
}

func (egressRepositoryStub) DeleteEgressNode(context.Context, uint64) error {
	return errors.New("unsupported")
}

func TestLiteChatRejectsInvalidImageConfigBeforeUpstream(t *testing.T) {
	adapter := &Adapter{}
	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Method: http.MethodPost, Model: "grok-imagine-image", Operation: "chat",
		Body: []byte(`{"model":"grok-imagine-image","messages":[{"role":"user","content":"draw"}],"image_config":{"n":0}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", response.StatusCode)
	}
}

func TestLiteChatStreamFailsBeforeReturningSuccessResponse(t *testing.T) {
	server := fhttptest.NewServer(fhttp.HandlerFunc(func(writer fhttp.ResponseWriter, request *fhttp.Request) {
		if request.URL.Path != "/ws/mgw/" {
			fhttp.NotFound(writer, request)
			return
		}
		connection, err := (&websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(writer, request, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer connection.Close()
		var initial map[string]any
		if err := connection.ReadJSON(&initial); err != nil {
			t.Errorf("read session.create: %v", err)
			return
		}
		initialID := initial["event"].(map[string]any)["event_id"].(string)
		_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "session.created", "client_event_id": initialID}})
		_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "conversation.attached", "conversation": map[string]any{"id": "conv_1"}}})
		for range 2 {
			var event map[string]any
			if err := connection.ReadJSON(&event); err != nil {
				t.Errorf("read turn event: %v", err)
				return
			}
		}
		_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "response.done", "response": map[string]any{"id": "parent_1", "status": "completed"}}})
	}))
	defer server.Close()

	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{BaseURL: server.URL, StatsigMode: "manual"}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, imageAssetStoreStub{})
	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: account.Credential{ID: 1, Provider: account.ProviderWeb, UserID: "497f19f8-49d4-458a-bee4-43ec3dcaf8ca", EncryptedAccessToken: encrypted},
		Method:     http.MethodPost, Path: "/v1/chat/completions", Model: "grok-imagine-image", Operation: conversation.OperationChat,
		Body: []byte(`{"model":"grok-imagine-image-lite","stream":true,"messages":[{"role":"user","content":"draw a cat"}]}`), Streaming: true,
	})
	if err == nil || response != nil || !strings.Contains(err.Error(), "未解析到最终图片") {
		t.Fatalf("response=%#v error=%v", response, err)
	}
}

func TestParseLiteImageCardAttachment(t *testing.T) {
	parsed := &parsedChat{}
	frame := map[string]any{"result": map[string]any{"response": map[string]any{
		"cardAttachment": map[string]any{"jsonData": `{"id":"card_1","image_chunk":{"progress":100,"imageUrl":"generated/image.jpg","moderated":false}}`},
	}}}
	data, _ := json.Marshal(frame)
	kind, delta, err := parseUpstreamFrame(data, parsed)
	if err != nil || kind != "image" || delta != "https://assets.grok.com/generated/image.jpg" || !slices.Equal(parsed.Images, []string{delta}) {
		t.Fatalf("kind=%q delta=%q err=%v", kind, delta, err)
	}
}

func TestParseLiteNestedUsageLimit(t *testing.T) {
	parsed := &parsedChat{}
	frame := []byte(`{"result":{"response":{"error":{"message":"You've reached your usage limit. Please try again later."},"cardAttachment":{"jsonData":"{\"image_chunk\":{\"progress\":100,\"systemErrCode\":\"rate_limit\"}}"}}}}`)
	if _, _, err := parseUpstreamFrame(frame, parsed); !errors.Is(err, errWebUsageLimit) {
		t.Fatalf("error = %v", err)
	}
}

func TestParseLiteImageCardAttachmentVariants(t *testing.T) {
	parsed := &parsedChat{}
	frame := map[string]any{"result": map[string]any{"response": map[string]any{
		"cardAttachments": []any{
			map[string]any{"jsonData": map[string]any{"id": "pending", "imageChunk": map[string]any{"progress": 50, "imageUrl": "generated/partial.jpg"}}},
			map[string]any{"jsonData": map[string]any{"id": "final", "imageChunk": map[string]any{"progress": 100, "image_url": "users/user_1/generated/final/image.jpg", "moderated": false}}},
		},
	}}}
	data, _ := json.Marshal(frame)
	kind, delta, err := parseUpstreamFrame(data, parsed)
	want := "https://assets.grok.com/users/user_1/generated/final/image.jpg"
	if err != nil || kind != "image" || delta != want || !slices.Equal(parsed.Images, []string{want}) {
		t.Fatalf("kind=%q delta=%q images=%#v err=%v", kind, delta, parsed.Images, err)
	}
}

func TestCapturedLiteRenderFileFlowCompletesOnImageCard(t *testing.T) {
	fixture := strings.Join([]string{
		`data: {"result":{"conversation":{"conversationId":"conv_1"}}}`,
		`data: {"result":{"response":{"cardAttachment":{"jsonData":"{\"id\":\"x1Xyo\",\"type\":\"render_file\",\"cardType\":\"generated_image_card\",\"image_chunk\":null}"},"messageTag":"final"}}}`,
		`data: {"result":{"response":{"token":"<grok:render card_id=\"x1Xyo\" card_type=\"generated_image_card\" type=\"render_file\"><argument name=\"file_path\">/home/workdir/artifacts/imagine_images/cat.jpg</argument></grok:render>","isThinking":false,"messageTag":"final"}}}`,
		`data: {"result":{"response":{"cardAttachment":{"jsonData":"{\"id\":\"x1Xyo\",\"type\":\"render_file\",\"cardType\":\"generated_image_card\",\"image_chunk\":{\"imageUuid\":\"image_1\",\"imageUrl\":\"users/user_1/generated/cat/image.jpg\",\"progress\":100,\"moderated\":false}}"},"messageTag":"final"}}}`,
		`data: {"result":{"response":{"token":"这段文本不应影响图片完成","isThinking":false,"messageTag":"final"}}}`,
	}, "\n")
	firstImage := ""
	parsed, err := consumeUpstream(strings.NewReader(fixture), func(kind, delta string) error {
		if kind == "image" {
			firstImage = delta
			return errLiteImageReady
		}
		return nil
	})
	if !errors.Is(err, errLiteImageReady) {
		t.Fatalf("error = %v", err)
	}
	if firstImage != "https://assets.grok.com/users/user_1/generated/cat/image.jpg" || parsed.Text.String() != "" {
		t.Fatalf("image=%q text=%q parsed=%#v", firstImage, parsed.Text.String(), parsed)
	}
}

func TestConsumeUpstreamHandlesConcatenatedImageEditFrames(t *testing.T) {
	fixture := `{"result":{"response":{"streamingImageGenerationResponse":{"imageUrl":"users/test/generated/edit/image.jpg","progress":50}}}}` +
		`{"result":{"response":{"streamingImageGenerationResponse":{"imageUrl":"users/test/generated/edit/image.jpg","progress":100}}}}` +
		`{"result":{"response":{"modelResponse":{"generatedImageUrls":["users/test/generated/edit/image.jpg"]}}}}`
	parsed, err := consumeUpstream(strings.NewReader(fixture), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://assets.grok.com/users/test/generated/edit/image.jpg"
	if !slices.Equal(parsed.Images, []string{want}) {
		t.Fatalf("images = %#v, want %#v", parsed.Images, []string{want})
	}
}

func TestStreamingImageEditRejectsModeratedFinalImage(t *testing.T) {
	parsed := &parsedChat{}
	frame := []byte(`{"result":{"response":{"streamingImageGenerationResponse":{"imageUrl":"users/test/generated/moderated/image.jpg","progress":100,"moderated":true}}}}`)
	kind, delta, err := parseUpstreamFrame(frame, parsed)
	if err != nil || kind != "" || delta != "" || len(parsed.Images) != 0 {
		t.Fatalf("kind=%q delta=%q images=%#v err=%v", kind, delta, parsed.Images, err)
	}
	final := []byte(`{"result":{"response":{"modelResponse":{"generatedImageUrls":["users/test/generated/moderated/image.jpg"]}}}}`)
	if kind, delta, err := parseUpstreamFrame(final, parsed); err != nil || kind != "" || delta != "" || len(parsed.Images) != 0 {
		t.Fatalf("fallback kind=%q delta=%q images=%#v err=%v", kind, delta, parsed.Images, err)
	}
	capture := append(append([]byte(nil), frame...), final...)
	if urls := imageEditResultURLs(parsed, capture); len(urls) != 0 {
		t.Fatalf("moderated capture leaked images: %#v", urls)
	}
}

func TestImageEditRejectsUnconfirmedCountAndResolution(t *testing.T) {
	adapter := &Adapter{}
	for _, request := range []provider.ImageEditRequest{
		{ImageURLs: []string{"data:image/png;base64,AA=="}, Count: 2, Resolution: "1k"},
		{ImageURLs: []string{"data:image/png;base64,AA=="}, Count: 1, Resolution: "2k"},
		{ImageURLs: []string{"data:image/png;base64,AA=="}, Count: 1, Resolution: "1k", PartialImages: 1},
		{ImageURLs: []string{"data:image/png;base64,AA=="}, Count: 1, Resolution: "1k", Streaming: true, PartialImages: 4},
	} {
		response, err := adapter.EditImage(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusBadRequest || !bytes.Contains(body, []byte(`"error"`)) {
			t.Fatalf("status=%d body=%s", response.StatusCode, body)
		}
	}
}

func TestBuildImageEditPayloadMatchesCapturedAspectRatioShape(t *testing.T) {
	payload := buildImageEditPayload("改成兔子", []string{"https://assets.grok.com/users/test/reference/content"}, "post_1", "1:1")
	metadata, _ := payload["responseMetadata"].(map[string]any)
	override, _ := metadata["modelConfigOverride"].(map[string]any)
	modelMap, _ := override["modelMap"].(map[string]any)
	config, _ := modelMap["imageEditModelConfig"].(map[string]any)
	if payload["modelName"] != "imagine-image-edit" || payload["imageGenerationCount"] != 2 || modelMap["imageEditModel"] != "imagine" {
		t.Fatalf("payload = %#v", payload)
	}
	if config["aspectRatio"] != "1:1" || config["parentPostId"] != "post_1" || !slices.Equal(config["imageReferences"].([]string), []string{"https://assets.grok.com/users/test/reference/content"}) {
		t.Fatalf("image edit config = %#v", config)
	}
	withoutRatio := buildImageEditPayload("edit", []string{"reference"}, "post_2", "")
	metadata = withoutRatio["responseMetadata"].(map[string]any)
	override = metadata["modelConfigOverride"].(map[string]any)
	modelMap = override["modelMap"].(map[string]any)
	config = modelMap["imageEditModelConfig"].(map[string]any)
	if _, exists := config["aspectRatio"]; exists {
		t.Fatalf("empty aspect ratio leaked into payload: %#v", config)
	}
}

func TestImageEditAspectRatioSupportsOpenAISize(t *testing.T) {
	for _, test := range []struct {
		aspectRatio string
		size        string
		want        string
	}{
		{aspectRatio: "1:1", size: "1536x1024", want: "1:1"},
		{size: "1024x1024", want: "1:1"},
		{size: "1024x1536", want: "2:3"},
		{size: "1536x1024", want: "3:2"},
		{size: "auto", want: "auto"},
		{want: ""},
	} {
		got, err := resolveImageEditAspectRatio(test.aspectRatio, test.size)
		if err != nil || got != test.want {
			t.Fatalf("aspect=%q size=%q got=%q err=%v", test.aspectRatio, test.size, got, err)
		}
	}
}

func TestParseImageEditStreamFrame(t *testing.T) {
	frame, ok := parseImageEditStreamFrame([]byte(`{"result":{"response":{"streamingImageGenerationResponse":{"imageUrl":"users/test/generated/edit-part-0/image.jpg","progress":50}}}}`))
	if !ok || frame.URL != "https://assets.grok.com/users/test/generated/edit-part-0/image.jpg" || frame.Progress != 50 || frame.Moderated {
		t.Fatalf("partial frame = %#v ok=%t", frame, ok)
	}
	frame, ok = parseImageEditStreamFrame([]byte(`{"result":{"response":{"streamingImageGenerationResponse":{"imageUrl":"users/test/generated/edit/image.jpg","isFinal":true,"moderated":true}}}}`))
	if !ok || frame.Progress != 100 || !frame.Moderated {
		t.Fatalf("final frame = %#v ok=%t", frame, ok)
	}
}

func TestImageEditStreamUsesOfficialOpenAIEvents(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0, 'I', 'H', 'D', 'R'}
	partial := openAIImageEditStreamEvent("image_edit.partial_image", png, 123, "1024x1024", 0)
	if partial["type"] != "image_edit.partial_image" || partial["partial_image_index"] != 0 || partial["usage"] != nil || partial["output_format"] != "png" {
		t.Fatalf("partial event = %#v", partial)
	}
	completed := openAIImageEditStreamEvent("image_edit.completed", []byte("jpeg"), 123, "auto", 0)
	usage, _ := completed["usage"].(map[string]any)
	if completed["type"] != "image_edit.completed" || completed["partial_image_index"] != nil || usage["total_tokens"] != 0 {
		t.Fatalf("completed event = %#v", completed)
	}
	var output bytes.Buffer
	if err := writeSSE(&output, "image_edit.partial_image", partial); err != nil {
		t.Fatal(err)
	}
	if err := writeSSE(&output, "image_edit.completed", completed); err != nil {
		t.Fatal(err)
	}
	value := output.String()
	if !strings.Contains(value, "event: image_edit.partial_image") || !strings.Contains(value, "event: image_edit.completed") || strings.Contains(value, "image_generation.") {
		t.Fatalf("stream = %q", value)
	}
}

func TestExtractCapturedImageURLsPrefersFinalImage(t *testing.T) {
	fixture := []byte(`{"result":{"response":{"streamingImageGenerationResponse":{"imageUrl":"users/test/generated/id-part-0/image.jpg","progress":50}}}}` +
		`{"result":{"response":{"streamingImageGenerationResponse":{"imageUrl":"users/test/generated/id/image.jpg","progress":100}}}}` +
		`{"result":{"response":{"modelResponse":{"generatedImageUrls":["users/test/generated/id/image.jpg"]}}}}`)
	want := []string{"https://assets.grok.com/users/test/generated/id/image.jpg"}
	if got := extractCapturedImageURLs(fixture); !slices.Equal(got, want) {
		t.Fatalf("urls = %#v, want %#v", got, want)
	}
}

func TestExtractCapturedImageURLsHandlesNestedJSONData(t *testing.T) {
	fixture := []byte(`{"result":{"response":{"unknownWrapper":{"jsonData":"{\"image_chunk\":{\"imageUrl\":\"users/test/generated/id-part-0/image.jpg\",\"progress\":50}}"}}}}` +
		`{"result":{"response":{"unknownWrapper":{"jsonData":"{\"image_chunk\":{\"imageUrl\":\"users/test/generated/id/image.jpg\",\"progress\":100,\"moderated\":false}}"}}}}`)
	want := []string{"https://assets.grok.com/users/test/generated/id/image.jpg"}
	if got := extractCapturedImageURLs(fixture); !slices.Equal(got, want) {
		t.Fatalf("urls = %#v, want %#v", got, want)
	}
}

func TestLiteModelResponseCardAttachmentsFallback(t *testing.T) {
	parsed := &parsedChat{}
	frame := map[string]any{"result": map[string]any{"response": map[string]any{
		"modelResponse": map[string]any{
			"generatedImageUrls":  []any{},
			"cardAttachmentsJson": []any{`{"id":"x1Xyo","image_chunk":{"imageUrl":"users/user_1/generated/fallback/image.jpg","progress":100,"moderated":false}}`},
		},
	}}}
	data, _ := json.Marshal(frame)
	kind, delta, err := parseUpstreamFrame(data, parsed)
	if err != nil || kind != "image" || delta != "https://assets.grok.com/users/user_1/generated/fallback/image.jpg" {
		t.Fatalf("kind=%q delta=%q images=%#v err=%v", kind, delta, parsed.Images, err)
	}
}

func TestBuildDirectFileUploadBodyMatchesImagineMultipartProtocol(t *testing.T) {
	raw := []byte("png-binary")
	body, contentType, err := buildDirectFileUploadBody(provider.ImageInput{
		Filename: "测试中文.png", MIMEType: "image/png", Data: raw,
	}, imagineSelfUploadSource)
	if err != nil {
		t.Fatal(err)
	}
	mediaType, parameters, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" || parameters["boundary"] == "" {
		t.Fatalf("content type=%q parameters=%#v err=%v", contentType, parameters, err)
	}
	if !bytes.Contains(body, []byte(`Content-Disposition: form-data; name="file"; filename="测试中文.png"`)) || bytes.Contains(body, []byte("filename*=")) {
		t.Fatalf("multipart disposition does not match browser format: %q", body)
	}
	reader := multipart.NewReader(bytes.NewReader(body), parameters["boundary"])
	filePart, err := reader.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	fileData, err := io.ReadAll(filePart)
	if err != nil || filePart.FormName() != "file" || filePart.FileName() != "测试中文.png" || filePart.Header.Get("Content-Type") != "image/png" || !bytes.Equal(fileData, raw) {
		t.Fatalf("file name=%q filename=%q content-type=%q data=%q err=%v", filePart.FormName(), filePart.FileName(), filePart.Header.Get("Content-Type"), fileData, err)
	}
	sourcePart, err := reader.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	source, err := io.ReadAll(sourcePart)
	if err != nil || sourcePart.FormName() != "file_source" || string(source) != imagineSelfUploadSource {
		t.Fatalf("source name=%q value=%q err=%v", sourcePart.FormName(), source, err)
	}
	if part, err := reader.NextPart(); !errors.Is(err, io.EOF) || part != nil {
		t.Fatalf("unexpected trailing part=%v err=%v", part, err)
	}
}

func TestBrowserMultipartFilenameEscapesAndSanitizesUnsafeValues(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
		want  string
	}{
		{name: "quote and backslash", value: "a\\b\"c.png", want: `a\\b\"c.png`},
		{name: "line breaks", value: "a\r\nb.png", want: "ab.png"},
		{name: "control characters", value: "a\x00\tb.png", want: "a__b.png"},
		{name: "empty after cleanup", value: "\r\n", want: "upload.bin"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := browserMultipartFilename(test.value); got != test.want {
				t.Fatalf("filename = %q, want %q", got, test.want)
			}
		})
	}
}

func TestBuildDirectFileUploadBodySanitizesUnsafeFilename(t *testing.T) {
	body, contentType, err := buildDirectFileUploadBody(provider.ImageInput{
		Filename: "a\\b\"c\r\n\x00.png", MIMEType: "image/png", Data: []byte("png"),
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	_, parameters, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatal(err)
	}
	reader := multipart.NewReader(bytes.NewReader(body), parameters["boundary"])
	part, err := reader.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	// multipart 的 Part.FileName() 内部会过一层 filepath.Base(平台相关):
	// Windows 把 \ 视作路径分隔符,会剥掉 a\ 前缀;Linux 则原样返回。
	// 期望值同样用 filepath.Base 计算,双平台同时成立,断言强度不变。
	if part.FormName() != "file" || part.FileName() != filepath.Base(`a\b"c_.png`) {
		t.Fatalf("form name=%q filename=%q", part.FormName(), part.FileName())
	}
}

func TestBuildDirectFileUploadBodyOmitsSourceForChat(t *testing.T) {
	body, contentType, err := buildDirectFileUploadBody(provider.ImageInput{Filename: "chat.png", MIMEType: "image/png", Data: []byte("png")}, "")
	if err != nil {
		t.Fatal(err)
	}
	_, parameters, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatal(err)
	}
	reader := multipart.NewReader(bytes.NewReader(body), parameters["boundary"])
	part, err := reader.NextPart()
	if err != nil || part.FormName() != "file" {
		t.Fatalf("file part=%v err=%v", part, err)
	}
	if _, err := io.Copy(io.Discard, part); err != nil {
		t.Fatal(err)
	}
	if part, err := reader.NextPart(); !errors.Is(err, io.EOF) || part != nil {
		t.Fatalf("chat upload unexpectedly contains another form field: part=%v err=%v", part, err)
	}
}

func TestVideoReferenceUsesV2DirectUpload(t *testing.T) {
	dataURI := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/http/upload-file-v2/direct" {
			t.Errorf("unexpected upload path %q", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		if err := request.ParseMultipartForm(2 << 20); err != nil {
			t.Errorf("multipart: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		file, header, err := request.FormFile("file")
		if err != nil {
			t.Errorf("file part: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		defer file.Close()
		content, _ := io.ReadAll(file)
		if header.Filename != "image.png" || header.Header.Get("Content-Type") != "image/png" || len(content) == 0 {
			t.Errorf("upload filename=%q content-type=%q bytes=%d", header.Filename, header.Header.Get("Content-Type"), len(content))
		}
		if request.FormValue("file_source") != imagineSelfUploadSource {
			t.Errorf("file_source = %q", request.FormValue("file_source"))
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"uploadId":"upload-1","fileMetadata":{"fileMetadataId":"metadata-1","fileUri":"users/test/reference/content"}}`)
	}))
	defer server.Close()

	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	manager := infraegress.NewManager(egressRepositoryStub{}, cipher)
	lease, err := manager.Acquire(context.Background(), egressdomain.ScopeWeb, "video-reference-test")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	adapter := NewAdapter(Config{BaseURL: server.URL}, manager, cipher, nil, nil)
	uri, err := adapter.prepareVideoReference(context.Background(), adapter.config(), lease, "test-sso", dataURI)
	if err != nil {
		t.Fatal(err)
	}
	if uri != "https://assets.grok.com/users/test/reference/content" {
		t.Fatalf("uri = %q", uri)
	}
}

func TestDecodeDirectFileUploadResponse(t *testing.T) {
	uploaded, err := decodeDirectFileUploadResponse(strings.NewReader(`{
		"uploadId":"upload-1",
		"fileMetadata":{"fileMetadataId":"metadata-1","fileUri":"users/test/reference/content"}
	}`))
	if err != nil || uploaded.ID != "metadata-1" || uploaded.URI != "https://assets.grok.com/users/test/reference/content" {
		t.Fatalf("uploaded=%#v err=%v", uploaded, err)
	}
	uploaded, err = decodeDirectFileUploadResponse(strings.NewReader(`{"uploadId":"upload-1","terminalError":{}}`))
	if err != nil || uploaded.ID != "upload-1" || uploaded.URI != "" {
		t.Fatalf("uploadId-only response: uploaded=%#v err=%v", uploaded, err)
	}
	if _, err := decodeDirectFileUploadResponse(strings.NewReader(`{"uploadId":"upload-1","terminalError":{"message":"rejected"}}`)); err == nil {
		t.Fatal("terminal upload error was accepted")
	}
	if _, err := decodeDirectFileUploadResponse(strings.NewReader(`{"fileMetadata":{}}`)); err == nil {
		t.Fatal("response without any file identifier was accepted")
	}
}

func TestWebMediaStreamErrorRedactsSensitiveValues(t *testing.T) {
	err := webMediaStreamError(map[string]any{
		"message": "Bearer sensitive-token from owner@example.com at https://grok.com/private?token=secret",
	})
	for _, secret := range []string{"sensitive-token", "owner@example.com", "token=secret"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("stream error exposed %q: %v", secret, err)
		}
	}
}

func TestWebMediaUpstreamDiagnosticLogsStageHeadersWithoutBodyPreview(t *testing.T) {
	var output bytes.Buffer
	adapter := &Adapter{logger: slog.New(slog.NewTextHandler(&output, nil))}
	body := []byte(`<html><title>Just a moment...</title><script>window.__cf_chl_token="challenge-secret"</script><p>access_token=secret owner@example.com https://grok.com/private?token=secret</p></html>` + strings.Repeat("A", 1024))
	upstreamErr := newWebMediaUpstreamError(http.StatusForbidden, body, true)
	response := &http.Response{
		StatusCode:    http.StatusForbidden,
		ContentLength: 70000,
		Header: http.Header{
			"Content-Type": []string{"text/html; charset=UTF-8"},
			"Server":       []string{"cloudflare"},
			"Cf-Ray":       []string{"test-ray-SIN"},
		},
	}

	adapter.logWebMediaUpstreamRejection("video_reference_upload", response, upstreamErr)
	logLine := output.String()
	for _, expected := range []string{
		"msg=web_media_upstream_rejected", "stage=video_reference_upload", "status=403",
		"body_truncated=true", "body_prefix_sha256=", "body_kind=html", "cloudflare_challenge=true",
		"content_type=\"text/html; charset=UTF-8\"",
		"server=cloudflare", "cf_ray=test-ray-SIN",
	} {
		if !strings.Contains(logLine, expected) {
			t.Fatalf("log missing %q: %s", expected, logLine)
		}
	}
	for _, secret := range []string{"Just a moment", "challenge-secret", "access_token=secret", "owner@example.com", "token=secret", strings.Repeat("A", 256)} {
		if strings.Contains(logLine, secret) {
			t.Fatalf("log exposed %q: %s", secret, logLine)
		}
	}
}

func TestModelsUseLowestSufficientTierFirst(t *testing.T) {
	adapter := &Adapter{}
	tests := []struct {
		model string
		want  []account.WebTier
	}{
		{model: "grok-chat-fast", want: []account.WebTier{account.WebTierBasic, account.WebTierSuper, account.WebTierHeavy}},
		{model: "grok-chat-auto", want: []account.WebTier{account.WebTierSuper, account.WebTierHeavy}},
		{model: "grok-chat-expert", want: []account.WebTier{account.WebTierSuper, account.WebTierHeavy}},
		{model: "grok-chat-heavy", want: []account.WebTier{account.WebTierHeavy}},
		{model: "grok-imagine-image", want: []account.WebTier{account.WebTierBasic, account.WebTierSuper, account.WebTierHeavy}},
		{model: "grok-imagine-image-quality", want: []account.WebTier{account.WebTierBasic, account.WebTierSuper, account.WebTierHeavy}},
		{model: "imagine-image-edit", want: []account.WebTier{account.WebTierSuper, account.WebTierHeavy}},
		{model: "grok-imagine-video", want: []account.WebTier{account.WebTierSuper, account.WebTierHeavy}},
	}
	for _, test := range tests {
		got := adapter.TierOrder(test.model)
		if !slices.Equal(got, test.want) {
			t.Fatalf("tier order for %s = %v, want %v", test.model, got, test.want)
		}
	}
}

func TestOnlyChatModelsExposeRateLimitModes(t *testing.T) {
	for _, spec := range Catalog() {
		if spec.Capability == modeldomain.CapabilityChat {
			if !slices.Contains([]string{"auto", "fast", "expert", "heavy"}, spec.Mode) {
				t.Fatalf("chat model %s has invalid quota mode %q", spec.PublicID, spec.Mode)
			}
			continue
		}
		if spec.ProtocolModel == "imagine-lite" {
			if spec.Mode != "fast" {
				t.Fatalf("Lite image must use fast quota mode, got %q", spec.Mode)
			}
			continue
		}
		if isImagineQuotaMode(spec.Mode) {
			continue
		}
		if spec.Mode != "" {
			t.Fatalf("media model %s must not expose chat quota mode %q", spec.PublicID, spec.Mode)
		}
	}
}

func TestQuotaRefreshGroupSeparatesImagineEndpointFromLiteChatQuota(t *testing.T) {
	adapter := &Adapter{}
	if got := adapter.QuotaRefreshGroup("grok-imagine-image"); got != "" {
		t.Fatalf("lite refresh group = %q", got)
	}
	for _, model := range []string{"grok-imagine-image-quality", "imagine-image-edit", "grok-imagine-video"} {
		if got := adapter.QuotaRefreshGroup(model); got != account.QuotaGroupWebImagine {
			t.Fatalf("QuotaRefreshGroup(%q) = %q", model, got)
		}
	}
}

func TestConsumeUpstreamChatFixture(t *testing.T) {
	fixture := strings.Join([]string{
		`data: {"result":{"conversation":{"conversationId":"conv_1"}}}`,
		`data: {"result":{"response":{"userResponse":{"responseId":"up_1"}}}}`,
		`data: {"result":{"response":{"token":"thinking ","isThinking":true,"messageTag":"analysis"}}}`,
		`data: {"result":{"response":{"token":"hello","isThinking":false,"messageTag":"final","webSearchResults":{"results":[{"url":"https://example.com"}]}}}}`,
		`data: [DONE]`,
	}, "\n")
	parsed, err := consumeUpstream(strings.NewReader(fixture), nil)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ConversationID != "conv_1" || parsed.ParentID != "up_1" || parsed.Reasoning.String() != "thinking " || parsed.Text.String() != "hello" || len(parsed.SearchSources) != 1 {
		t.Fatalf("parsed = %#v, text=%q reasoning=%q", parsed, parsed.Text.String(), parsed.Reasoning.String())
	}
}

func TestWebVisibleStreamPhaseSuppressesLateReasoning(t *testing.T) {
	phase := webVisibleStreamPhase{}
	tests := []struct {
		kind  string
		delta string
		want  bool
	}{
		{kind: "reasoning", delta: "first thought", want: true},
		{kind: "text", delta: "", want: false},
		{kind: "reasoning", delta: "continued thought", want: true},
		{kind: "text", delta: "answer", want: true},
		{kind: "reasoning", delta: "late thought", want: false},
		{kind: "text", delta: " continued", want: true},
	}
	for index, test := range tests {
		if got := phase.Allow(test.kind, test.delta); got != test.want {
			t.Fatalf("step %d Allow(%q, %q) = %v, want %v", index, test.kind, test.delta, got, test.want)
		}
	}
}

func TestPreflightRejectsInBandErrorBeforeStreaming(t *testing.T) {
	source := io.NopCloser(strings.NewReader(`data: {"error":{"message":"rate limited","code":8}}` + "\n"))
	if _, err := preflightUpstream(source); err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("error = %v", err)
	}
}

func TestPreflightClassifiesAntiBotRejection(t *testing.T) {
	source := io.NopCloser(strings.NewReader(`{"error":{"message":"Request rejected by anti-bot rules.","code":7,"details":[]}}` + "\n"))
	if _, err := preflightUpstream(source); !errors.Is(err, errWebAntiBot) {
		t.Fatalf("error = %v", err)
	}
}

func TestImagineRequestContainsOnlyProtocolProperties(t *testing.T) {
	message := imagineRequestMessage("request", "prompt", "16:9", false, true, 8)
	item := message["item"].(map[string]any)
	content := item["content"].([]any)[0].(map[string]any)
	properties := content["properties"].(map[string]any)
	if properties["aspect_ratio"] != "16:9" || properties["enable_pro"] != true || properties["enable_nsfw"] != false || properties["num_generations"] != 8 {
		t.Fatalf("properties = %#v", properties)
	}
	encoded := string(MarshalJSONBytes(message))
	assertForbiddenFieldsAbsent(t, encoded)
}

func TestImagineResolutionAndBatchMapping(t *testing.T) {
	tests := []struct {
		resolution string
		count      int
		pro        bool
		batch      int
	}{
		{resolution: "1k", count: 1, batch: 4},
		{resolution: "1k", count: 4, batch: 4},
		{resolution: "1k", count: 5, batch: 8},
		{resolution: "2k", count: 8, pro: true, batch: 8},
		{resolution: "2k", count: 9, pro: true, batch: 12},
		{resolution: "2k", count: 10, pro: true, batch: 12},
	}
	for _, test := range tests {
		config, ok := resolveImagineModel("imagine", test.resolution, test.count)
		if !ok || config.Pro != test.pro || config.NativeBatchSize != test.batch || config.MaxReturnCount != 10 {
			t.Fatalf("resolution=%s count=%d config=%#v", test.resolution, test.count, config)
		}
	}
	config, _ := resolveImagineModel("imagine", "1k", 1)
	if got := imagineUpstreamGenerationCount(true, 1, config); got != 1 {
		t.Fatalf("streaming upstream count = %d, want 1", got)
	}
	if got := imagineUpstreamGenerationCount(false, 1, config); got != 4 {
		t.Fatalf("non-streaming upstream count = %d, want 4", got)
	}
}

func TestImageStreamingRejectsMultipleOutputs(t *testing.T) {
	response, err := (&Adapter{}).GenerateImage(context.Background(), provider.ImageGenerationRequest{
		Model: "grok-imagine-image-quality", Prompt: "cat", Count: 2, Streaming: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		t.Fatalf("body=%s", body)
	}
	errorValue, _ := payload["error"].(map[string]any)
	if response.StatusCode != http.StatusBadRequest || errorValue["message"] != "Streaming is only supported with n=1." || errorValue["type"] != "image_generation_user_error" || errorValue["param"] != "input" || errorValue["code"] != "unsupported_parameter" {
		t.Fatalf("status=%d error=%#v", response.StatusCode, errorValue)
	}
}

func TestImageAspectRatioFollowsXAIContractAndSizeAlias(t *testing.T) {
	for input, expected := range map[string]string{"auto": "auto", "19.5:9": "19.5:9", "9:20": "9:20", "1536x1024": "3:2", "1024x1536": "2:3"} {
		got, err := resolveImageAspectRatio(input, "")
		if err != nil || got != expected {
			t.Fatalf("aspect ratio %q = %q, err=%v", input, got, err)
		}
	}
	if got, err := resolveImageAspectRatio("", "1024x1024"); err != nil || got != "1:1" {
		t.Fatalf("size alias = %q, err=%v", got, err)
	}
	if _, err := resolveImageAspectRatio("7:5", ""); err == nil {
		t.Fatal("unsupported aspect ratio accepted")
	}
}

func TestImagineCollectorHandlesOutOfOrderFrames(t *testing.T) {
	collector := newImagineCollector()
	collector.Accept(map[string]any{"type": "json", "current_status": "completed", "image_id": "image-b", "order": 2.0, "moderated": false})
	collector.Accept(map[string]any{"type": "image", "id": "image-a", "grid_index": 0.0, "url": "https://imagine-public.x.ai/imagine-public/images/image-a.jpg"})
	collector.Accept(map[string]any{"type": "json", "current_status": "completed", "image_id": "image-a", "moderated": false})
	if collector.Done(2) {
		t.Fatal("collector completed before delayed image payload arrived")
	}
	collector.Accept(map[string]any{"type": "image", "id": "image-b", "grid_index": 1.0, "url": "https://imagine-public.x.ai/imagine-public/images/image-b.jpg"})
	if !collector.Done(2) {
		t.Fatal("collector did not complete after both image payloads arrived")
	}
	images := collector.Images()
	if len(images) != 2 || images[0].ID != "image-a" || images[1].ID != "image-b" {
		t.Fatalf("images = %#v", images)
	}
	ready := collector.ReadyImages()
	if len(ready) != 2 || len(collector.ReadyImages()) != 0 {
		t.Fatalf("ready images emitted more than once: %#v", ready)
	}
}

func TestImagineCollectorIgnoresWebSocketPreviewFrames(t *testing.T) {
	collector := newImagineCollector()
	collector.Accept(map[string]any{
		"type": "image", "id": "image-a", "order": 0.0,
		"percentage_complete": 50.0,
		"url":                 "https://imagine-public.x.ai/imagine-public/images/image-a.png",
		"blob":                "preview",
	})
	previews := collector.ReadyPreviews()
	if len(previews) != 1 || previews[0].ID != "image-a" || previews[0].Blob != "preview" || len(collector.ReadyPreviews()) != 0 {
		t.Fatalf("previews = %#v", previews)
	}
	collector.Accept(map[string]any{
		"type": "json", "current_status": "completed", "image_id": "image-a",
		"order": 0.0, "moderated": false,
	})
	if collector.Done(1) || collector.UsableCount() != 0 || len(collector.Images()) != 0 {
		t.Fatalf("preview became final: %#v", collector)
	}
	collector.Accept(map[string]any{
		"type": "image", "id": "image-a", "order": 0.0,
		"percentage_complete": 100.0,
		"url":                 "https://imagine-public.x.ai/imagine-public/images/image-a.jpg",
		"blob":                "final",
	})
	if !collector.Done(1) || collector.UsableCount() != 1 {
		t.Fatalf("final image not recognized: %#v", collector)
	}
	images := collector.Images()
	if len(images) != 1 || images[0].URL != "https://imagine-public.x.ai/imagine-public/images/image-a.jpg" || images[0].Blob != "final" {
		t.Fatalf("images = %#v", images)
	}
}

func TestImagineCollectorKeepsInterleavedJobsIsolated(t *testing.T) {
	collector := newImagineCollector()
	collector.Accept(map[string]any{
		"type": "image", "id": "image-a", "order": 0.0, "percentage_complete": 50.0, "blob": "a-preview",
	})
	collector.Accept(map[string]any{
		"type": "image", "id": "image-b", "order": 1.0, "percentage_complete": 50.0, "blob": "b-preview",
	})
	previews := collector.ReadyPreviews()
	if len(previews) != 2 || previews[0].ID != "image-a" || previews[0].Blob != "a-preview" || previews[1].ID != "image-b" || previews[1].Blob != "b-preview" {
		t.Fatalf("previews = %#v", previews)
	}
	collector.Accept(map[string]any{
		"type": "image", "id": "image-b", "order": 1.0, "percentage_complete": 100.0, "blob": "b-final",
	})
	collector.Accept(map[string]any{"type": "json", "current_status": "completed", "image_id": "image-b", "order": 1.0, "moderated": false})
	ready := collector.ReadyImages()
	if len(ready) != 1 || ready[0].ID != "image-b" || ready[0].Blob != "b-final" {
		t.Fatalf("first ready = %#v", ready)
	}
	collector.Accept(map[string]any{
		"type": "image", "id": "image-a", "order": 0.0, "percentage_complete": 100.0, "blob": "a-final",
	})
	collector.Accept(map[string]any{"type": "json", "current_status": "completed", "image_id": "image-a", "order": 0.0, "moderated": false})
	ready = collector.ReadyImages()
	if len(ready) != 1 || ready[0].ID != "image-a" || ready[0].Blob != "a-final" {
		t.Fatalf("second ready = %#v", ready)
	}
}

func TestImagineCollectorCanReturnRequestedSubsetBeforeNativeBatchCompletes(t *testing.T) {
	collector := newImagineCollector()
	for index := 0; index < 3; index++ {
		id := fmt.Sprintf("image-%d", index)
		collector.Accept(map[string]any{
			"type": "image", "id": id, "order": float64(index), "percentage_complete": 100.0,
			"url": "https://imagine-public.x.ai/imagine-public/images/" + id + ".jpg",
		})
		collector.Accept(map[string]any{
			"type": "json", "current_status": "completed", "image_id": id,
			"order": float64(index), "moderated": false,
		})
	}
	if collector.Done(4) || collector.UsableCount() != 3 {
		t.Fatalf("collector=%#v usable=%d", collector, collector.UsableCount())
	}
	if collector.UsableCount() < 2 {
		t.Fatal("a request for two images should already be satisfiable")
	}
}

func TestImagineCollectorSettlesModeratedSlots(t *testing.T) {
	collector := newImagineCollector()
	collector.Accept(map[string]any{"type": "json", "current_status": "completed", "image_id": "blocked", "moderated": true})
	if !collector.Done(1) || len(collector.Images()) != 0 {
		t.Fatalf("moderated collector = %#v", collector)
	}
}

func TestGeneratedImageAssetHostsRemainStrict(t *testing.T) {
	if !trustedImageAssetHost("assets.grok.com") || !trustedImageAssetHost("imagine-public.x.ai") || !trustedImageAssetHost("imgen.x.ai") || trustedImageAssetHost("example.com") {
		t.Fatal("generated image host allowlist is incorrect")
	}
}

func TestImageStreamUsesOfficialOpenAIEventsWithoutTokenUsage(t *testing.T) {
	adapter := &Adapter{assets: imageAssetStoreStub{}}
	urlItem, err := adapter.imageDataItem(context.Background(), account.Credential{}, imagineImageValue{URL: "https://imgen.x.ai/image.jpg", Blob: "aW1hZ2U="}, "url")
	if err != nil || urlItem["url"] != "https://api.example/v1/media/images/img_test" || urlItem["mime_type"] != "image/jpeg" || urlItem["revised_prompt"] != "" {
		t.Fatalf("url item = %#v, err=%v", urlItem, err)
	}
	b64Item, err := adapter.imageDataItem(context.Background(), account.Credential{}, imagineImageValue{Blob: "aW1hZ2U="}, "b64_json")
	if err != nil || b64Item["b64_json"] != "aW1hZ2U=" || b64Item["mime_type"] != "image/jpeg" {
		t.Fatalf("base64 item = %#v, err=%v", b64Item, err)
	}
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0, 'I', 'H', 'D', 'R'}
	partial := openAIImageStreamEvent("image_generation.partial_image", imagineImageValue{ID: "image-a", Width: 960, Height: 960}, png, 0)
	if partial["type"] != "image_generation.partial_image" || partial["partial_image_index"] != 0 || partial["size"] != "960x960" || partial["output_format"] != "png" || partial["usage"] != nil {
		t.Fatalf("partial event = %#v", partial)
	}
	completed := openAIImageStreamEvent("image_generation.completed", imagineImageValue{ID: "image-a", Width: 960, Height: 960}, []byte("jpeg"), 0)
	if completed["type"] != "image_generation.completed" || completed["partial_image_index"] != nil || completed["quality"] != "auto" || completed["usage"] != nil {
		t.Fatalf("completed event = %#v", completed)
	}
	var output bytes.Buffer
	if err := writeSSE(&output, "image_generation.partial_image", partial); err != nil {
		t.Fatal(err)
	}
	value := output.String()
	if !strings.Contains(value, "event: image_generation.partial_image") || strings.Contains(value, "image_generation.started") || strings.Contains(value, "image_generation.image.completed") || strings.Contains(value, `"usage"`) {
		t.Fatalf("stream event = %q", value)
	}
}

func TestImageDataItemRetriesStorageWithoutRegenerating(t *testing.T) {
	store := &imageAssetStoreRetryStub{failures: 2}
	adapter := &Adapter{assets: store}
	item, err := adapter.imageDataItem(context.Background(), account.Credential{ID: 42}, imagineImageValue{Blob: "aW1hZ2U="}, "url")
	if err != nil {
		t.Fatal(err)
	}
	if store.calls != mediaOutputAttempts || item["url"] != "https://api.example/v1/media/images/img_retry" {
		t.Fatalf("storage retry calls=%d item=%#v", store.calls, item)
	}
}

func TestImageDataItemClassifiesExhaustedStorageFailure(t *testing.T) {
	store := &imageAssetStoreRetryStub{failures: mediaOutputAttempts}
	adapter := &Adapter{assets: store}
	_, err := adapter.imageDataItem(context.Background(), account.Credential{ID: 42}, imagineImageValue{Blob: "aW1hZ2U="}, "url")
	if err == nil || !provider.IsMediaPostProcessingError(err) || store.calls != mediaOutputAttempts {
		t.Fatalf("storage failure err=%v calls=%d", err, store.calls)
	}
	var processingErr *provider.MediaPostProcessingError
	if !errors.As(err, &processingErr) || processingErr.Stage != provider.MediaPostProcessingStorage {
		t.Fatalf("storage failure classification = %#v", processingErr)
	}
}

type imageAssetStoreStub struct{}

func (imageAssetStoreStub) SaveImage(context.Context, []byte) (mediadomain.Asset, error) {
	return mediadomain.Asset{ID: "img_test", MIMEType: "image/jpeg"}, nil
}

func (imageAssetStoreStub) PublicImageURL(string) string {
	return "https://api.example/v1/media/images/img_test"
}

type imageAssetStoreRetryStub struct {
	failures int
	calls    int
}

func (s *imageAssetStoreRetryStub) SaveImage(context.Context, []byte) (mediadomain.Asset, error) {
	s.calls++
	if s.calls <= s.failures {
		return mediadomain.Asset{}, errors.New("temporary storage failure")
	}
	return mediadomain.Asset{ID: "img_retry", MIMEType: "image/jpeg"}, nil
}

func (*imageAssetStoreRetryStub) PublicImageURL(string) string {
	return "https://api.example/v1/media/images/img_retry"
}

func TestParseVideoStreamFixture(t *testing.T) {
	fixture := `data: {"result":{"response":{"streamingVideoGenerationResponse":{"progress":42,"videoPostId":"post_1"}}}}` + "\n" +
		`data: {"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoPostId":"post_1","videoUrl":"/videos/final.mp4"}}}}` + "\n"
	response := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(fixture))}
	progress := 0
	result, postID, err := parseVideoStream(response, func(value int) { progress = value })
	if err != nil {
		t.Fatal(err)
	}
	if progress != 100 || postID != "post_1" || result.URL != "https://assets.grok.com/videos/final.mp4" || result.ContentType != "video/mp4" {
		t.Fatalf("result = %#v, post = %q, progress = %d", result, postID, progress)
	}
}

func TestParseVideoStreamPreservesUpstreamStatus(t *testing.T) {
	response := &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader("limited"))}
	_, _, err := parseVideoStream(response, nil)
	status, ok := provider.ErrorHTTPStatus(err)
	if !ok || status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, ok = %v, err = %v", status, ok, err)
	}
}

func TestGenerateVideoClassifiesOnlyExplicitHTTPRejectionAsCreateFailure(t *testing.T) {
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/rest/media/post/create":
			_, _ = io.WriteString(writer, `{"post":{"id":"post_1"}}`)
		case "/rest/app-chat/conversations/new":
			writer.WriteHeader(status)
			if status == http.StatusOK {
				_, _ = io.WriteString(writer, `{}`)
			} else {
				_, _ = io.WriteString(writer, `{"error":"rate limited"}`)
			}
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encryptedToken, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	manager := infraegress.NewManager(egressRepositoryStub{}, cipher)
	adapter := NewAdapter(Config{BaseURL: server.URL, StatsigMode: "manual", StatsigManualValue: "test"}, manager, cipher, nil, nil)
	request := provider.VideoRequest{
		Credential: account.Credential{ID: 1, Provider: account.ProviderWeb, EncryptedAccessToken: encryptedToken},
		Prompt:     "test",
		Duration:   5,
	}

	_, err = adapter.GenerateVideo(context.Background(), request)
	if stage, ok := provider.VideoErrorStage(err); !ok || stage != provider.VideoStagePoll {
		t.Fatalf("successful response without a completed video stage = %q, ok=%t, err=%v", stage, ok, err)
	}
	status = http.StatusTooManyRequests
	_, err = adapter.GenerateVideo(context.Background(), request)
	if stage, ok := provider.VideoErrorStage(err); !ok || stage != provider.VideoStageCreate {
		t.Fatalf("explicit HTTP rejection stage = %q, ok=%t, err=%v", stage, ok, err)
	}
	status = http.StatusInternalServerError
	_, err = adapter.GenerateVideo(context.Background(), request)
	if stage, ok := provider.VideoErrorStage(err); !ok || stage != provider.VideoStageSubmitted {
		t.Fatalf("server failure stage = %q, ok=%t, err=%v", stage, ok, err)
	}
}

func TestParseVideoConcatenatedJSONFixture(t *testing.T) {
	fixture := `{"result":{"conversation":{"conversationId":"conversation_1"}}}` +
		`{"result":{"response":{"streamingVideoGenerationResponse":{"videoId":"video_1","progress":1,"videoPostId":"post_1","resolutionName":"720p"}}}}` +
		`{"result":{"response":{"streamingVideoGenerationResponse":{"videoId":"video_1","progress":95,"videoPostId":"post_1"}}}}` +
		`{"result":{"response":{"streamingVideoGenerationResponse":{"videoId":"video_1","progress":100,"assetId":"video_1","videoPostId":"post_1","videoUrl":"users/user_1/generated/video_1/generated_video.mp4","thumbnailImageUrl":"users/user_1/generated/video_1/preview_image.jpg","moderated":false}}}}` +
		`{"result":{"response":{"token":"I generated a video","isSoftStop":true}}}`
	response := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(fixture))}
	var values []int
	result, postID, err := parseVideoStream(response, func(value int) { values = append(values, value) })
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(values, []int{1, 95, 100}) || postID != "post_1" || result.URL != "https://assets.grok.com/users/user_1/generated/video_1/generated_video.mp4" || result.ContentType != "video/mp4" {
		t.Fatalf("result = %#v, post = %q, progress = %#v", result, postID, values)
	}
}

func TestParseVideoStreamUsesModelResponseAttachment(t *testing.T) {
	fixture := `data: {"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoPostId":"post_1"},"modelResponse":{"fileAttachments":["users/user_1/generated/video_1/generated_video.mp4"]}}}}` + "\n"
	response := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(fixture))}
	result, postID, err := parseVideoStream(response, nil)
	if err != nil {
		t.Fatal(err)
	}
	if postID != "post_1" || result.URL != "https://assets.grok.com/users/user_1/generated/video_1/generated_video.mp4" || result.ContentType != "video/mp4" {
		t.Fatalf("result = %#v, post = %q", result, postID)
	}
}

func MarshalJSONBytes(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}
