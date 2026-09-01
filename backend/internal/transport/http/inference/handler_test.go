package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/pkg/neterror"
	"github.com/gin-gonic/gin"
)

type idleErrorReader struct{}

func (idleErrorReader) Read([]byte) (int, error) {
	return 0, neterror.ErrUpstreamStreamIdleTimeout
}

type outputLoopErrorReader struct{}

func (outputLoopErrorReader) Read([]byte) (int, error) {
	return 0, fmt.Errorf("%w (repeated content delta 129 times)", neterror.ErrUpstreamOutputLoop)
}

type chunkErrorReader struct {
	data []byte
	done bool
}

func (r *chunkErrorReader) Read(target []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	return copy(target, r.data), errors.New("upstream cut")
}

func TestVideoGenerationUsesOfficialXAIEndpointsAndFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewHandler(nil, nil, 1<<20).Register(router.Group("/v1"))

	for _, test := range []struct {
		name string
		body string
	}{
		{name: "unsupported seconds", body: `{"model":"grok-imagine-video","prompt":"test","seconds":8}`},
		{name: "unsupported nested image url", body: `{"model":"grok-imagine-video","image":{"image_url":"https://example.com/input.png"}}`},
		{name: "unsupported size", body: `{"model":"grok-imagine-video","prompt":"test","size":"16:9"}`},
		{name: "unsupported quality", body: `{"model":"grok-imagine-video","prompt":"test","quality":"720p"}`},
		{name: "unsupported input reference", body: `{"model":"grok-imagine-video","input_reference":"https://example.com/input.png"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/videos/generations", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "unknown field") {
				t.Fatalf("unsupported field status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}

	invalidDuration := httptest.NewRequest(http.MethodPost, "/v1/videos/generations", strings.NewReader(`{"model":"grok-imagine-video","prompt":"test","duration":16}`))
	invalidDuration.Header.Set("Content-Type", "application/json")
	invalidRecorder := httptest.NewRecorder()
	router.ServeHTTP(invalidRecorder, invalidDuration)
	if invalidRecorder.Code != http.StatusBadRequest || !strings.Contains(invalidRecorder.Body.String(), "1 到 15") {
		t.Fatalf("invalid duration status=%d body=%s", invalidRecorder.Code, invalidRecorder.Body.String())
	}

	valid := httptest.NewRequest(http.MethodPost, "/v1/videos/generations", strings.NewReader(`{
		"model":"grok-imagine-video","prompt":"test","duration":"8",
		"aspect_ratio":"16:9","resolution":"720p","user":"end_user_1"
	}`))
	valid.Header.Set("Content-Type", "application/json")
	validRecorder := httptest.NewRecorder()
	router.ServeHTTP(validRecorder, valid)
	if validRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("official generation shape status=%d body=%s", validRecorder.Code, validRecorder.Body.String())
	}

	imageOnly := httptest.NewRequest(http.MethodPost, "/v1/videos/generations", strings.NewReader(`{
		"model":"grok-imagine-video","image":{"url":"https://example.com/input.png"}
	}`))
	imageOnly.Header.Set("Content-Type", "application/json")
	imageRecorder := httptest.NewRecorder()
	router.ServeHTTP(imageRecorder, imageOnly)
	if imageRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("image-only generation status=%d body=%s", imageRecorder.Code, imageRecorder.Body.String())
	}

	fileInput := httptest.NewRequest(http.MethodPost, "/v1/videos/generations", strings.NewReader(`{
		"model":"grok-imagine-video","image":{"file_id":"input_abcdefghijklmnopqrstuvwxyz012345"}
	}`))
	fileInput.Header.Set("Content-Type", "application/json")
	fileRecorder := httptest.NewRecorder()
	router.ServeHTTP(fileRecorder, fileInput)
	if fileRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("file input generation status=%d body=%s", fileRecorder.Code, fileRecorder.Body.String())
	}

	ambiguousInput := httptest.NewRequest(http.MethodPost, "/v1/videos/generations", strings.NewReader(`{
		"model":"grok-imagine-video","image":{"url":"https://example.com/input.png","file_id":"input_abcdefghijklmnopqrstuvwxyz012345"}
	}`))
	ambiguousInput.Header.Set("Content-Type", "application/json")
	ambiguousRecorder := httptest.NewRecorder()
	router.ServeHTTP(ambiguousRecorder, ambiguousInput)
	if ambiguousRecorder.Code != http.StatusBadRequest || !strings.Contains(ambiguousRecorder.Body.String(), "url 或 file_id") {
		t.Fatalf("ambiguous input status=%d body=%s", ambiguousRecorder.Code, ambiguousRecorder.Body.String())
	}

	wrongContentType := httptest.NewRequest(http.MethodPost, "/v1/videos/generations", strings.NewReader(`{"model":"grok-imagine-video","prompt":"test"}`))
	wrongContentType.Header.Set("Content-Type", "text/plain")
	wrongContentTypeRecorder := httptest.NewRecorder()
	router.ServeHTTP(wrongContentTypeRecorder, wrongContentType)
	if wrongContentTypeRecorder.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("wrong content type status=%d body=%s", wrongContentTypeRecorder.Code, wrongContentTypeRecorder.Body.String())
	}

	unsupportedRecorder := httptest.NewRecorder()
	router.ServeHTTP(unsupportedRecorder, httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{}`)))
	if unsupportedRecorder.Code != http.StatusNotFound {
		t.Fatalf("unsupported video endpoint status=%d", unsupportedRecorder.Code)
	}
	contentRecorder := httptest.NewRecorder()
	router.ServeHTTP(contentRecorder, httptest.NewRequest(http.MethodGet, "/v1/videos/request_1/content", nil))
	if contentRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("video content endpoint status=%d", contentRecorder.Code)
	}
}

func TestWriteVideoContentRejectsDeclaredOversizeMedia(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	writeVideoContent(context, strings.NewReader("ignored"), "video/mp4", maxMediaResponseTransferBytes+1, "video_request_1")
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "media_too_large") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// A saved response needs an extension or the file will not open in a player.
func TestWriteVideoContentNamesDownloadWithExtension(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, testCase := range []struct {
		contentType string
		want        string
	}{
		{"video/mp4", `inline; filename="video_request_1.mp4"`},
		{"video/webm", `inline; filename="video_request_1.webm"`},
		{"video/quicktime", `inline; filename="video_request_1.mov"`},
	} {
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		writeVideoContent(context, strings.NewReader("body"), testCase.contentType, 4, "video_request_1")
		if got := recorder.Header().Get("Content-Disposition"); got != testCase.want {
			t.Fatalf("%s disposition = %q, want %q", testCase.contentType, got, testCase.want)
		}
	}
}

func TestVideoContentURLUsesConfiguredPublicAPIBase(t *testing.T) {
	handler := NewHandler(nil, nil, 1<<20, "https://api.example.com/grok2api/")
	response := videoGenerationResponse(mediadomain.Job{ID: "video_request_1", Status: mediadomain.StatusCompleted, UpstreamURL: "https://assets.grok.com/source.mp4"}, handler.videoContentURL("video_request_1"))
	video, ok := response["video"].(gin.H)
	if !ok || video["url"] != "https://api.example.com/grok2api/v1/videos/video_request_1/content" {
		t.Fatalf("response = %#v", response)
	}
}

// A completed job whose local asset has been verified by the gateway must point
// at the public media route. /v1/videos/{id}/content requires the client API key
// and is therefore not usable in a browser or player.
func TestVideoPlaybackURLPrefersPublicAssetRoute(t *testing.T) {
	handler := NewHandler(nil, nil, 1<<20, "https://api.example.com/grok2api/")
	job := mediadomain.Job{
		ID: "video_request_1", Status: mediadomain.StatusCompleted,
		UpstreamURL: "https://assets.grok.com/source.mp4", ResultAssetID: "vid_abc123",
	}
	response := videoGenerationResponse(job, handler.videoPlaybackURL(job))
	video, ok := response["video"].(gin.H)
	if !ok || video["url"] != "https://api.example.com/grok2api/v1/media/videos/vid_abc123" {
		t.Fatalf("response = %#v", response)
	}
}

// Without a readable stored asset the protected content endpoint remains the
// only option and can still fall back to the upstream download path.
func TestVideoPlaybackURLFallsBackToContentEndpoint(t *testing.T) {
	handler := NewHandler(nil, nil, 1<<20, "https://api.example.com/grok2api/")
	job := mediadomain.Job{ID: "video_request_1", Status: mediadomain.StatusCompleted}
	if got := handler.videoPlaybackURL(job); got != "https://api.example.com/grok2api/v1/videos/video_request_1/content" {
		t.Fatalf("fallback URL = %q", got)
	}
}

func TestVideoContentURLFollowsRuntimePublicAPIBase(t *testing.T) {
	baseURL := "https://old.example.com"
	handler := NewHandler(nil, nil, 1<<20, "https://static.example.com").SetPublicAPIBaseURLResolver(func() string {
		return baseURL
	})
	if got := handler.videoContentURL("video_request_1"); got != "https://old.example.com/v1/videos/video_request_1/content" {
		t.Fatalf("initial URL = %q", got)
	}
	baseURL = "https://new.example.com/api/"
	if got := handler.videoContentURL("video_request_2"); got != "https://new.example.com/api/v1/videos/video_request_2/content" {
		t.Fatalf("updated URL = %q", got)
	}
}

func TestGatewayErrorDoesNotExposeInternalDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/", func(c *gin.Context) {
		writeGatewayError(c, errors.New("dial postgres://secret@internal:5432 failed"))
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusBadGateway || strings.Contains(recorder.Body.String(), "postgres") || !strings.Contains(recorder.Body.String(), "上游服务暂不可用") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestGatewayErrorMapsOversizedVideoInputToBadRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/", func(c *gin.Context) {
		writeGatewayError(c, gateway.ErrVideoInputTooLarge)
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"code":"invalid_request"`) || !strings.Contains(recorder.Body.String(), "32 MiB") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestGatewayErrorMapsInvalidVideoParametersToBadRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/", func(c *gin.Context) {
		writeGatewayError(c, fmt.Errorf("%w: Console reference_images 最多 7 张", gateway.ErrVideoParameterInvalid))
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"code":"invalid_request"`) || !strings.Contains(recorder.Body.String(), "最多 7 张") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestGatewayErrorMapsLedgerUnavailableToServiceUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/", func(c *gin.Context) {
		writeGatewayError(c, gateway.ErrLedgerUnavailable)
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), `"code":"ledger_unavailable"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestGatewayErrorMapsDisallowedModelWithoutCallingItUpstreamUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name      string
		anthropic bool
		wantType  string
	}{
		{name: "openai", wantType: `"code":"model_not_allowed"`},
		{name: "anthropic", anthropic: true, wantType: `"type":"permission_error"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			router := gin.New()
			router.GET("/", func(c *gin.Context) {
				if test.anthropic {
					writeGatewayAnthropicError(c, clientkeyapp.ErrModelNotAllowed)
					return
				}
				writeGatewayError(c, clientkeyapp.ErrModelNotAllowed)
			})
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
			if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), test.wantType) || strings.Contains(recorder.Body.String(), "upstream_unavailable") {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestGatewayErrorMapsResponseHeaderTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openAIRouter := gin.New()
	openAIRouter.GET("/", func(c *gin.Context) {
		writeGatewayError(c, &gateway.UpstreamFailure{
			HTTPStatus: http.StatusGatewayTimeout, Code: "upstream_header_timeout", PublicMessage: "等待上游响应头超时",
		})
	})
	openAIRecorder := httptest.NewRecorder()
	openAIRouter.ServeHTTP(openAIRecorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if openAIRecorder.Code != http.StatusGatewayTimeout || !strings.Contains(openAIRecorder.Body.String(), `"code":"upstream_header_timeout"`) {
		t.Fatalf("OpenAI status=%d body=%s", openAIRecorder.Code, openAIRecorder.Body.String())
	}

	anthropicRouter := gin.New()
	anthropicRouter.GET("/", func(c *gin.Context) {
		writeGatewayAnthropicError(c, &gateway.UpstreamFailure{
			HTTPStatus: http.StatusGatewayTimeout, Code: "upstream_header_timeout", PublicMessage: "等待上游响应头超时",
		})
	})
	anthropicRecorder := httptest.NewRecorder()
	anthropicRouter.ServeHTTP(anthropicRecorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if anthropicRecorder.Code != http.StatusGatewayTimeout || !strings.Contains(anthropicRecorder.Body.String(), `"type":"timeout_error"`) {
		t.Fatalf("Anthropic status=%d body=%s", anthropicRecorder.Code, anthropicRecorder.Body.String())
	}
}

func TestGatewayErrorHidesUpstreamCredentialStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	openAIRouter := gin.New()
	openAIRouter.GET("/", func(c *gin.Context) {
		writeGatewayError(c, &gateway.UpstreamFailure{
			HTTPStatus: http.StatusForbidden, Code: "upstream_forbidden", PublicMessage: "上游拒绝了该请求",
			UpstreamCode: "permission-denied",
			Cause:        errors.New("secret upstream response"),
		})
	})
	openAIRecorder := httptest.NewRecorder()
	openAIRouter.ServeHTTP(openAIRecorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if openAIRecorder.Code != http.StatusServiceUnavailable || !strings.Contains(openAIRecorder.Body.String(), `"code":"permission-denied"`) || !strings.Contains(openAIRecorder.Body.String(), "上游服务暂不可用，聊天端点访问被拒绝") || strings.Contains(openAIRecorder.Body.String(), "secret") || strings.Contains(openAIRecorder.Body.String(), "上游拒绝了该请求") {
		t.Fatalf("OpenAI status=%d body=%s", openAIRecorder.Code, openAIRecorder.Body.String())
	}

	anthropicRouter := gin.New()
	anthropicRouter.GET("/", func(c *gin.Context) {
		writeGatewayAnthropicError(c, &gateway.UpstreamFailure{
			HTTPStatus: http.StatusTooManyRequests, Code: "upstream_rate_limited", PublicMessage: "上游请求频率受限",
		})
	})
	anthropicRecorder := httptest.NewRecorder()
	anthropicRouter.ServeHTTP(anthropicRecorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if anthropicRecorder.Code != http.StatusTooManyRequests || !strings.Contains(anthropicRecorder.Body.String(), `"type":"rate_limit_error"`) {
		t.Fatalf("Anthropic status=%d body=%s", anthropicRecorder.Code, anthropicRecorder.Body.String())
	}

	quotaRouter := gin.New()
	quotaRouter.GET("/", func(c *gin.Context) {
		writeGatewayAnthropicError(c, &gateway.UpstreamFailure{
			HTTPStatus: http.StatusTooManyRequests, Code: "upstream_rate_limited", PublicMessage: "official upgrade prompt",
			QuotaExhausted: true,
		})
	})
	quotaRecorder := httptest.NewRecorder()
	quotaRouter.ServeHTTP(quotaRecorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if quotaRecorder.Code != http.StatusServiceUnavailable || !strings.Contains(quotaRecorder.Body.String(), `"type":"overloaded_error"`) || strings.Contains(quotaRecorder.Body.String(), "upgrade") {
		t.Fatalf("Anthropic quota status=%d body=%s", quotaRecorder.Code, quotaRecorder.Body.String())
	}

	credentialRouter := gin.New()
	credentialRouter.GET("/", func(c *gin.Context) {
		writeGatewayAnthropicError(c, &gateway.UpstreamFailure{
			HTTPStatus: http.StatusUnauthorized, Code: "upstream_unauthorized", PublicMessage: "上游账号认证失败",
		})
	})
	credentialRecorder := httptest.NewRecorder()
	credentialRouter.ServeHTTP(credentialRecorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if credentialRecorder.Code != http.StatusServiceUnavailable || !strings.Contains(credentialRecorder.Body.String(), `"type":"overloaded_error"`) || strings.Contains(credentialRecorder.Body.String(), "认证") {
		t.Fatalf("Anthropic credential status=%d body=%s", credentialRecorder.Code, credentialRecorder.Body.String())
	}
}

func TestDirectUpstreamCredentialResponsesAreRewritten(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(nil, nil, 1<<20)
	for _, tc := range []struct {
		name      string
		status    int
		anthropic bool
		media     bool
		body      string
		wantCode  string
	}{
		{name: "openai unauthorized", status: http.StatusUnauthorized, body: `{"error":"secret upstream credential detail"}`, wantCode: "upstream_unavailable"},
		{name: "anthropic forbidden", status: http.StatusForbidden, anthropic: true, body: `{"code":"permission-denied","error":"secret upstream credential detail"}`, wantCode: "permission-denied"},
		{name: "media forbidden", status: http.StatusForbidden, media: true, body: `{"code":"permission-denied","error":"secret upstream credential detail"}`, wantCode: "permission-denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			finalCode := ""
			result := &gateway.Result{
				StatusCode: tc.status,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(tc.body)),
				Finalize: func(_ gateway.Usage, _, code string) {
					finalCode = code
				},
			}
			router := gin.New()
			router.GET("/", func(c *gin.Context) {
				switch {
				case tc.media:
					handler.writeMediaResult(c, result)
				case tc.anthropic:
					handler.writeAnthropicResult(c, result, false)
				default:
					handler.writeResult(c, result, false, streamProtocolResponses)
				}
			})
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
			if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), `"`+tc.wantCode+`"`) || strings.Contains(recorder.Body.String(), "secret") || finalCode != "upstream_unavailable" {
				t.Fatalf("status=%d body=%s finalize=%s", recorder.Code, recorder.Body.String(), finalCode)
			}
			if tc.wantCode == "permission-denied" && !strings.Contains(recorder.Body.String(), "上游服务暂不可用，聊天端点访问被拒绝") {
				t.Fatalf("permission message missing: %s", recorder.Body.String())
			}
		})
	}
}

func TestNonStreamingEmptyAndIdleResponsesFailBeforeCommittingSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(nil, nil, 1<<20)
	for _, test := range []struct {
		name       string
		body       io.ReadCloser
		wantStatus int
		wantCode   string
	}{
		{name: "empty", body: io.NopCloser(strings.NewReader("")), wantStatus: http.StatusBadGateway, wantCode: "upstream_response_empty"},
		{name: "idle", body: io.NopCloser(idleErrorReader{}), wantStatus: http.StatusGatewayTimeout, wantCode: "upstream_stream_idle_timeout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			finalCode := ""
			result := &gateway.Result{
				StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": {"application/json"}}, Body: test.body,
				Finalize: func(_ gateway.Usage, _, code string) { finalCode = code },
			}
			router := gin.New()
			router.GET("/", func(c *gin.Context) { handler.writeResult(c, result, false, streamProtocolResponses) })
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
			if recorder.Code != test.wantStatus || finalCode != test.wantCode || !strings.Contains(recorder.Body.String(), `"`+test.wantCode+`"`) {
				t.Fatalf("status=%d body=%s finalize=%q", recorder.Code, recorder.Body.String(), finalCode)
			}
		})
	}
}

func TestMediaResultRejectsActiveContentAndRedirects(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(nil, nil, 1<<20)
	for _, tc := range []struct {
		name        string
		status      int
		contentType string
		headers     http.Header
	}{
		{name: "html", status: http.StatusOK, contentType: "text/html; charset=utf-8"},
		{name: "svg", status: http.StatusOK, contentType: "image/svg+xml"},
		{name: "redirect", status: http.StatusFound, contentType: "text/plain", headers: http.Header{"Location": {"javascript:alert(1)"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			finalCode := ""
			header := make(http.Header)
			for name, values := range tc.headers {
				header[name] = append([]string(nil), values...)
			}
			header.Set("Content-Type", tc.contentType)
			result := &gateway.Result{
				StatusCode: tc.status,
				Header:     header,
				Body:       io.NopCloser(strings.NewReader(`<script id="must-not-reflect">alert(1)</script>`)),
				Finalize: func(_ gateway.Usage, _, code string) {
					finalCode = code
				},
			}
			router := gin.New()
			router.GET("/", func(c *gin.Context) { handler.writeMediaResult(c, result) })
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
			if recorder.Code != http.StatusBadGateway || strings.Contains(recorder.Body.String(), "must-not-reflect") || recorder.Header().Get("Location") != "" || finalCode == "" {
				t.Fatalf("status=%d headers=%#v body=%s finalize=%q", recorder.Code, recorder.Header(), recorder.Body.String(), finalCode)
			}
		})
	}
}

func TestMediaResultPinsSafeResponseHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(nil, nil, 1<<20)
	result := &gateway.Result{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":   {"audio/mpeg; attacker=ignored"},
			"Content-Length": {"5"},
			"Set-Cookie":     {"upstream=secret"},
			"Location":       {"javascript:alert(1)"},
			"X-Request-Id":   {"upstream-request"},
		},
		Body:     io.NopCloser(strings.NewReader("audio")),
		Finalize: func(gateway.Usage, string, string) {},
	}
	router := gin.New()
	router.GET("/", func(c *gin.Context) { handler.writeMediaResult(c, result) })
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "audio" {
		t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Content-Type") != "audio/mpeg" || recorder.Header().Get("X-Content-Type-Options") != "nosniff" || recorder.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("safe headers = %#v", recorder.Header())
	}
	if recorder.Header().Get("Set-Cookie") != "" || recorder.Header().Get("Location") != "" || recorder.Header().Get("X-Request-Id") != "upstream-request" {
		t.Fatalf("unsafe or missing forwarded headers = %#v", recorder.Header())
	}
}

func TestVideoContentRejectsUnsafeContentType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	writeVideoContent(context, strings.NewReader(`<script id="must-not-reflect">alert(1)</script>`), "text/html", -1, "video_request_1")
	if recorder.Code != http.StatusBadGateway || strings.Contains(recorder.Body.String(), "must-not-reflect") {
		t.Fatalf("status=%d headers=%#v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
	}
}

func TestMessagesEndpointUsesAnthropicContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewHandler(nil, nil, 1<<20).Register(router.Group("/v1"))

	missingVersion := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"grok-4.5","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`))
	missingVersion.Header.Set("Content-Type", "application/json")
	missingRecorder := httptest.NewRecorder()
	router.ServeHTTP(missingRecorder, missingVersion)
	if missingRecorder.Code != http.StatusBadRequest || !strings.Contains(missingRecorder.Body.String(), `"type":"error"`) {
		t.Fatalf("missing version status=%d body=%s", missingRecorder.Code, missingRecorder.Body.String())
	}

	valid := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"grok-4.5","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`))
	valid.Header.Set("Content-Type", "application/json")
	valid.Header.Set("anthropic-version", "2023-06-01")
	validRecorder := httptest.NewRecorder()
	router.ServeHTTP(validRecorder, valid)
	if validRecorder.Code != http.StatusUnauthorized || !strings.Contains(validRecorder.Body.String(), `"type":"authentication_error"`) {
		t.Fatalf("valid shape status=%d body=%s", validRecorder.Code, validRecorder.Body.String())
	}

	zeroTokens := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"grok-4.5","max_tokens":0,"messages":[{"role":"user","content":"hi"}]}`))
	zeroTokens.Header.Set("Content-Type", "application/json")
	zeroTokens.Header.Set("anthropic-version", "2023-06-01")
	zeroRecorder := httptest.NewRecorder()
	router.ServeHTTP(zeroRecorder, zeroTokens)
	if zeroRecorder.Code != http.StatusBadRequest {
		t.Fatalf("zero max_tokens status=%d body=%s", zeroRecorder.Code, zeroRecorder.Body.String())
	}
}

func TestJSONInferenceEndpointsRejectWrongMediaTypeAndTrailingDocument(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewHandler(nil, nil, 1<<20).Register(router.Group("/v1"))

	for _, path := range []string{"/v1/responses", "/v1/images/generations"} {
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"test","prompt":"test"}`))
		request.Header.Set("Content-Type", "text/plain")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("%s status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
	}

	for _, test := range []struct {
		path string
		body string
	}{
		{path: "/v1/images/generations", body: `{"model":"grok-imagine-image","prompt":"test"}{}`},
		{path: "/v1/images/edits", body: `{"model":"grok-imagine-image-edit","prompt":"test","image":{"url":"https://example.com/input.png"}}{}`},
	} {
		request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d body=%s", test.path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestVideoDurationUsesOfficialFieldOnly(t *testing.T) {
	if value, err := parseVideoDuration(nil); err != nil || value != 8 {
		t.Fatalf("default duration=%d err=%v", value, err)
	}
	if value, err := parseVideoDuration(json.RawMessage(`"6"`)); err != nil || value != 6 {
		t.Fatalf("duration=%d err=%v", value, err)
	}
}

func TestVideoGenerationResponseMatchesOfficialPollingShape(t *testing.T) {
	now := time.Now().UTC()
	pending := videoGenerationResponse(mediadomain.Job{Model: "grok-imagine-video", Status: mediadomain.StatusInProgress, Progress: 42})
	if pending["status"] != "pending" || pending["progress"] != 42 || pending["model"] != "grok-imagine-video" || pending["video"] != nil {
		t.Fatalf("pending response=%#v", pending)
	}
	done := videoGenerationResponse(mediadomain.Job{Model: "grok-imagine-video", Status: mediadomain.StatusCompleted, Progress: 100, Seconds: 8, UpstreamURL: "https://assets.grok.com/video.mp4", CompletedAt: &now})
	video, ok := done["video"].(gin.H)
	if done["status"] != "done" || done["progress"] != 100 || !ok || video["url"] != "https://assets.grok.com/video.mp4" || video["duration"] != 8 || video["respect_moderation"] != true {
		t.Fatalf("done response=%#v", done)
	}
	extended := videoGenerationResponse(mediadomain.Job{Operation: mediadomain.VideoOperationExtend, Status: mediadomain.StatusCompleted, Seconds: 6, UpstreamURL: "https://assets.grok.com/extended.mp4"})
	extendedVideo, ok := extended["video"].(gin.H)
	if !ok || extendedVideo["duration"] != nil {
		t.Fatalf("extended response exposes requested extension as output duration: %#v", extended)
	}
	failed := videoGenerationResponse(mediadomain.Job{Status: mediadomain.StatusFailed, ErrorCode: "account_unavailable", ErrorMessage: "try later"})
	errorValue, ok := failed["error"].(gin.H)
	if failed["status"] != "failed" || !ok || errorValue["code"] != "service_unavailable" || failed["model"] != nil || failed["progress"] != nil {
		t.Fatalf("failed response=%#v", failed)
	}
}

func TestImageGenerationEndpointValidatesXAIContractBeforeRouting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewHandler(nil, nil, 1<<20).Register(router.Group("/v1"))

	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{name: "zero n", body: `{"model":"grok-imagine-image","prompt":"test","n":0}`, want: "n 必须在 1 到 10 之间"},
		{name: "large n", body: `{"model":"grok-imagine-image","prompt":"test","n":11}`, want: "n 必须在 1 到 10 之间"},
		{name: "invalid quality", body: `{"model":"grok-imagine-image-2.0","prompt":"test","quality":"high"}`, want: "quality 必须是 low 或 medium"},
		{name: "storage options", body: `{"model":"grok-imagine-image","prompt":"test","storage_options":{"filename":"test.jpg"}}`, want: "不支持 storage_options"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), test.want) {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/image", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("singular image endpoint status = %d", recorder.Code)
	}
}

func TestImageEditAcceptsOfficialJSONShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewHandler(nil, nil, 1<<20).Register(router.Group("/v1"))

	missingImage := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(`{
		"model":"grok-imagine-image-edit","prompt":"变成黑色 白字","n":1
	}`))
	missingImage.Header.Set("Content-Type", "application/json")
	missingRecorder := httptest.NewRecorder()
	router.ServeHTTP(missingRecorder, missingImage)
	if missingRecorder.Code != http.StatusBadRequest || !strings.Contains(missingRecorder.Body.String(), "image 或 images") {
		t.Fatalf("missing image status=%d body=%s", missingRecorder.Code, missingRecorder.Body.String())
	}

	validShape := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(`{
		"model":"grok-imagine-image-edit","prompt":"变成黑色 白字","n":1,"resolution":"1k",
		"image":{"url":"https://example.com/input.png"},"aspect_ratio":"1:1",
		"stream":true,"partial_images":1
	}`))
	validShape.Header.Set("Content-Type", "application/json")
	validRecorder := httptest.NewRecorder()
	router.ServeHTTP(validRecorder, validShape)
	if validRecorder.Code != http.StatusUnauthorized || strings.Contains(validRecorder.Body.String(), "multipart") {
		t.Fatalf("valid JSON shape status=%d body=%s", validRecorder.Code, validRecorder.Body.String())
	}

	invalidResolution := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(`{
		"model":"grok-imagine-image-edit","prompt":"test","resolution":"4k",
		"image":{"url":"https://example.com/input.png"}
	}`))
	invalidResolution.Header.Set("Content-Type", "application/json")
	invalidResolutionRecorder := httptest.NewRecorder()
	router.ServeHTTP(invalidResolutionRecorder, invalidResolution)
	if invalidResolutionRecorder.Code != http.StatusBadRequest || !strings.Contains(invalidResolutionRecorder.Body.String(), "resolution 必须是 1k 或 2k") {
		t.Fatalf("invalid resolution status=%d body=%s", invalidResolutionRecorder.Code, invalidResolutionRecorder.Body.String())
	}

	invalidQuality := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(`{
		"model":"grok-imagine-image-2.0","prompt":"test","quality":"high",
		"image":{"url":"https://example.com/input.png"}
	}`))
	invalidQuality.Header.Set("Content-Type", "application/json")
	invalidQualityRecorder := httptest.NewRecorder()
	router.ServeHTTP(invalidQualityRecorder, invalidQuality)
	if invalidQualityRecorder.Code != http.StatusBadRequest || !strings.Contains(invalidQualityRecorder.Body.String(), "quality 必须是 low 或 medium") {
		t.Fatalf("invalid quality status=%d body=%s", invalidQualityRecorder.Code, invalidQualityRecorder.Body.String())
	}

	validBatchCount := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(`{
		"model":"grok-imagine-image-quality","prompt":"test","n":2,
		"image":{"url":"https://example.com/input.png"}
	}`))
	validBatchCount.Header.Set("Content-Type", "application/json")
	validBatchRecorder := httptest.NewRecorder()
	router.ServeHTTP(validBatchRecorder, validBatchCount)
	if validBatchRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("valid batch count status=%d body=%s", validBatchRecorder.Code, validBatchRecorder.Body.String())
	}

	invalidCount := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(`{
		"model":"grok-imagine-image-quality","prompt":"test","n":11,
		"image":{"url":"https://example.com/input.png"}
	}`))
	invalidCount.Header.Set("Content-Type", "application/json")
	invalidCountRecorder := httptest.NewRecorder()
	router.ServeHTTP(invalidCountRecorder, invalidCount)
	if invalidCountRecorder.Code != http.StatusBadRequest || !strings.Contains(invalidCountRecorder.Body.String(), "n 必须在 1 到 10 之间") {
		t.Fatalf("invalid count status=%d body=%s", invalidCountRecorder.Code, invalidCountRecorder.Body.String())
	}

	for _, test := range []struct {
		name string
		body string
	}{
		{name: "negative partial images", body: `{"model":"grok-imagine-image-edit","prompt":"test","stream":true,"partial_images":-1,"image":{"url":"https://example.com/input.png"}}`},
		{name: "too many partial images", body: `{"model":"grok-imagine-image-edit","prompt":"test","stream":true,"partial_images":4,"image":{"url":"https://example.com/input.png"}}`},
		{name: "partial images require stream", body: `{"model":"grok-imagine-image-edit","prompt":"test","partial_images":1,"image":{"url":"https://example.com/input.png"}}`},
		{name: "invalid aspect ratio", body: `{"model":"grok-imagine-image-edit","prompt":"test","aspect_ratio":"7:5","image":{"url":"https://example.com/input.png"}}`},
		{name: "invalid size", body: `{"model":"grok-imagine-image-edit","prompt":"test","size":"512x512","image":{"url":"https://example.com/input.png"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}

	multipartRequest := httptest.NewRequest(http.MethodPost, "/v1/images/edits", strings.NewReader("ignored"))
	multipartRequest.Header.Set("Content-Type", "multipart/form-data; boundary=test")
	multipartRecorder := httptest.NewRecorder()
	router.ServeHTTP(multipartRecorder, multipartRequest)
	if multipartRecorder.Code != http.StatusUnsupportedMediaType || !strings.Contains(multipartRecorder.Body.String(), "application/json") {
		t.Fatalf("multipart status=%d body=%s", multipartRecorder.Code, multipartRecorder.Body.String())
	}
}

func TestImageGenerationValidatesOpenAIPartialImages(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewHandler(nil, nil, 1<<20).Register(router.Group("/v1"))

	for _, test := range []struct {
		name string
		body string
	}{
		{name: "negative", body: `{"model":"grok-imagine-image-quality","prompt":"cat","stream":true,"partial_images":-1}`},
		{name: "too many", body: `{"model":"grok-imagine-image-quality","prompt":"cat","stream":true,"partial_images":4}`},
		{name: "requires stream", body: `{"model":"grok-imagine-image-quality","prompt":"cat","partial_images":1}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "partial_images") {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}

	invalidStreamingCount := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{
		"model":"grok-imagine-image-quality","prompt":"cat","n":2,"stream":true
	}`))
	invalidStreamingCount.Header.Set("Content-Type", "application/json")
	invalidStreamingCountRecorder := httptest.NewRecorder()
	router.ServeHTTP(invalidStreamingCountRecorder, invalidStreamingCount)
	if invalidStreamingCountRecorder.Code != http.StatusBadRequest {
		t.Fatalf("stream n status=%d body=%s", invalidStreamingCountRecorder.Code, invalidStreamingCountRecorder.Body.String())
	}
	var payload map[string]any
	if json.Unmarshal(invalidStreamingCountRecorder.Body.Bytes(), &payload) != nil {
		t.Fatalf("stream n body=%s", invalidStreamingCountRecorder.Body.String())
	}
	errorValue, _ := payload["error"].(map[string]any)
	if errorValue["message"] != "Streaming is only supported with n=1." || errorValue["type"] != "image_generation_user_error" || errorValue["param"] != "input" || errorValue["code"] != "unsupported_parameter" {
		t.Fatalf("stream n error=%#v", errorValue)
	}

	valid := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{
		"model":"grok-imagine-image-quality","prompt":"cat","n":1,"stream":true,"partial_images":1
	}`))
	valid.Header.Set("Content-Type", "application/json")
	validRecorder := httptest.NewRecorder()
	router.ServeHTTP(validRecorder, valid)
	if validRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("valid status=%d body=%s", validRecorder.Code, validRecorder.Body.String())
	}
}

func TestExtractUsageFromCompletedEvent(t *testing.T) {
	metadata := extractMetadata([]byte(`{"type":"response.completed","response":{"id":"resp_1","model":"grok-4.5-build-free","usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":4},"output_tokens":5,"output_tokens_details":{"reasoning_tokens":2},"total_tokens":15,"cost_in_usd_ticks":158500,"num_sources_used":1,"num_server_side_tools_used":2,"context_details":{"input_tokens":9,"output_tokens":4}}}}`))
	usage := metadata.Usage
	if usage.InputTokens != 10 || usage.OutputTokens != 5 || usage.TotalTokens != 15 {
		t.Fatalf("usage = %#v", usage)
	}
	if usage.CachedInputTokens != 4 || usage.ReasoningTokens != 2 || metadata.ResponseID != "resp_1" {
		t.Fatalf("metadata = %#v", metadata)
	}
	if usage.CostInUSDTicks != 158500 || usage.NumSourcesUsed != 1 || usage.NumServerSideToolsUsed != 2 || usage.ContextInputTokens != 9 || usage.ContextOutputTokens != 4 || usage.ResponseModel != "grok-4.5-build-free" {
		t.Fatalf("observed usage = %#v", usage)
	}
}

func TestExtractUsageFromAnthropicMessagesCaches(t *testing.T) {
	metadata := normalizeMetadataUsage(extractMetadata([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"grok-4.5","usage":{"input_tokens":20,"output_tokens":20,"cache_creation_input_tokens":0,"cache_read_input_tokens":80,"output_tokens_details":{"thinking_tokens":12},"cost_in_usd_ticks":1000}}`)), streamProtocolAnthropic)
	if metadata.Usage.CachedInputTokens != 80 || metadata.Usage.InputTokens != 100 || metadata.Usage.OutputTokens != 20 || metadata.Usage.ReasoningTokens != 12 {
		t.Fatalf("anthropic usage = %#v", metadata.Usage)
	}
	if metadata.Usage.TotalTokens != 120 {
		t.Fatalf("anthropic total usage = %#v", metadata.Usage)
	}
}

func TestExtractUsageFromChatCompletionsCaches(t *testing.T) {
	// OpenAI Chat Completions 用 prompt_tokens_details.cached_tokens。
	metadata := extractMetadata([]byte(`{"id":"chatcmpl_1","object":"chat.completion","model":"grok-4.5","usage":{"prompt_tokens":50,"completion_tokens":10,"total_tokens":60,"prompt_tokens_details":{"cached_tokens":30},"completion_tokens_details":{"reasoning_tokens":5}}}`))
	if metadata.Usage.CachedInputTokens != 30 || metadata.Usage.InputTokens != 50 || metadata.Usage.OutputTokens != 10 || metadata.Usage.ReasoningTokens != 5 || metadata.Usage.TotalTokens != 60 {
		t.Fatalf("chat usage = %#v", metadata.Usage)
	}
}

func TestExtractUsagePrefersResponsesCachedTokensOverAnthropicField(t *testing.T) {
	// 同时存在时优先 Responses 字段（正常路径不会并存，防回归）。
	metadata := extractMetadata([]byte(`{"usage":{"input_tokens":10,"output_tokens":1,"input_tokens_details":{"cached_tokens":7},"cache_read_input_tokens":99}}`))
	if metadata.Usage.CachedInputTokens != 7 {
		t.Fatalf("prefer responses cached = %#v", metadata.Usage)
	}
}

func TestStreamInspectorMergesCachedTokensAcrossFrames(t *testing.T) {
	inspector := &responseInspector{protocol: streamProtocolAnthropic}
	inspector.Inspect([]byte("data: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":20,\"output_tokens\":20}}\n\n"))
	inspector.Inspect([]byte("data: {\"type\":\"message_delta\",\"usage\":{\"cache_read_input_tokens\":80,\"output_tokens_details\":{\"thinking_tokens\":12}}}\n\n"))
	inspector.Inspect([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	inspector.Finish()
	usage := inspector.Metadata().Usage
	if usage.InputTokens != 100 || usage.OutputTokens != 20 || usage.CachedInputTokens != 80 || usage.ReasoningTokens != 12 || usage.TotalTokens != 120 {
		t.Fatalf("merged stream usage = %#v", usage)
	}
}

func TestStreamInspectorMarksFirstGeneratedTokenOnce(t *testing.T) {
	tests := []struct {
		name     string
		protocol streamProtocol
		prelude  string
		delta    string
	}{
		{
			name: "responses text", protocol: streamProtocolResponses,
			prelude: `data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n" + `data: {"type":"response.output_text.delta","delta":""}` + "\n\n",
			delta:   `data: {"type":"response.output_text.delta","delta":"hello"}` + "\n\n",
		},
		{
			name: "responses custom tool input", protocol: streamProtocolResponses,
			prelude: `data: {"type":"response.custom_tool_call_input.delta","output_index":1,"item_id":"ctc_1","delta":""}` + "\n\n",
			delta:   `data: {"type":"response.custom_tool_call_input.delta","output_index":1,"item_id":"ctc_1","delta":"{}"}` + "\n\n",
		},
		{
			name: "responses encrypted reasoning item", protocol: streamProtocolResponses,
			prelude: `data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n",
			delta:   `data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning","status":"in_progress"}}` + "\n\n",
		},
		{
			name: "chat reasoning", protocol: streamProtocolChat,
			prelude: `data: {"choices":[{"delta":{"role":"assistant"}}]}` + "\n\n",
			delta:   `data: {"choices":[{"delta":{"reasoning_content":"thinking"}}]}` + "\n\n",
		},
		{
			name: "chat thinking_content", protocol: streamProtocolChat,
			prelude: `data: {"choices":[{"delta":{"role":"assistant"}}]}` + "\n\n",
			delta:   `data: {"choices":[{"delta":{"thinking_content":"thinking"}}]}` + "\n\n",
		},
		{
			name: "anthropic tool input", protocol: streamProtocolAnthropic,
			prelude: `data: {"type":"message_start","message":{"id":"msg_1"}}` + "\n\n",
			delta:   `data: {"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}` + "\n\n",
		},
		{
			name: "anthropic thinking start", protocol: streamProtocolAnthropic,
			prelude: `data: {"type":"message_start","message":{"id":"msg_1"}}` + "\n\n",
			delta:   `data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			marked := 0
			inspector := &responseInspector{protocol: test.protocol, onFirstToken: func() { marked++ }}
			inspector.Inspect([]byte(test.prelude))
			inspector.markFirstTokenForwarded()
			if marked != 0 {
				t.Fatalf("metadata marked first token %d times", marked)
			}
			inspector.Inspect([]byte(test.delta + test.delta))
			if marked != 0 {
				t.Fatalf("generated delta was marked before forwarding %d times", marked)
			}
			inspector.markFirstTokenForwarded()
			inspector.markFirstTokenForwarded()
			if marked != 1 {
				t.Fatalf("generated delta marked first token %d times", marked)
			}
		})
	}
}

func TestStreamInspectorDoesNotMarkImageEvents(t *testing.T) {
	marked := 0
	inspector := &responseInspector{protocol: streamProtocolImage, onFirstToken: func() { marked++ }}
	inspector.Inspect([]byte(`data: {"type":"image_generation.partial_image","partial_image_b64":"abc"}` + "\n\n"))
	inspector.markFirstTokenForwarded()
	if marked != 0 {
		t.Fatalf("image stream marked first token %d times", marked)
	}
}

func TestStreamInspectorMarksChatReasoningComment(t *testing.T) {
	marked := 0
	inspector := &responseInspector{protocol: streamProtocolChat, onFirstToken: func() { marked++ }}
	inspector.Inspect([]byte(": grok2api-reasoning-start\n\n"))
	if marked != 0 {
		t.Fatalf("reasoning comment marked first token before forwarding %d times", marked)
	}
	inspector.markFirstTokenForwarded()
	if marked != 1 {
		t.Fatalf("reasoning comment marked first token %d times", marked)
	}
}

func TestInternalSSEMarkerFilterAcrossChunkBoundaries(t *testing.T) {
	markers := reasoningStartSSEComment + "\n\n" + reasoningEvidenceSSEComment + "\n\n"
	input := []byte("data: before\n\n" + markers + "data: after\n\n")
	want := "data: before\n\ndata: after\n\n"
	for split := 0; split <= len(input); split++ {
		filter := internalSSEMarkerFilter{enabled: true}
		var output []byte
		output = append(output, filter.Filter(input[:split], false)...)
		output = append(output, filter.Filter(input[split:], false)...)
		output = append(output, filter.Filter(nil, true)...)
		if string(output) != want {
			t.Fatalf("split %d output = %q, want %q", split, output, want)
		}
	}
}

func TestCopyStreamConsumesInternalReasoningMarker(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	body := `data: {"choices":[{"delta":{"role":"assistant"}}]}` + "\n\n" +
		": grok2api-reasoning-start\n\n" +
		": grok2api-reasoning-evidence\n\n" +
		`data: {"choices":[{"delta":{"content":"hello"}}]}` + "\n\n" +
		"data: [DONE]\n\n"
	marked := 0
	if _, err := copyStream(context.Writer, strings.NewReader(body), streamProtocolChat, func() { marked++ }); err != nil {
		t.Fatal(err)
	}
	if marked != 1 {
		t.Fatalf("first token marked %d times", marked)
	}
	if strings.Contains(recorder.Body.String(), "grok2api-reasoning-") {
		t.Fatalf("internal marker leaked to client: %q", recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"content":"hello"`) {
		t.Fatalf("visible Chat delta missing: %q", recorder.Body.String())
	}
}

func TestCopyStreamConsumesAnthropicReasoningEvidenceMarker(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	body := ": grok2api-reasoning-evidence\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	if _, err := copyStream(context.Writer, strings.NewReader(body), streamProtocolAnthropic, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(recorder.Body.String(), "grok2api-reasoning-evidence") {
		t.Fatalf("internal Anthropic marker leaked to client: %q", recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"signature":"sig"`) {
		t.Fatalf("Anthropic signature delta missing: %q", recorder.Body.String())
	}
}

func TestCopyStreamPreservesBufferedTailOnReadError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	body := []byte(`data: {"choices":[{"delta":{"content":"partial"}}]}` + "\n\n:")
	_, err := copyStream(context.Writer, &chunkErrorReader{data: body}, streamProtocolChat, nil)
	if !errors.Is(err, errUpstreamStreamRead) {
		t.Fatalf("copy error = %v", err)
	}
	got := recorder.Body.Bytes()
	if !bytes.HasPrefix(got, body) {
		t.Fatalf("interrupted body prefix = %q, want %q", got, body)
	}
	if !bytes.Contains(got, []byte(`"code":"upstream_stream_interrupted"`)) || !bytes.Contains(got, []byte("data: [DONE]")) {
		t.Fatalf("interrupted body missing terminal SSE: %q", got)
	}
}

func TestClassifyCopyErrorDistinguishesClientFromUpstream(t *testing.T) {
	readErr := fmt.Errorf("%w: cut", errUpstreamStreamRead)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := classifyCopyError(canceled, readErr); got != "client_stream_interrupted" {
		t.Fatalf("client abort = %q", got)
	}
	if got := classifyCopyError(context.Background(), readErr); got != "upstream_stream_interrupted" {
		t.Fatalf("upstream abort = %q", got)
	}
	idle, stop := context.WithCancelCause(context.Background())
	stop(neterror.ErrUpstreamStreamIdleTimeout)
	idleErr := fmt.Errorf("%w: %w", errUpstreamStreamRead, neterror.ErrUpstreamStreamIdleTimeout)
	if got := classifyCopyError(idle, idleErr); got != "upstream_stream_idle_timeout" {
		t.Fatalf("idle timeout = %q", got)
	}
	loopErr := fmt.Errorf("%w: %w", errUpstreamStreamRead, neterror.ErrUpstreamOutputLoop)
	if got := classifyCopyError(context.Background(), loopErr); got != "upstream_output_loop" {
		t.Fatalf("output loop = %q", got)
	}
}

func TestCopyStreamWritesTerminalOnIdleTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name     string
		protocol streamProtocol
		want     []string
	}{
		{name: "chat", protocol: streamProtocolChat, want: []string{`"code":"upstream_stream_idle_timeout"`, "data: [DONE]"}},
		{name: "responses", protocol: streamProtocolResponses, want: []string{`"type":"response.failed"`, `"id":"resp_abort"`, `"created_at":`, `"sequence_number":`, `"code":"server_error"`, `"message":"upstream_stream_idle_timeout:`, `"status":"failed"`, `"model":"grok-test"`}},
		{name: "anthropic", protocol: streamProtocolAnthropic, want: []string{`"type":"error"`, "上游流式响应长时间无数据"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			_, err := copyStreamWithFallbackModel(context.Writer, &idleErrorReader{}, test.protocol, nil, "grok-test")
			if !errors.Is(err, errUpstreamStreamRead) || !errors.Is(err, neterror.ErrUpstreamStreamIdleTimeout) {
				t.Fatalf("copy error = %v", err)
			}
			got := recorder.Body.String()
			for _, fragment := range test.want {
				if !strings.Contains(got, fragment) {
					t.Fatalf("body %q missing %q", got, fragment)
				}
			}
		})
	}
}

func TestCopyStreamWritesTerminalOnOutputLoop(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name     string
		protocol streamProtocol
		want     []string
	}{
		{name: "chat", protocol: streamProtocolChat, want: []string{`"code":"upstream_output_loop"`, "data: [DONE]"}},
		{name: "responses", protocol: streamProtocolResponses, want: []string{`"type":"response.failed"`, `"code":"server_error"`, `"message":"upstream_output_loop:`, `"status":"failed"`}},
		{name: "anthropic", protocol: streamProtocolAnthropic, want: []string{`"type":"error"`, `"message":"upstream_output_loop: 上游输出陷入循环"`}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			_, err := copyStreamWithFallbackModel(context.Writer, &outputLoopErrorReader{}, test.protocol, nil, "grok-test")
			if !errors.Is(err, errUpstreamStreamRead) || !errors.Is(err, neterror.ErrUpstreamOutputLoop) {
				t.Fatalf("copy error = %v", err)
			}
			got := recorder.Body.String()
			for _, fragment := range test.want {
				if !strings.Contains(got, fragment) {
					t.Fatalf("body %q missing %q", got, fragment)
				}
			}
		})
	}
}

func TestCopyStreamFlushesUnterminatedResponsesTail(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	body := "event: response.completed\n" + `data: {"type":"response.completed","response":{"id":"resp_tail","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
	metadata, err := copyStream(context.Writer, strings.NewReader(body), streamProtocolResponses, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := recorder.Body.String()
	if !strings.Contains(got, `"type":"response.completed"`) || !strings.HasSuffix(got, "\n\n") {
		t.Fatalf("unterminated Responses tail was not flushed as an SSE event: %q", got)
	}
	if metadata.ResponseID != "resp_tail" || metadata.Usage.TotalTokens != 2 {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func TestCopyStreamFlushesUnterminatedResponsesTailBeforeAbort(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	body := []byte(`data: {"type":"response.output_text.delta","delta":"partial"}`)
	marked := 0
	metadata, err := copyStream(context.Writer, &chunkErrorReader{data: body}, streamProtocolResponses, func() {
		marked++
		if !strings.Contains(recorder.Body.String(), `"delta":"partial"`) {
			t.Fatalf("first token marked before the unterminated tail was flushed: %q", recorder.Body.String())
		}
	})
	if !errors.Is(err, errUpstreamStreamRead) {
		t.Fatalf("copy error = %v", err)
	}
	got := recorder.Body.String()
	if !strings.Contains(got, `"delta":"partial"`) || !strings.Contains(got, "\n\nevent: response.failed") {
		t.Fatalf("unterminated tail and abort event were not separated: %q", got)
	}
	if marked != 1 {
		t.Fatalf("first token marked %d times", marked)
	}
	if !metadata.Usage.OutputObserved {
		t.Fatal("forwarded unterminated delta was not recorded as observed output")
	}
}

func TestCopyStreamDropsMalformedResponsesTailBeforeAbort(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	malformed := []byte(`data: {"type":"response.output_text.delta","delta":"partial`)
	_, err := copyStream(context.Writer, &chunkErrorReader{data: malformed}, streamProtocolResponses, nil)
	if !errors.Is(err, errUpstreamStreamRead) {
		t.Fatalf("copy error = %v", err)
	}
	got := recorder.Body.String()
	if strings.Contains(got, string(malformed)) {
		t.Fatalf("malformed tail leaked before terminal event: %q", got)
	}
	if !strings.Contains(got, `"type":"response.failed"`) {
		t.Fatalf("abort event missing after malformed tail: %q", got)
	}
}

func TestCopyStreamDoesNotAppendAbortAfterUpstreamFailureTerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	body := []byte(`data: {"type":"response.failed","response":{"id":"resp_failed","status":"failed","error":{"message":"failed"}}}` + "\n\n")
	_, err := copyStream(context.Writer, &chunkErrorReader{data: body}, streamProtocolResponses, nil)
	if !errors.Is(err, errUpstreamStreamFailed) {
		t.Fatalf("copy error = %v", err)
	}
	got := recorder.Body.String()
	if strings.Count(got, `"type":"response.failed"`) != 1 || strings.Contains(got, `"type":"response.incomplete"`) {
		t.Fatalf("upstream terminal was followed by a second terminal event: %q", got)
	}
}

func TestCopyStreamWritesTerminalOnIncompleteEOF(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	_, err := copyStreamWithFallbackModel(context.Writer, strings.NewReader(""), streamProtocolResponses, nil, "grok-test")
	if !errors.Is(err, errUpstreamStreamIncomplete) {
		t.Fatalf("copy error = %v", err)
	}
	got := recorder.Body.String()
	if !strings.Contains(got, `"type":"response.failed"`) || !strings.Contains(got, `"code":"server_error"`) || !strings.Contains(got, `"message":"upstream_stream_incomplete:`) || !strings.Contains(got, `"model":"grok-test"`) {
		t.Fatalf("incomplete EOF missing terminal SSE event: %q", got)
	}
}

func TestSanitizeResponsesFillsMissingIDs(t *testing.T) {
	state := &responsesCompatState{model: "grok-test"}
	got := rewriteResponsesDataLine([]byte(`data: {"type":"response.output_item.added","item":{"type":"reasoning"}}`+"\n"), state)
	if !bytes.Contains(got, []byte(`"id":"item_1"`)) {
		t.Fatalf("item id not filled: %s", got)
	}
	got = rewriteResponsesDataLine([]byte(`data: {"type":"response.created","response":{"status":"in_progress"}}`+"\n"), state)
	if !bytes.Contains(got, []byte(`"id":"resp_abort"`)) || !bytes.Contains(got, []byte(`"created_at":`)) || !bytes.Contains(got, []byte(`"model":"grok-test"`)) {
		t.Fatalf("response id/created_at/model not filled: %s", got)
	}
}

func TestCopyStreamAbortTrailerAlwaysIncludesModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	_, err := copyStreamWithFallbackModel(context.Writer, &idleErrorReader{}, streamProtocolResponses, nil, "grok-test")
	if !errors.Is(err, errUpstreamStreamRead) {
		t.Fatalf("copy error = %v", err)
	}
	got := recorder.Body.String()
	if !strings.Contains(got, `"type":"response.failed"`) || !strings.Contains(got, `"model":"grok-test"`) {
		t.Fatalf("abort trailer missing model: %q", got)
	}
}

func TestCopyStreamAbortTrailerPrefersUpstreamModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	body := []byte(`data: {"type":"response.created","response":{"id":"resp_real","model":"upstream-model","status":"in_progress"}}` + "\n\n")
	_, err := copyStreamWithFallbackModel(context.Writer, &chunkErrorReader{data: body}, streamProtocolResponses, nil, "request-model")
	if !errors.Is(err, errUpstreamStreamRead) {
		t.Fatalf("copy error = %v", err)
	}
	got := recorder.Body.String()
	failedAt := strings.LastIndex(got, "event: response.failed")
	if failedAt < 0 {
		t.Fatalf("abort trailer missing: %q", got)
	}
	failed := got[failedAt:]
	if !strings.Contains(failed, `"model":"upstream-model"`) || strings.Contains(failed, `"model":"request-model"`) {
		t.Fatalf("abort trailer did not prefer upstream model: %q", failed)
	}
}

func TestResponsesAbortTrailerUsesStandardErrorShape(t *testing.T) {
	trailer := streamAbortTrailer(
		streamProtocolResponses,
		neterror.ErrUpstreamStreamIdleTimeout,
		responseMetadata{},
		&responsesCompatState{model: "grok-test"},
	)
	dataAt := bytes.Index(trailer, []byte("data: "))
	if dataAt < 0 {
		t.Fatalf("abort trailer missing data line: %q", trailer)
	}
	payload := bytes.TrimSpace(trailer[dataAt+len("data: "):])
	var event struct {
		Response struct {
			Error map[string]any `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatalf("decode abort trailer: %v; payload=%q", err, payload)
	}
	if event.Response.Error["code"] != "server_error" || !strings.Contains(stringAny(event.Response.Error["message"]), "upstream_stream_idle_timeout") {
		t.Fatalf("unexpected Responses error: %#v", event.Response.Error)
	}
	if _, exists := event.Response.Error["type"]; exists {
		t.Fatalf("Responses error contains non-protocol type: %#v", event.Response.Error)
	}
}

func TestSanitizeResponsesKeepsItemIDStableAcrossEvents(t *testing.T) {
	state := &responsesCompatState{}
	delta := rewriteResponsesDataLine([]byte(`data: {"type":"response.reasoning_text.delta","output_index":0,"delta":"plan"}`+"\n"), state)
	added := rewriteResponsesDataLine([]byte(`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning"}}`+"\n"), state)
	done := rewriteResponsesDataLine([]byte(`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning"}}`+"\n"), state)
	for name, got := range map[string][]byte{"added": added, "done": done, "delta": delta} {
		if !bytes.Contains(got, []byte(`"id":"item_1"`)) && !bytes.Contains(got, []byte(`"item_id":"item_1"`)) {
			t.Fatalf("%s event did not reuse item_1: %s", name, got)
		}
	}
}

func TestSanitizeResponsesKeepsGeneratedResponseIDStable(t *testing.T) {
	state := &responsesCompatState{}
	created := rewriteResponsesDataLine([]byte(`data: {"type":"response.created","response":{"status":"in_progress"}}`+"\n"), state)
	completed := rewriteResponsesDataLine([]byte(`data: {"type":"response.completed","id":"resp_late","response":{"id":"resp_late","status":"completed"}}`+"\n"), state)
	for name, got := range map[string][]byte{"created": created, "completed": completed} {
		if !bytes.Contains(got, []byte(`"id":"resp_abort"`)) || bytes.Contains(got, []byte("resp_late")) {
			t.Fatalf("%s event changed response identity: %s", name, got)
		}
	}
}

func TestSanitizeResponsesUsesExistingEventIDForMissingResponseID(t *testing.T) {
	state := &responsesCompatState{}
	got := rewriteResponsesDataLine([]byte(`data: {"type":"response.created","id":"resp_real","response":{"status":"in_progress"}}`+"\n"), state)
	if !bytes.Contains(got, []byte(`"id":"resp_real"`)) || bytes.Contains(got, []byte("resp_abort")) {
		t.Fatalf("existing event id was not reused for response: %s", got)
	}
}

func TestSanitizeResponsesKeepsResponseCreatedAtStable(t *testing.T) {
	state := &responsesCompatState{}
	created := rewriteResponsesDataLine([]byte(`data: {"type":"response.created","id":"resp_real","response":{"id":"resp_real","created_at":123,"status":"in_progress"}}`+"\n"), state)
	completed := rewriteResponsesDataLine([]byte(`data: {"type":"response.completed","id":"resp_real","response":{"id":"resp_real","created_at":456,"status":"completed"}}`+"\n"), state)
	for name, got := range map[string][]byte{"created": created, "completed": completed} {
		if !bytes.Contains(got, []byte(`"created_at":123`)) || bytes.Contains(got, []byte(`"created_at":456`)) {
			t.Fatalf("%s event changed response creation time: %s", name, got)
		}
	}
}

func TestSanitizeResponsesDoesNotAddResponseIDToErrorEvent(t *testing.T) {
	state := &responsesCompatState{}
	got := rewriteResponsesDataLine([]byte(`data: {"type":"error","code":"server_error","message":"failed"}`+"\n"), state)
	if bytes.Contains(got, []byte(`"id":`)) {
		t.Fatalf("Responses error event gained a non-protocol response id: %s", got)
	}
}

func TestSanitizeResponsesFillsMissingOutputTextAnnotations(t *testing.T) {
	state := &responsesCompatState{}
	added := rewriteResponsesDataLine([]byte(`data: {"type":"response.output_item.added","item":{"type":"message","content":[{"type":"output_text","text":"hi"}]}}`+"\n"), state)
	if !bytes.Contains(added, []byte(`"annotations":[]`)) {
		t.Fatalf("item output_text missing annotations: %s", added)
	}
	completed := rewriteResponsesDataLine([]byte(`data: {"type":"response.completed","response":{"id":"resp_1","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}}`+"\n"), state)
	if !bytes.Contains(completed, []byte(`"annotations":[]`)) {
		t.Fatalf("completed output_text missing annotations: %s", completed)
	}
	part := rewriteResponsesDataLine([]byte(`data: {"type":"response.content_part.added","part":{"type":"output_text","text":"hi"}}`+"\n"), state)
	if !bytes.Contains(part, []byte(`"annotations":[]`)) {
		t.Fatalf("content_part output_text missing annotations: %s", part)
	}
	kept := rewriteResponsesDataLine([]byte(`data: {"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"hi","annotations":[{"type":"url_citation","url":"https://x.test"}]}]}}`+"\n"), state)
	if !bytes.Contains(kept, []byte(`"url":"https://x.test"`)) || bytes.Contains(kept, []byte(`"annotations":[]`)) {
		t.Fatalf("existing annotations were replaced: %s", kept)
	}
}

func TestResponsesCompatBoundsUnterminatedLineBuffer(t *testing.T) {
	state := &responsesCompatState{}
	chunk := bytes.Repeat([]byte{'x'}, maxStreamEventInspectionBytes+1)
	got := rewriteResponsesStreamChunk(chunk, state)
	if len(got) != len(chunk) || len(state.pending) != 0 || !state.passingLongLine {
		t.Fatalf("oversized line was retained: output=%d pending=%d passing=%t", len(got), len(state.pending), state.passingLongLine)
	}
	got = rewriteResponsesStreamChunk([]byte("tail\n"), state)
	if string(got) != "tail\n" || state.passingLongLine {
		t.Fatalf("oversized line passthrough did not finish: %q passing=%t", got, state.passingLongLine)
	}
}

func TestResponseInspectorTreatsOversizedDataLineAsObservedOutput(t *testing.T) {
	inspector := &responseInspector{protocol: streamProtocolResponses}
	inspector.Inspect(append([]byte("data: "), bytes.Repeat([]byte{'x'}, maxStreamEventInspectionBytes)...))
	if !inspector.Metadata().Usage.OutputObserved {
		t.Fatal("forwarded oversized SSE data line was still classified as empty")
	}
}

func TestCopyStreamMarksFirstTokenAfterFlush(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	body := `data: {"type":"response.reasoning_text.delta","delta":"thinking"}` + "\n\n" +
		`data: {"type":"response.completed","response":{"usage":{"output_tokens":1}}}` + "\n\n"
	marked := 0
	_, err := copyStream(context.Writer, strings.NewReader(body), streamProtocolResponses, func() {
		marked++
		if !recorder.Flushed || !strings.Contains(recorder.Body.String(), `"delta":"thinking"`) {
			t.Fatalf("first token was marked before the generated delta was flushed: flushed=%v body=%q", recorder.Flushed, recorder.Body.String())
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if marked != 1 {
		t.Fatalf("first token marked %d times", marked)
	}
}

func BenchmarkFirstTokenInspection(b *testing.B) {
	tests := []struct {
		name     string
		protocol streamProtocol
		data     []byte
	}{
		{name: "responses", protocol: streamProtocolResponses, data: []byte(`{"type":"response.output_text.delta","delta":"hello"}`)},
		{name: "responses custom tool", protocol: streamProtocolResponses, data: []byte(`{"type":"response.custom_tool_call_input.delta","delta":"{}"}`)},
		{name: "chat", protocol: streamProtocolChat, data: []byte(`{"choices":[{"delta":{"content":"hello"}}]}`)},
		{name: "anthropic", protocol: streamProtocolAnthropic, data: []byte(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`)},
	}
	for _, test := range tests {
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if !containsGeneratedDelta(test.data, test.protocol) {
					b.Fatal("generated delta not detected")
				}
			}
		})
	}
}

func TestAnthropicUsageReconstructsCacheCreationAndSaturates(t *testing.T) {
	metadata := responseMetadata{
		Usage:                    gateway.Usage{InputTokens: 20, CachedInputTokens: 70, OutputTokens: 5},
		cacheCreationInputTokens: 10,
	}
	usage := normalizeMetadataUsage(metadata, streamProtocolAnthropic).Usage
	if usage.InputTokens != 100 || usage.CachedInputTokens != 70 || usage.TotalTokens != 105 {
		t.Fatalf("anthropic reconstructed usage = %#v", usage)
	}

	overflow := responseMetadata{Usage: gateway.Usage{InputTokens: math.MaxInt64, CachedInputTokens: 1, OutputTokens: 1}}
	usage = normalizeMetadataUsage(overflow, streamProtocolAnthropic).Usage
	if usage.InputTokens != math.MaxInt64 || usage.TotalTokens != math.MaxInt64 {
		t.Fatalf("anthropic saturated usage = %#v", usage)
	}
}

func TestExtractMetadataPreservesReportedZeroUsage(t *testing.T) {
	metadata := extractMetadata([]byte(`{"id":"resp_zero","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}`))
	if !metadata.Usage.Reported {
		t.Fatalf("zero usage object was treated as missing: %#v", metadata.Usage)
	}
	if metadata.Usage.InputTokens != 0 || metadata.Usage.OutputTokens != 0 || metadata.Usage.TotalTokens != 0 {
		t.Fatalf("zero usage object changed values: %#v", metadata.Usage)
	}

	missing := extractMetadata([]byte(`{"id":"resp_missing"}`))
	if missing.Usage.Reported {
		t.Fatalf("missing usage object was treated as reported: %#v", missing.Usage)
	}
}

func TestCopyJSONReconstructsAnthropicTotalInputForAudit(t *testing.T) {
	payload := []byte(`{"id":"msg_1","type":"message","model":"grok-4.5","usage":{"input_tokens":10899,"output_tokens":227,"cache_creation_input_tokens":0,"cache_read_input_tokens":229504}}`)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)

	metadata, err := copyJSON(context.Writer, bytes.NewReader(payload), streamProtocolAnthropic)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(recorder.Body.Bytes(), payload) {
		t.Fatalf("forwarded body = %s", recorder.Body.String())
	}
	usage := metadata.Usage
	if usage.InputTokens != 240403 || usage.CachedInputTokens != 229504 || usage.OutputTokens != 227 || usage.TotalTokens != 240630 {
		t.Fatalf("anthropic audit usage = %#v", usage)
	}
}

func TestStreamInspectorAcceptsChatCachedOnlyFrame(t *testing.T) {
	inspector := &responseInspector{protocol: streamProtocolChat}
	inspector.Inspect([]byte("data: {\"usage\":{\"prompt_tokens\":40,\"completion_tokens\":5,\"total_tokens\":45,\"prompt_tokens_details\":{\"cached_tokens\":25}}}\n\n"))
	inspector.Inspect([]byte("data: [DONE]\n\n"))
	inspector.Finish()
	usage := inspector.Metadata().Usage
	if usage.CachedInputTokens != 25 || usage.InputTokens != 40 || usage.TotalTokens != 45 {
		t.Fatalf("chat stream cached usage = %#v", usage)
	}
}

func TestUsageInspectorHandlesChunkedSSE(t *testing.T) {
	inspector := &responseInspector{}
	inspector.Inspect([]byte("data: {\"response\":{\"id\":\"resp_stream\",\"usage\":{\"input_tokens\":2,"))
	inspector.Inspect([]byte("\"output_tokens\":3}}}\n\n"))
	metadata := inspector.Metadata()
	usage := metadata.Usage
	if usage.TotalTokens != 5 {
		t.Fatalf("usage = %#v", usage)
	}
	if metadata.ResponseID != "resp_stream" {
		t.Fatalf("response ID = %q", metadata.ResponseID)
	}
}

func TestUsageInspectorHandlesFinalEventWithoutNewline(t *testing.T) {
	inspector := &responseInspector{}
	inspector.Inspect([]byte(`data: {"response":{"id":"resp_final","usage":{"input_tokens":7,"output_tokens":4}}}`))
	inspector.Finish()
	metadata := inspector.Metadata()
	if metadata.ResponseID != "resp_final" || metadata.Usage.TotalTokens != 11 {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func TestCopyStreamRequiresProtocolTerminalEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name           string
		protocol       streamProtocol
		body           string
		wantErr        error
		wantDiagnostic bool
	}{
		{
			name: "responses completed", protocol: streamProtocolResponses,
			body: `data: {"type":"response.completed","response":{"id":"resp_ok","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}` + "\n\n",
		},
		{
			name: "responses eof before completed", protocol: streamProtocolResponses,
			body:    `data: {"type":"response.created","response":{"id":"resp_cut"}}` + "\n\n",
			wantErr: errUpstreamStreamIncomplete,
		},
		{
			name: "responses failed", protocol: streamProtocolResponses,
			body:    `data: {"type":"response.failed","response":{"id":"resp_failed","status":"failed","error":{"code":"upstream_error","message":"failed"},"output":[{"type":"reasoning","encrypted_content":"must-not-be-audited"}]}}` + "\n\n",
			wantErr: errUpstreamStreamFailed, wantDiagnostic: true,
		},
		{name: "chat done", protocol: streamProtocolChat, body: "data: [DONE]\n\n"},
		{name: "chat error", protocol: streamProtocolChat, body: `data: {"type":"error","error":{"code":"server_error","message":"chat failed"}}` + "\n\n", wantErr: errUpstreamStreamFailed, wantDiagnostic: true},
		{name: "anthropic stop", protocol: streamProtocolAnthropic, body: `data: {"type":"message_stop"}` + "\n\n"},
		{name: "anthropic error", protocol: streamProtocolAnthropic, body: `data: {"type":"error","error":{"type":"api_error","message":"messages failed"}}` + "\n\n", wantErr: errUpstreamStreamFailed, wantDiagnostic: true},
		{name: "image completed", protocol: streamProtocolImage, body: `data: {"type":"image_generation.completed"}` + "\n\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			metadata, err := copyStream(context.Writer, strings.NewReader(test.body), test.protocol, nil)
			if test.wantErr == nil && err != nil {
				t.Fatal(err)
			}
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %#v, want %v", err, test.wantErr)
			}
			if test.name == "responses completed" && (metadata.ResponseID != "resp_ok" || metadata.Usage.TotalTokens != 5) {
				t.Fatalf("metadata = %#v", metadata)
			}
			if test.wantDiagnostic {
				if metadata.StreamFailure == nil || !strings.Contains(string(metadata.StreamFailure.Body), "failed") || strings.Contains(string(metadata.StreamFailure.Body), "must-not-be-audited") {
					t.Fatalf("stream failure diagnostic = %#v", metadata.StreamFailure)
				}
			} else if metadata.StreamFailure != nil {
				t.Fatalf("unexpected stream failure diagnostic = %#v", metadata.StreamFailure)
			}
			if test.protocol != streamProtocolResponses && recorder.Body.String() != test.body {
				t.Fatalf("forwarded = %q", recorder.Body.String())
			}
			if test.protocol == streamProtocolResponses && !strings.Contains(recorder.Body.String(), `"type":`) {
				t.Fatalf("responses stream missing type: %q", recorder.Body.String())
			}
		})
	}
}

func TestWriteResultRecordsStreamFailureDiagnostic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(nil, nil, 1<<20)
	stream := `data: {"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"upstream failed"}}}` + "\n\n"
	var finalCode string
	var diagnostic *gateway.StreamFailureDiagnostic
	result := &gateway.Result{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(stream)),
		RecordStreamFailure: func(value gateway.StreamFailureDiagnostic) {
			diagnostic = &value
		},
		Finalize: func(_ gateway.Usage, _, code string) {
			finalCode = code
		},
	}
	router := gin.New()
	router.GET("/", func(c *gin.Context) {
		handler.writeResult(c, result, true, streamProtocolResponses)
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"type":"response.failed"`) || finalCode != "upstream_stream_error" {
		t.Fatalf("status=%d body=%q final=%q", recorder.Code, recorder.Body.String(), finalCode)
	}
	if diagnostic == nil || !strings.Contains(string(diagnostic.Body), `"code":"server_error"`) {
		t.Fatalf("diagnostic = %#v", diagnostic)
	}
}

func TestWriteResultClientAbortKeepsUpstreamStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(nil, nil, 1<<20)
	var finalCode string
	result := &gateway.Result{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(&chunkErrorReader{data: []byte("data: {\"type\":\"response.created\"}\n\n")}),
		Finalize:   func(_ gateway.Usage, _, code string) { finalCode = code },
	}
	router := gin.New()
	router.POST("/", func(c *gin.Context) {
		handler.writeResult(c, result, true, streamProtocolResponses)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx))
	if recorder.Code != http.StatusOK || finalCode != "client_stream_interrupted" {
		t.Fatalf("status=%d finalize=%q body=%q", recorder.Code, finalCode, recorder.Body.String())
	}
}

func TestWriteResultUpstreamCutStaysUpstreamInterrupted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(nil, nil, 1<<20)
	var finalCode string
	result := &gateway.Result{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(&chunkErrorReader{data: []byte("data: {\"type\":\"response.created\"}\n\n")}),
		Finalize:   func(_ gateway.Usage, _, code string) { finalCode = code },
	}
	router := gin.New()
	router.POST("/", func(c *gin.Context) {
		handler.writeResult(c, result, true, streamProtocolResponses)
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/", nil))
	if recorder.Code != http.StatusOK || finalCode != "upstream_stream_interrupted" {
		t.Fatalf("status=%d finalize=%q body=%q", recorder.Code, finalCode, recorder.Body.String())
	}
}

func TestWriteProtocolResultMapsJSONInvalidArgumentWithoutCopyStream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(nil, nil, 1<<20)
	body := `{"code":"invalid-argument","error":"mcp__codex_app__automation_update: tool parameter root must be an object type (root schema is an anyOf/oneOf union with a non-object branch)"}`
	var finalCode string
	recorded := false
	result := &gateway.Result{
		StatusCode: http.StatusBadRequest,
		Status:     "400 Bad Request",
		Header:     http.Header{"Content-Type": {"application/json"}, "Content-Length": {fmt.Sprintf("%d", len(body))}},
		Body:       io.NopCloser(strings.NewReader(body)),
		RecordStreamFailure: func(gateway.StreamFailureDiagnostic) {
			recorded = true
		},
		Finalize: func(_ gateway.Usage, _, code string) {
			finalCode = code
		},
	}
	router := gin.New()
	router.GET("/", func(c *gin.Context) {
		handler.writeResponsesResult(c, result, true, "")
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusBadRequest || recorded || finalCode != "invalid_argument" {
		t.Fatalf("status=%d recorded=%v final=%q body=%s", recorder.Code, recorded, finalCode, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "mcp__codex_app__automation_update") || strings.Contains(recorder.Body.String(), "upstream_stream_incomplete") {
		t.Fatalf("body=%s", recorder.Body.String())
	}
}

func TestWriteProtocolResultAnthropicJSONReadFailureKeepsAnthropicShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(nil, nil, 1<<20)
	var finalCode string
	result := &gateway.Result{
		StatusCode: http.StatusBadRequest,
		Status:     "400 Bad Request",
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(&chunkErrorReader{data: []byte(`{"error":"partial"}`)}),
		Finalize: func(_ gateway.Usage, _, code string) {
			finalCode = code
		},
	}
	router := gin.New()
	router.GET("/", func(c *gin.Context) {
		handler.writeAnthropicResult(c, result, true)
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	var payload struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusBadGateway || payload.Type != "error" || payload.Error.Type != "api_error" || payload.Error.Code != "upstream_error" || finalCode != "upstream_error" {
		t.Fatalf("status=%d payload=%#v final=%q body=%s", recorder.Code, payload, finalCode, recorder.Body.String())
	}
}

func TestWriteProtocolResultDoesNotRemapOtherJSON4xxAsServerError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(nil, nil, 1<<20)
	body := `{"error":{"code":"not_found","message":"requested response is missing"}}`
	var finalCode string
	result := &gateway.Result{
		StatusCode: http.StatusNotFound,
		Status:     "404 Not Found",
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Finalize: func(_ gateway.Usage, _, code string) {
			finalCode = code
		},
	}
	router := gin.New()
	router.GET("/", func(c *gin.Context) {
		handler.writeResponsesResult(c, result, true, "")
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusNotFound || finalCode != "upstream_error" || !strings.Contains(recorder.Body.String(), "requested response is missing") {
		t.Fatalf("status=%d final=%q body=%s", recorder.Code, finalCode, recorder.Body.String())
	}
}

func TestWriteResultOutputLoopFinalizesDistinctCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(nil, nil, 1<<20)
	var finalCode string
	result := &gateway.Result{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(&outputLoopErrorReader{}),
		Finalize:   func(_ gateway.Usage, _, code string) { finalCode = code },
	}
	router := gin.New()
	router.POST("/", func(c *gin.Context) {
		handler.writeResult(c, result, true, streamProtocolResponses)
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/", nil))
	if recorder.Code != http.StatusOK || finalCode != "upstream_output_loop" || !strings.Contains(recorder.Body.String(), `"message":"upstream_output_loop:`) {
		t.Fatalf("status=%d finalize=%q body=%q", recorder.Code, finalCode, recorder.Body.String())
	}
}

func TestProjectStreamFailureDiagnosticBoundsErrorMessage(t *testing.T) {
	diagnostic := projectStreamFailureDiagnostic([]byte(`{"type":"error","error":{"code":"server_error","message":"` + strings.Repeat("错误", maxStreamFailureDiagnosticBytes) + `"},"output":"must-not-be-audited"}`))
	if !diagnostic.BodyTruncated || len(diagnostic.Body) > maxStreamFailureDiagnosticBytes || len(diagnostic.Body) == 0 || !utf8.Valid(diagnostic.Body) || strings.Contains(string(diagnostic.Body), "must-not-be-audited") {
		t.Fatalf("diagnostic length=%d truncated=%v", len(diagnostic.Body), diagnostic.BodyTruncated)
	}
}

func TestExtractMetadataPreservesLargeCostTicks(t *testing.T) {
	metadata := extractMetadata([]byte(`{"id":"resp_cost","model":"grok-4.5","usage":{"input_tokens":1,"output_tokens":1,"cost_in_usd_ticks":9007199254740993}}`))
	if metadata.Usage.CostInUSDTicks != 9_007_199_254_740_993 {
		t.Fatalf("cost ticks = %d", metadata.Usage.CostInUSDTicks)
	}
}

func TestCopyHeadersFiltersHopByHopAndUpstreamCookies(t *testing.T) {
	source := http.Header{
		"Connection":          {"X-Upstream-Internal"},
		"Content-Type":        {"application/json"},
		"Set-Cookie":          {"upstream_session=secret"},
		"X-Models-Etag":       {`"upstream-account-catalog"`},
		"X-Request-Id":        {"req_123"},
		"X-Upstream-Internal": {"hidden"},
	}
	destination := make(http.Header)

	copyHeaders(destination, source)

	if destination.Get("Content-Type") != "application/json" || destination.Get("X-Request-Id") != "req_123" {
		t.Fatalf("forwarded headers = %#v", destination)
	}
	if destination.Get("Set-Cookie") != "" || destination.Get("X-Models-Etag") != "" || destination.Get("X-Upstream-Internal") != "" || destination.Get("Connection") != "" {
		t.Fatalf("filtered headers leaked = %#v", destination)
	}
}

func TestCopyJSONForwardsBodyBeyondMetadataInspectionLimit(t *testing.T) {
	payload := make([]byte, 0, maxJSONMetadataInspectionBytes+1024)
	payload = append(payload, []byte(`{"padding":"`)...)
	payload = append(payload, bytes.Repeat([]byte("a"), maxJSONMetadataInspectionBytes)...)
	payload = append(payload, []byte(`"}`)...)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)

	metadata, err := copyJSON(context.Writer, bytes.NewReader(payload), streamProtocolResponses)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(recorder.Body.Bytes(), payload) {
		t.Fatalf("forwarded body size = %d, want %d", recorder.Body.Len(), len(payload))
	}
	if metadata.ResponseID != "" || metadata.Usage.TotalTokens != 0 {
		t.Fatalf("metadata should be skipped after inspection limit: %#v", metadata)
	}
}

func TestCopyMediaRejectsUnknownLengthOverflowWithoutWritingPastLimit(t *testing.T) {
	payload := bytes.Repeat([]byte("v"), 33)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	err := writeMediaBody(context, bytes.NewReader(payload), "video/mp4", http.StatusOK, 32)
	if !errors.Is(err, errResponseTransferLimit) {
		t.Fatalf("copy error = %v", err)
	}
	if recorder.Body.Len() != 32 {
		t.Fatalf("forwarded media size = %d", recorder.Body.Len())
	}
}

func TestCopyMediaAllowsExactLimit(t *testing.T) {
	payload := bytes.Repeat([]byte("v"), 32)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	if err := writeMediaBody(context, bytes.NewReader(payload), "video/mp4", http.StatusOK, 32); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(recorder.Body.Bytes(), payload) {
		t.Fatalf("forwarded media = %q", recorder.Body.Bytes())
	}
	if recorder.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("content type = %q", recorder.Header().Get("Content-Type"))
	}
}

func TestSelectionErrorResponseDistinguishesCoolingAndSaturation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name       string
		failure    *gateway.SelectionUnavailableError
		status     int
		code       string
		retryAfter string
	}{
		{name: "cooling", failure: &gateway.SelectionUnavailableError{Reason: gateway.SelectionCooling, RetryAfter: 1500 * time.Millisecond}, status: http.StatusTooManyRequests, code: "upstream_cooling", retryAfter: "2"},
		{name: "model cooling", failure: &gateway.SelectionUnavailableError{Reason: gateway.SelectionModelCooling, RetryAfter: time.Second}, status: http.StatusTooManyRequests, code: "upstream_model_cooling", retryAfter: "1"},
		{name: "saturated", failure: &gateway.SelectionUnavailableError{Reason: gateway.SelectionSaturated, RetryAfter: time.Second}, status: http.StatusServiceUnavailable, code: "upstream_saturated", retryAfter: "1"},
		{name: "pinned unavailable", failure: &gateway.SelectionUnavailableError{Reason: gateway.SelectionPinnedUnavailable, AccountID: 9}, status: http.StatusServiceUnavailable, code: "upstream_pinned_account_unavailable"},
		{name: "scoped account range", failure: &gateway.SelectionUnavailableError{Reason: gateway.SelectionNoAccounts, Scope: clientkeydomain.AccountScope{Providers: clientkeydomain.ProviderScopeBuild, Tiers: clientkeydomain.TierScopeFree}}, status: http.StatusServiceUnavailable, code: "client_key_account_scope_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			status, code, _ := selectionErrorResponse(context, test.failure)
			if status != test.status || code != test.code || recorder.Header().Get("Retry-After") != test.retryAfter {
				t.Fatalf("status=%d code=%q retry-after=%q", status, code, recorder.Header().Get("Retry-After"))
			}
		})
	}
}

func TestReferenceToVideoRequestValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &Handler{maxBodyBytes: 1 << 20}
	router := gin.New()
	handler.Register(router.Group("/v1"))

	combined := httptest.NewRequest(http.MethodPost, "/v1/videos/generations", strings.NewReader(`{
		"model":"grok-imagine-video","prompt":"x",
		"image":{"url":"https://example.com/a.png"},
		"reference_images":[{"url":"https://example.com/b.png"}]
	}`))
	combined.Header.Set("Content-Type", "application/json")
	combinedRec := httptest.NewRecorder()
	router.ServeHTTP(combinedRec, combined)
	if combinedRec.Code != http.StatusBadRequest || !strings.Contains(combinedRec.Body.String(), "不能与") {
		t.Fatalf("combined image+refs status=%d body=%s", combinedRec.Code, combinedRec.Body.String())
	}

	highRes := httptest.NewRequest(http.MethodPost, "/v1/videos/generations", strings.NewReader(`{
		"model":"grok-imagine-video","prompt":"x","resolution":"1080p",
		"reference_images":[{"url":"https://example.com/b.png"}]
	}`))
	highRes.Header.Set("Content-Type", "application/json")
	highResRec := httptest.NewRecorder()
	router.ServeHTTP(highResRec, highRes)
	if highResRec.Code != http.StatusBadRequest || !strings.Contains(highResRec.Body.String(), "720p") {
		t.Fatalf("r2v 1080p status=%d body=%s", highResRec.Code, highResRec.Body.String())
	}

	tooManyAudio := httptest.NewRequest(http.MethodPost, "/v1/videos/generations", strings.NewReader(`{
		"model":"grok-imagine-video","prompt":"x","duration":8,"resolution":"720p",
		"reference_audios":[{"voice_id":"eve"},{"voice_id":"ara"},{"voice_id":"rex"},{"voice_id":"sal"}]
	}`))
	tooManyAudio.Header.Set("Content-Type", "application/json")
	tooManyAudioRec := httptest.NewRecorder()
	router.ServeHTTP(tooManyAudioRec, tooManyAudio)
	if tooManyAudioRec.Code != http.StatusBadRequest || !strings.Contains(tooManyAudioRec.Body.String(), "最多 3") {
		t.Fatalf("too many audios status=%d body=%s", tooManyAudioRec.Code, tooManyAudioRec.Body.String())
	}

	valid := httptest.NewRequest(http.MethodPost, "/v1/videos/generations", strings.NewReader(`{
		"model":"grok-imagine-video","prompt":"The person speaks","duration":8,
		"aspect_ratio":"9:16","resolution":"720p",
		"reference_images":[{"url":"https://example.com/p.png"}],
		"reference_audios":[{"voice_id":"eve"}]
	}`))
	valid.Header.Set("Content-Type", "application/json")
	validRec := httptest.NewRecorder()
	router.ServeHTTP(validRec, valid)
	if validRec.Code != http.StatusUnauthorized {
		t.Fatalf("valid r2v status=%d body=%s", validRec.Code, validRec.Body.String())
	}
}
func TestEditAndExtendVideoRequestValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &Handler{maxBodyBytes: 1 << 20}
	router := gin.New()
	group := router.Group("/v1")
	handler.Register(group)

	// Model compatibility is resolved from the selected upstream route, not the
	// caller-controlled public model name.
	rec := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(rec)
	writeGatewayError(context, gateway.ErrVideoOperationUnsupported)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unsupported video route status = %d body = %s", rec.Code, rec.Body.String())
	}

	// missing video on extend
	req := httptest.NewRequest(http.MethodPost, "/v1/videos/extensions", strings.NewReader(`{"model":"grok-imagine-video","prompt":"x","duration":4}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("extend missing video status = %d body = %s", rec.Code, rec.Body.String())
	}

	// invalid extend duration
	req = httptest.NewRequest(http.MethodPost, "/v1/videos/extensions", strings.NewReader(`{"model":"grok-imagine-video","prompt":"x","duration":15,"video":{"url":"https://example.com/a.mp4"}}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("extend bad duration status = %d body = %s", rec.Code, rec.Body.String())
	}
}
