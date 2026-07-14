package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestCatalogMatchesSupportedSurface(t *testing.T) {
	values := Catalog()
	if len(values) != 8 {
		t.Fatalf("catalog size = %d", len(values))
	}
	publicIDs := make(map[string]struct{}, len(values))
	upstreamIDs := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, exists := publicIDs[value.PublicID]; exists {
			t.Fatalf("duplicate public model: %s", value.PublicID)
		}
		if _, exists := upstreamIDs[value.UpstreamModel]; exists {
			t.Fatalf("duplicate route upstream model: %s", value.UpstreamModel)
		}
		publicIDs[value.PublicID] = struct{}{}
		upstreamIDs[value.UpstreamModel] = struct{}{}
	}
	for _, required := range []string{"grok-chat-fast", "grok-chat-auto", "grok-chat-expert", "grok-chat-heavy", "grok-imagine-image", "grok-imagine-image-quality", "grok-imagine-image-edit", "grok-imagine-video"} {
		if _, exists := publicIDs[required]; !exists {
			t.Fatalf("missing supported model: %s", required)
		}
	}
	for _, removed := range []string{"grok-imagine-image-lite", "grok-imagine-image-speed", "grok-imagine-image-pro"} {
		if _, exists := publicIDs[removed]; exists {
			t.Fatalf("obsolete image model remains: %s", removed)
		}
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

func TestBuildWebChatPayloadMatchesCurrentConversationProtocol(t *testing.T) {
	payload := buildWebChatPayload("你好", "auto", []string{"file_1"})
	if payload["modeId"] != "auto" || payload["temporary"] != true || payload["disableMemory"] != true {
		t.Fatalf("payload protocol fields = %#v", payload)
	}
	attachments, ok := payload["fileAttachments"].([]string)
	if !ok || !slices.Equal(attachments, []string{"file_1"}) {
		t.Fatalf("fileAttachments = %#v", payload["fileAttachments"])
	}
	if _, ok := payload["disabledConnectorIds"]; !ok {
		t.Fatal("payload missing disabledConnectorIds")
	}
	device, ok := payload["deviceEnvInfo"].(map[string]any)
	if !ok || device["screenWidth"] != 2056 || device["screenHeight"] != 1328 || device["viewportWidth"] != 2056 || device["viewportHeight"] != 1083 {
		t.Fatalf("deviceEnvInfo = %#v", payload["deviceEnvInfo"])
	}
	for _, obsolete := range []string{"connectors", "searchAllConnectors", "toolOverrides"} {
		if _, ok := payload[obsolete]; ok {
			t.Fatalf("payload contains obsolete field %q", obsolete)
		}
	}
	encoded := string(MarshalJSONBytes(payload))
	assertForbiddenFieldsAbsent(t, encoded)
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
	if value.Prompt != "[user]\n描述这张图" || !slices.Equal(value.Images, []string{dataURI}) {
		t.Fatalf("normalized input = %#v", value)
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
	if value.Prompt != "[user]\nwhat is this" || !slices.Equal(value.Images, []string{dataURI}) {
		t.Fatalf("normalized responses input = %#v", value)
	}
}

func TestNormalizeResponsesInputFileFailsExplicitly(t *testing.T) {
	input, _ := json.Marshal([]any{map[string]any{
		"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_file", "file_url": "https://example.com/a.pdf"},
		},
	}})
	_, err := normalizeOpenAIInput(openAIRequest{Input: input}, "responses")
	if err == nil || !strings.Contains(err.Error(), "input_file") {
		t.Fatalf("error = %v", err)
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

func TestRemoteChatImageURLBlocksPrivateNetworks(t *testing.T) {
	for _, value := range []string{"http://example.com/image.png", "https://127.0.0.1/image.png", "https://169.254.169.254/latest/meta-data", "https://[::1]/image.png", "https://[::ffff:127.0.0.1]/image.png"} {
		if _, err := validateRemoteImageURL(context.Background(), value); err == nil {
			t.Fatalf("unsafe image URL accepted: %s", value)
		}
	}
	if value, err := validateRemoteImageURL(context.Background(), "https://8.8.8.8/image.png"); err != nil || value.Hostname() != "8.8.8.8" {
		t.Fatalf("public image URL rejected: value=%v err=%v", value, err)
	}
}

func TestRemoteChatImageHeadersNeverLeakCredentials(t *testing.T) {
	headers := remoteImageHeaders("test-agent")
	if headers.Get("User-Agent") != "test-agent" || headers.Get("Cookie") != "" || headers.Get("Authorization") != "" {
		t.Fatalf("remote image headers = %#v", headers)
	}
}

func TestChatImageUploadFeedsFileMetadataIntoConversation(t *testing.T) {
	dataURI := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
	var uploadUserAgent string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/rest/app-chat/upload-file":
			uploadUserAgent = request.Header.Get("User-Agent")
			if !strings.Contains(request.Header.Get("Cookie"), "sso=test-sso") {
				t.Errorf("upload cookie = %q", request.Header.Get("Cookie"))
			}
			var payload struct {
				FileName string `json:"fileName"`
				MIMEType string `json:"fileMimeType"`
				Content  string `json:"content"`
			}
			if json.NewDecoder(request.Body).Decode(&payload) != nil || payload.FileName != "image.png" || payload.MIMEType != "image/png" || payload.Content == "" {
				t.Errorf("upload payload = %#v", payload)
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"fileMetadataId":"file_meta_1","fileUri":"https://assets.grok.com/file.png"}`)
		case "/rest/app-chat/conversations/new":
			if request.Header.Get("User-Agent") != uploadUserAgent {
				t.Errorf("chat user-agent %q differs from upload %q", request.Header.Get("User-Agent"), uploadUserAgent)
			}
			var payload map[string]any
			if json.NewDecoder(request.Body).Decode(&payload) != nil {
				t.Error("chat payload is invalid JSON")
			}
			attachments, _ := payload["fileAttachments"].([]any)
			if len(attachments) != 1 || attachments[0] != "file_meta_1" {
				t.Errorf("fileAttachments = %#v", payload["fileAttachments"])
			}
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(writer, "data: {\"result\":{\"conversation\":{\"conversationId\":\"conv_1\"}}}\n")
			_, _ = io.WriteString(writer, "data: {\"result\":{\"response\":{\"userResponse\":{\"responseId\":\"parent_1\"}}}}\n")
			_, _ = io.WriteString(writer, "data: {\"result\":{\"response\":{\"token\":\"seen\",\"isThinking\":false,\"messageTag\":\"final\"}}}\n")
			_, _ = io.WriteString(writer, "data: [DONE]\n")
		default:
			http.NotFound(writer, request)
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
		Credential: account.Credential{ID: 1, EncryptedAccessToken: encrypted}, Method: http.MethodPost,
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

func TestChatModelsUseLowestSufficientTierFirst(t *testing.T) {
	adapter := &Adapter{}
	tests := []struct {
		model string
		want  []account.WebTier
	}{
		{model: "grok-chat-fast", want: []account.WebTier{account.WebTierBasic, account.WebTierSuper, account.WebTierHeavy}},
		{model: "grok-chat-auto", want: []account.WebTier{account.WebTierSuper, account.WebTierHeavy}},
		{model: "grok-chat-expert", want: []account.WebTier{account.WebTierSuper, account.WebTierHeavy}},
		{model: "grok-chat-heavy", want: []account.WebTier{account.WebTierHeavy}},
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
		if spec.Mode != "" {
			t.Fatalf("media model %s must not expose chat quota mode %q", spec.PublicID, spec.Mode)
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

func TestImageStreamExtensionEventsAndPayloads(t *testing.T) {
	adapter := &Adapter{assets: imageAssetStoreStub{}}
	urlItem, err := adapter.imageDataItem(context.Background(), account.Credential{}, imagineImageValue{URL: "https://imgen.x.ai/image.jpg", Blob: "aW1hZ2U="}, "url")
	if err != nil || urlItem["url"] != "https://api.example/v1/media/images/img_test" || urlItem["mime_type"] != "image/jpeg" || urlItem["revised_prompt"] != "" {
		t.Fatalf("url item = %#v, err=%v", urlItem, err)
	}
	b64Item, err := adapter.imageDataItem(context.Background(), account.Credential{}, imagineImageValue{Blob: "aW1hZ2U="}, "b64_json")
	if err != nil || b64Item["b64_json"] != "aW1hZ2U=" || b64Item["mime_type"] != "image/jpeg" {
		t.Fatalf("base64 item = %#v, err=%v", b64Item, err)
	}
	var output bytes.Buffer
	writeImagineStreamFailure(&output, "imggen_test", "upstream_error", "generation failed")
	value := output.String()
	if !strings.Contains(value, "event: image_generation.failed") || !strings.Contains(value, `"id":"imggen_test"`) || !strings.Contains(value, `"code":"upstream_error"`) {
		t.Fatalf("failure event = %q", value)
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

func MarshalJSONBytes(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}
