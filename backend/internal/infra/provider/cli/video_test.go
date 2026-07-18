package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestBuildVideoCreatePayloadNoImageAndSingleR2URL(t *testing.T) {
	noImage, err := videoCreatePayload(provider.VideoRequest{
		Prompt: "animate waves", Duration: 6, AspectRatio: "16:9", Resolution: "720p",
	}, "", buildVideoRequestProfile)
	if err != nil {
		t.Fatal(err)
	}
	if noImage["model"] != buildVideoModel || noImage["prompt"] != "animate waves" || noImage["duration"] != 6 {
		t.Fatalf("no-image payload = %#v", noImage)
	}
	if _, exists := noImage["image"]; exists {
		t.Fatalf("unexpected image field: %#v", noImage)
	}
	if _, exists := noImage["output"]; exists {
		t.Fatalf("primary payload must not include output: %#v", noImage)
	}

	withImage, err := videoCreatePayload(provider.VideoRequest{
		Prompt: "animate", Duration: 6, AspectRatio: "16:9", Resolution: "720p",
		ReferenceURLs: []string{"https://cdn.example.com/r2/first.png"},
	}, "", buildVideoRequestProfile)
	if err != nil {
		t.Fatal(err)
	}
	image, ok := withImage["image"].(map[string]any)
	if !ok || image["image_url"] != "https://cdn.example.com/r2/first.png" {
		t.Fatalf("image payload = %#v", withImage)
	}
	if withImage["model"] != buildVideoModel || withImage["resolution"] != "720p" || withImage["aspect_ratio"] != "16:9" {
		t.Fatalf("single-image payload = %#v", withImage)
	}

	withUpload, err := videoCreatePayload(provider.VideoRequest{Prompt: "x", Duration: 6}, "https://api.example/v1/media/uploads/tok", buildVideoRequestProfile)
	if err != nil {
		t.Fatal(err)
	}
	output, ok := withUpload["output"].(map[string]any)
	if !ok || output["upload_url"] != "https://api.example/v1/media/uploads/tok" {
		t.Fatalf("upload payload = %#v", withUpload)
	}
}

func TestBuildVideoCreatePayloadImageOnlyEmptyPrompt(t *testing.T) {
	payload, err := videoCreatePayload(provider.VideoRequest{
		Prompt:        "   ",
		Duration:      6,
		AspectRatio:   "16:9",
		Resolution:    "720p",
		ReferenceURLs: []string{"https://r2.example.com/first.png"},
	}, "", buildVideoRequestProfile)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["prompt"]; exists {
		t.Fatalf("empty prompt should be omitted: %#v", payload)
	}
	image, ok := payload["image"].(map[string]any)
	if !ok || image["image_url"] != "https://r2.example.com/first.png" || payload["model"] != buildVideoModel {
		t.Fatalf("image-only payload = %#v", payload)
	}
}

func TestXAIVideoCreatePayloadMatchesOfficialSchema(t *testing.T) {
	payload, err := videoCreatePayload(provider.VideoRequest{
		Prompt:        "animate",
		Duration:      6,
		AspectRatio:   "16:9",
		Resolution:    "720p",
		ReferenceURLs: []string{"https://cdn.example.com/r2/first.png"},
	}, "https://api.example/v1/media/uploads/tok", xaiVideoRequestProfile)
	if err != nil {
		t.Fatal(err)
	}
	if payload["model"] != xaiVideoModel {
		t.Fatalf("XAI model = %#v", payload["model"])
	}
	image, ok := payload["image"].(map[string]any)
	if !ok || image["url"] != "https://cdn.example.com/r2/first.png" {
		t.Fatalf("XAI image payload = %#v", payload["image"])
	}
	if _, exists := image["image_url"]; exists {
		t.Fatalf("XAI image payload leaked Build field: %#v", image)
	}
	output, ok := payload["output"].(map[string]any)
	if !ok || output["upload_url"] != "https://api.example/v1/media/uploads/tok" {
		t.Fatalf("XAI output payload = %#v", payload["output"])
	}
}

func TestGenerateVideoRejectsTwoImagesBeforeUpstream(t *testing.T) {
	adapter, encrypted := newTestBuildVideoAdapter(t)
	var hits atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		hits.Add(1)
		t.Fatalf("two-image request must not reach upstream: %s %s", request.Method, request.URL)
		return nil, nil
	})
	_, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: account.Credential{ID: 1, EncryptedAccessToken: encrypted},
		Prompt:     "animate",
		ReferenceURLs: []string{
			"https://cdn.example.com/one.png",
			"https://cdn.example.com/two.png",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "最多支持 1 张首图") {
		t.Fatalf("error = %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("upstream hits = %d", hits.Load())
	}
}

func TestGenerateVideoPostsSingleImageAndPollsUntilReady(t *testing.T) {
	adapter, encrypted := newTestBuildVideoAdapter(t)
	var createBody map[string]any
	var pollCount atomic.Int32
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if identity := infraegress.AccountFromContext(request.Context()); identity != "grok_build_9" {
			t.Fatalf("egress account identity = %q", identity)
		}
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/videos/generations":
			if request.Header.Get("Authorization") != "Bearer access-token" {
				t.Fatalf("missing auth header: %#v", request.Header)
			}
			if request.Header.Get("x-grok-model-override") != buildVideoModel {
				t.Fatalf("model override = %q", request.Header.Get("x-grok-model-override"))
			}
			if err := json.NewDecoder(request.Body).Decode(&createBody); err != nil {
				t.Fatal(err)
			}
			return jsonResponse(http.StatusOK, `{"request_id":"vid_123","status":"queued"}`, request), nil
		case request.Method == http.MethodGet && request.URL.Path == "/v1/videos/vid_123":
			count := pollCount.Add(1)
			if count == 1 {
				return jsonResponse(http.StatusOK, `{"status":"processing","progress":40}`, request), nil
			}
			return jsonResponse(http.StatusOK, `{"status":"completed","progress":100,"video":{"url":"https://assets.grok.com/videos/out.mp4"}}`, request), nil
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL)
			return nil, nil
		}
	})

	var progressValues []int
	result, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential:    account.Credential{ID: 9, UserID: "user-1", EncryptedAccessToken: encrypted},
		Prompt:        "animate knife",
		Duration:      6,
		AspectRatio:   "16:9",
		Resolution:    "720p",
		ReferenceURLs: []string{"https://r2.example.com/first.png"},
		Progress: func(value int) {
			progressValues = append(progressValues, value)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.URL != "https://assets.grok.com/videos/out.mp4" || result.ContentType != "video/mp4" {
		t.Fatalf("result = %#v", result)
	}
	image, _ := createBody["image"].(map[string]any)
	if createBody["model"] != buildVideoModel || image["image_url"] != "https://r2.example.com/first.png" || createBody["duration"] != float64(6) {
		t.Fatalf("create body = %#v", createBody)
	}
	if pollCount.Load() < 2 {
		t.Fatalf("poll count = %d", pollCount.Load())
	}
	if len(progressValues) == 0 || progressValues[0] != 1 {
		t.Fatalf("progress = %#v", progressValues)
	}
}

func TestGenerateVideoMapsUnauthorizedAndUpstreamErrors(t *testing.T) {
	adapter, encrypted := newTestBuildVideoAdapter(t)

	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusUnauthorized, `{"error":{"message":"bad token"}}`, request), nil
	})
	_, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: account.Credential{EncryptedAccessToken: encrypted},
		Prompt:     "test",
	})
	if !errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("401 err = %v", err)
	}

	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusBadRequest, `{"error":{"message":"invalid image"}}`, request), nil
	})
	_, err = adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: account.Credential{EncryptedAccessToken: encrypted},
		Prompt:     "test",
	})
	status, ok := provider.ErrorHTTPStatus(err)
	if !ok || status != http.StatusBadRequest || !strings.Contains(err.Error(), "invalid image") {
		t.Fatalf("4xx err = %v status=%d ok=%v", err, status, ok)
	}

	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusBadGateway, `upstream unavailable`, request), nil
	})
	_, err = adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: account.Credential{EncryptedAccessToken: encrypted},
		Prompt:     "test",
	})
	status, ok = provider.ErrorHTTPStatus(err)
	if !ok || status != http.StatusBadGateway {
		t.Fatalf("5xx err = %v status=%d ok=%v", err, status, ok)
	}
}

func TestVideoUpstreamErrorRedactsUploadTokenFromBody(t *testing.T) {
	const token = "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	if len(token) != 64 {
		t.Fatalf("fixture token length = %d", len(token))
	}
	fullURL := "https://api.example.com/v1/media/uploads/" + token
	// 上游校验响应可能回显完整 output.upload_url（含一次性 token）。
	body := []byte(`{"error":{"code":"invalid_request","message":"output.upload_url is not reachable: ` + fullURL + `"}}`)
	err := newVideoUpstreamError(http.StatusBadRequest, body)
	if err == nil {
		t.Fatal("expected videoUpstreamError")
	}
	status, ok := provider.ErrorHTTPStatus(err)
	if !ok || status != http.StatusBadRequest {
		t.Fatalf("status = %d ok=%v", status, ok)
	}
	msg := err.Error()
	if strings.Contains(msg, token) {
		t.Fatalf("error must not contain upload token: %q", msg)
	}
	if strings.Contains(msg, fullURL) {
		t.Fatalf("error must not contain full upload URL: %q", msg)
	}
	if strings.Contains(msg, "/v1/media/uploads/"+token) {
		t.Fatalf("error must not contain upload path with token: %q", msg)
	}
	if !strings.Contains(msg, "invalid_request") {
		t.Fatalf("safe error code should be preserved: %q", msg)
	}
	if !strings.Contains(msg, "not reachable") {
		t.Fatalf("safe error message fragment should be preserved: %q", msg)
	}
	if !strings.Contains(msg, "[REDACTED]") {
		t.Fatalf("expected redaction marker in summary: %q", msg)
	}

	// 非 JSON 正文中的完整路径同样脱敏。
	plain := newVideoUpstreamError(http.StatusUnprocessableEntity, []byte("reject upload_url="+fullURL))
	plainMsg := plain.Error()
	if strings.Contains(plainMsg, token) || strings.Contains(plainMsg, fullURL) {
		t.Fatalf("plain body error leaked secret: %q", plainMsg)
	}
	if plain.HTTPStatusCode() != http.StatusUnprocessableEntity {
		t.Fatalf("plain status = %d", plain.HTTPStatusCode())
	}
}

func TestParseVideoCreateAndStatusRedactUploadSecretsIn2xxErrors(t *testing.T) {
	const token = "11223344556677889900aabbccddeeff11223344556677889900aabbccddeeff"
	if len(token) != 64 {
		t.Fatalf("fixture token length = %d", len(token))
	}
	fullURL := "https://public.example/v1/media/uploads/" + token

	assertNoSecret := func(t *testing.T, msg string) {
		t.Helper()
		if strings.Contains(msg, token) {
			t.Fatalf("error leaked bare token: %q", msg)
		}
		if strings.Contains(msg, fullURL) {
			t.Fatalf("error leaked full upload URL: %q", msg)
		}
		if strings.Contains(msg, "/v1/media/uploads/"+token) {
			t.Fatalf("error leaked upload path+token: %q", msg)
		}
	}

	// 2xx create：error.message 回显完整 upload URL。
	_, err := parseVideoCreateResponse([]byte(`{"error":{"message":"invalid output.upload_url ` + fullURL + `"}}`))
	if err == nil {
		t.Fatal("expected create error")
	}
	createMsg := err.Error()
	assertNoSecret(t, createMsg)
	if !strings.Contains(createMsg, "invalid output.upload_url") {
		t.Fatalf("safe create diagnostics missing: %q", createMsg)
	}
	if !strings.Contains(createMsg, "[REDACTED]") {
		t.Fatalf("expected redaction in create error: %q", createMsg)
	}

	// 2xx create：error.message 仅含裸 64-hex token。
	_, err = parseVideoCreateResponse([]byte(`{"error":{"message":"token rejected: ` + token + `"}}`))
	if err == nil {
		t.Fatal("expected create bare-token error")
	}
	assertNoSecret(t, err.Error())
	if !strings.Contains(err.Error(), "token rejected") {
		t.Fatalf("safe bare-token fragment missing: %q", err.Error())
	}

	// 2xx status：顶层 error.message 含完整 URL。
	_, _, err = parseVideoStatusResponse([]byte(`{"error":{"message":"upload failed `+fullURL+`"}}`), nil, false)
	if err == nil {
		t.Fatal("expected status top-level error")
	}
	assertNoSecret(t, err.Error())

	// 2xx status：failed 状态 + error_message 含裸 token。
	_, _, err = parseVideoStatusResponse([]byte(`{"status":"failed","error_message":"moderation token=`+token+`"}`), nil, false)
	if err == nil {
		t.Fatal("expected failed status error")
	}
	statusMsg := err.Error()
	assertNoSecret(t, statusMsg)
	if !strings.Contains(statusMsg, "moderation") {
		t.Fatalf("safe status diagnostics missing: %q", statusMsg)
	}

	// 2xx status：failed + nested error.message 含完整 URL。
	_, _, err = parseVideoStatusResponse([]byte(`{"status":"failed","error":{"message":"bad upload_url `+fullURL+`"}}`), nil, false)
	if err == nil {
		t.Fatal("expected nested failed status error")
	}
	assertNoSecret(t, err.Error())
}

func TestGenerateVideoFailedStatusAndDownloadTrustedURL(t *testing.T) {
	adapter, encrypted := newTestBuildVideoAdapter(t)
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case request.Method == http.MethodPost:
			return jsonResponse(http.StatusOK, `{"id":"vid_fail"}`, request), nil
		case request.Method == http.MethodGet && request.URL.Path == "/v1/videos/vid_fail":
			return jsonResponse(http.StatusOK, `{"status":"failed","error":{"message":"moderation"}}`, request), nil
		default:
			t.Fatalf("unexpected %s %s", request.Method, request.URL)
			return nil, nil
		}
	})
	_, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: account.Credential{EncryptedAccessToken: encrypted},
		Prompt:     "bad",
	})
	if err == nil || !strings.Contains(err.Error(), "moderation") {
		t.Fatalf("failed status err = %v", err)
	}

	const assetURL = "https://vidgen.x.ai/videos/done.mp4"
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.String() != assetURL {
			t.Fatalf("download request = %s %s", request.Method, request.URL)
		}
		if identity := infraegress.AccountFromContext(request.Context()); identity != "grok_build_99" {
			t.Fatalf("download egress account identity = %q", identity)
		}
		for _, key := range []string{
			"Authorization", "X-XAI-Token-Auth", "x-userid",
			"x-grok-model-override", "x-grok-session-id", "x-grok-agent-id",
			"x-grok-conv-id", "x-grok-req-id", "x-grok-conversation-id",
			"x-grok-session-id-legacy", "x-grok-request-id",
			"x-grok-client-version", "x-grok-client-identifier",
		} {
			if value := request.Header.Get(key); value != "" {
				t.Fatalf("resource download must not send %s=%q", key, value)
			}
		}
		return &http.Response{
			StatusCode:    http.StatusOK,
			Status:        "200 OK",
			Header:        http.Header{"Content-Type": []string{"video/mp4"}},
			Body:          io.NopCloser(strings.NewReader("mp4-bytes")),
			ContentLength: 9,
			Request:       request,
		}, nil
	})
	// 即使传入凭据，下载也不得解密或转发 OAuth / 身份头。
	body, contentType, size, err := adapter.DownloadVideo(context.Background(), account.Credential{
		ID: 99, UserID: "user-1", EncryptedAccessToken: encrypted,
	}, assetURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = body.Close() }()
	data, _ := io.ReadAll(body)
	if string(data) != "mp4-bytes" || contentType != "video/mp4" || size != 9 {
		t.Fatalf("download = %q %s %d", data, contentType, size)
	}
	if !trustedBuildVideoAssetHost("vidgen.x.ai") {
		t.Fatal("vidgen.x.ai must be trusted")
	}

	_, _, _, err = adapter.DownloadVideo(context.Background(), account.Credential{EncryptedAccessToken: encrypted}, "https://evil.example/video.mp4")
	if err == nil || !strings.Contains(err.Error(), "不受信任") {
		t.Fatalf("untrusted download err = %v", err)
	}
}

func TestGenerateVideoRespectsContextCancelDuringPoll(t *testing.T) {
	adapter, encrypted := newTestBuildVideoAdapter(t)
	adapter.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodPost {
			return jsonResponse(http.StatusOK, `{"request_id":"vid_slow"}`, request), nil
		}
		return jsonResponse(http.StatusOK, `{"status":"processing","progress":10}`, request), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := adapter.GenerateVideo(ctx, provider.VideoRequest{
		Credential: account.Credential{EncryptedAccessToken: encrypted},
		Prompt:     "slow",
	})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel err = %v", err)
	}
}

func TestRegistryExposesBuildVideoAdapter(t *testing.T) {
	registry := provider.NewRegistry(NewAdapter(Config{}, nil))
	adapter, ok := registry.Videos(account.ProviderBuild)
	if !ok {
		t.Fatal("Build video adapter not registered")
	}
	if _, ok := adapter.(provider.VideoContentDownloader); !ok {
		t.Fatal("Build adapter must implement VideoContentDownloader")
	}
	definition, ok := registry.Definition(account.ProviderBuild)
	if !ok || !definition.SupportsModelCapability(modeldomain.CapabilityVideo) || !definition.Media.VideoGeneration {
		t.Fatalf("build definition = %#v", definition)
	}
}

func TestBuildVideoDefinitionDeclaresVideoCapability(t *testing.T) {
	adapter := NewAdapter(Config{}, nil)
	definition := adapter.Definition()
	if !definition.Media.VideoGeneration {
		t.Fatal("Media.VideoGeneration not enabled")
	}
	if !definition.SupportsModelCapability(modeldomain.CapabilityVideo) {
		t.Fatalf("capabilities = %#v", definition.ModelCapabilities)
	}
	if err := definition.Validate(); err != nil {
		t.Fatal(err)
	}
}

func newTestBuildVideoAdapter(t *testing.T) (*Adapter, string) {
	t.Helper()
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("access-token")
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter(Config{
		BaseURL:          "https://cli-chat-proxy.grok.com/v1",
		ClientVersion:    "0.2.99",
		ClientIdentifier: "grok-shell",
		TokenAuth:        "xai-grok-cli",
		UserAgent:        "grok-shell/0.2.99 (linux; x86_64)",
	}, cipher)
	return adapter, encrypted
}

func jsonResponse(status int, body string, request *http.Request) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}
