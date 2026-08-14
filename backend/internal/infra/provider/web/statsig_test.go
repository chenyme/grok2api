package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

type failingStatsigBody struct{}

func (failingStatsigBody) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (failingStatsigBody) Close() error             { return nil }

func TestFetchStatsigMetaContentFallsBackToRootWhenIndexHasNoVerification(t *testing.T) {
	var paths []string
	do := func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.Path)
		switch request.URL.Path {
		case "/index":
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(`<html><head><meta name="robots" content="noindex"/></head></html>`)), Header: http.Header{}}, nil
		case "/":
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`<html><head><meta name="grok-site-verification" content="root-meta"/></head></html>`)), Header: http.Header{}}, nil
		default:
			t.Fatalf("unexpected path %q", request.URL.Path)
			return nil, nil
		}
	}
	value, err := fetchStatsigMetaContentWithDo(context.Background(), "https://grok.com", "sso-token", &infraegress.Lease{UserAgent: "test-agent"}, do)
	if err != nil || value != "root-meta" {
		t.Fatalf("value=%q err=%v paths=%v", value, err, paths)
	}
	if len(paths) != 2 || paths[0] != "/index" || paths[1] != "/" {
		t.Fatalf("paths=%v", paths)
	}
}

func TestFetchStatsigMetaContentDoesNotFallbackWhenIndex404HasVerification(t *testing.T) {
	var paths []string
	do := func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.Path)
		if request.URL.Path != "/index" {
			t.Fatalf("unexpected fallback to %q", request.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(`<html><head><meta name="grok-site―verification" content="index-meta"/></head></html>`)), Header: http.Header{}}, nil
	}
	value, err := fetchStatsigMetaContentWithDo(context.Background(), "https://grok.com", "sso-token", &infraegress.Lease{UserAgent: "test-agent"}, do)
	if err != nil || value != "index-meta" {
		t.Fatalf("value=%q err=%v", value, err)
	}
	if len(paths) != 1 || paths[0] != "/index" {
		t.Fatalf("paths=%v", paths)
	}
}

func TestFetchStatsigMetaContentDoesNotFallbackWhenSuccessfulIndexHasNoVerification(t *testing.T) {
	var paths []string
	do := func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.Path)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`<html><head></head></html>`)), Header: http.Header{}}, nil
	}
	_, err := fetchStatsigMetaContentWithDo(context.Background(), "https://grok.com", "sso-token", &infraegress.Lease{UserAgent: "test-agent"}, do)
	if !errors.Is(err, errStatsigMetaMissing) {
		t.Fatalf("err=%v", err)
	}
	if len(paths) != 1 || paths[0] != "/index" {
		t.Fatalf("successful index without verification must not fall back: paths=%v", paths)
	}
}

func TestFetchStatsigMetaContentRejectsNon404IndexStatuses(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		for _, withMeta := range []bool{false, true} {
			t.Run(fmt.Sprintf("status_%d_meta_%t", status, withMeta), func(t *testing.T) {
				body := `<html><head></head></html>`
				if withMeta {
					body = `<html><head><meta name="grok-site-verification" content="error-meta"/></head></html>`
				}
				var calls int
				do := func(request *http.Request) (*http.Response, error) {
					calls++
					if request.URL.Path != "/index" {
						t.Fatalf("unexpected fallback to %q", request.URL.Path)
					}
					return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
				}
				_, err := fetchStatsigMetaContentWithDo(context.Background(), "https://grok.com", "sso-token", &infraegress.Lease{UserAgent: "test-agent"}, do)
				if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("Grok index 返回 %d", status)) {
					t.Fatalf("err=%v", err)
				}
				if calls != 1 {
					t.Fatalf("calls=%d", calls)
				}
			})
		}
	}
}

func TestFetchStatsigMetaContentRequiresSuccessfulRoot(t *testing.T) {
	for _, withMeta := range []bool{false, true} {
		t.Run(fmt.Sprintf("meta_%t", withMeta), func(t *testing.T) {
			body := `<html><head></head></html>`
			if withMeta {
				body = `<html><head><meta name="grok-site-verification" content="root-error-meta"/></head></html>`
			}
			do := func(request *http.Request) (*http.Response, error) {
				if request.URL.Path == "/index" {
					return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(`<html><head></head></html>`)), Header: http.Header{}}, nil
				}
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
			}
			_, err := fetchStatsigMetaContentWithDo(context.Background(), "https://grok.com", "sso-token", &infraegress.Lease{UserAgent: "test-agent"}, do)
			if err == nil || !strings.Contains(err.Error(), "Grok 首页返回 503") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestFetchStatsigMetaContentRequiresVerificationOnSuccessfulRoot(t *testing.T) {
	do := func(request *http.Request) (*http.Response, error) {
		status := http.StatusOK
		if request.URL.Path == "/index" {
			status = http.StatusNotFound
		}
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(`<html><head></head></html>`)),
			Header:     http.Header{},
		}, nil
	}
	_, err := fetchStatsigMetaContentWithDo(context.Background(), "https://grok.com", "sso-token", &infraegress.Lease{UserAgent: "test-agent"}, do)
	if !errors.Is(err, errStatsigMetaMissing) {
		t.Fatalf("err=%v", err)
	}
}

func TestFetchStatsigMetaContentDoesNotFallbackOnTransportReadOrOversizeFailure(t *testing.T) {
	t.Run("transport", func(t *testing.T) {
		var calls int
		do := func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("dial failed")
		}
		_, err := fetchStatsigMetaContentWithDo(context.Background(), "https://grok.com", "sso-token", &infraegress.Lease{UserAgent: "test-agent"}, do)
		if err == nil || err.Error() != "dial failed" || calls != 1 {
			t.Fatalf("err=%v calls=%d", err, calls)
		}
	})

	t.Run("read", func(t *testing.T) {
		var calls int
		do := func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: http.StatusNotFound, Body: failingStatsigBody{}, Header: http.Header{}}, nil
		}
		_, err := fetchStatsigMetaContentWithDo(context.Background(), "https://grok.com", "sso-token", &infraegress.Lease{UserAgent: "test-agent"}, do)
		if err == nil || err.Error() != "read failed" || calls != 1 {
			t.Fatalf("err=%v calls=%d", err, calls)
		}
	})

	t.Run("oversize", func(t *testing.T) {
		var calls int
		do := func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", statsigMetaBodyLimit+1))), Header: http.Header{}}, nil
		}
		_, err := fetchStatsigMetaContentWithDo(context.Background(), "https://grok.com", "sso-token", &infraegress.Lease{UserAgent: "test-agent"}, do)
		if err == nil || !strings.Contains(err.Error(), "超过安全上限") || calls != 1 {
			t.Fatalf("err=%v calls=%d", err, calls)
		}
	})
}

func TestExtractStatsigMetaContentAcceptsCurrentMetaName(t *testing.T) {
	for _, name := range []string{"grok-site―verification", "grok-site-verification"} {
		body := []byte(`<html><head><meta name="` + name + `" content="meta-value"/></head></html>`)
		value, err := extractStatsigMetaContent(body)
		if err != nil || value != "meta-value" {
			t.Fatalf("name=%q value=%q err=%v", name, value, err)
		}
	}
}

func TestStatsigSignerSendsMethodPathAndMetaContent(t *testing.T) {
	raw := make([]byte, 70)
	encoded := base64.RawStdEncoding.EncodeToString(raw)
	signer := newStatsigSigner()
	signer.validateEndpoint = func(context.Context, string) error { return nil }
	signer.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var payload struct {
			Method      string `json:"method"`
			Path        string `json:"path"`
			Environment struct {
				MetaContent string `json:"metaContent"`
			} `json:"environment"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Method != "POST" || payload.Path != "/rest/app-chat/conversations/new" || payload.Environment.MetaContent != "meta-value" {
			t.Fatalf("payload=%#v", payload)
		}
		body, _ := json.Marshal(map[string]string{"x-statsig-id": encoded})
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body))), Header: http.Header{}}, nil
	})}
	value, err := signer.requestSignature(context.Background(), "https://signer.example/sign", "post", "/rest/app-chat/conversations/new", "meta-value")
	if err != nil || value != encoded {
		t.Fatalf("value=%q err=%v", value, err)
	}
}

func TestStatsigSignerRejectsInvalidShape(t *testing.T) {
	signer := newStatsigSigner()
	signer.validateEndpoint = func(context.Context, string) error { return nil }
	signer.client = &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"x-statsig-id":"invalid"}`)), Header: http.Header{}}, nil
	})}
	if _, err := signer.requestSignature(context.Background(), "https://signer.example/sign", "POST", "/rest/test", "meta"); err == nil {
		t.Fatal("invalid signature was accepted")
	}
}

func TestValidateStatsigSignerEndpointUsesAdminURLBoundary(t *testing.T) {
	for _, endpoint := range []string{
		"https://grok.wodf.de/sign",
		"https://signer.example/sign",
		"http://grok-signer-go:8788/sign",
		"http://host.docker.internal:8788/sign",
		"http://127.0.0.1:8788/sign",
		"https://10.0.0.1:8443/sign",
	} {
		if err := validateStatsigSignerEndpoint(context.Background(), endpoint); err != nil {
			t.Fatalf("endpoint %q rejected: %v", endpoint, err)
		}
	}
	for _, endpoint := range []string{
		"http://grok.wodf.de/sign",
		"https://user:pass@grok.wodf.de/sign",
		"https://grok.wodf.de:8443/sign",
		"https://grok.wodf.de/sign?token=value",
		"http://8.8.8.8:8788/sign",
	} {
		if err := validateStatsigSignerEndpoint(context.Background(), endpoint); err == nil {
			t.Fatalf("unsafe endpoint %q accepted", endpoint)
		}
	}
}

func TestStatsigSignerClientRejectsRedirects(t *testing.T) {
	signer := newStatsigSigner()
	request, err := http.NewRequest(http.MethodGet, "http://grok-signer-go:8788/redirect", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := signer.client.CheckRedirect(request, []*http.Request{request}); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect policy error = %v", err)
	}
}

func TestStatsigSignerCachesByMethodAndPathForOneHour(t *testing.T) {
	var fetches int
	var signedMeta []string
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	signer := newStatsigSigner()
	signer.now = func() time.Time { return now }
	signer.validateEndpoint = func(context.Context, string) error { return nil }
	signer.fetchMeta = func(context.Context, string, string, *infraegress.Lease) (string, error) {
		fetches++
		return fmt.Sprintf("meta-%d", fetches), nil
	}
	signer.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var payload struct {
			Environment struct {
				MetaContent string `json:"metaContent"`
			} `json:"environment"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		signedMeta = append(signedMeta, payload.Environment.MetaContent)
		raw := make([]byte, 70)
		raw[0] = byte(len(signedMeta))
		encoded := base64.RawStdEncoding.EncodeToString(raw)
		body, _ := json.Marshal(map[string]string{"x-statsig-id": encoded})
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body))), Header: http.Header{}}, nil
	})}
	first, _, err := signer.Sign(context.Background(), "https://grok.com", "https://signer.example/sign", "token-a", nil, http.MethodPost, "https://grok.com/rest/test")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := signer.Sign(context.Background(), "https://grok.com", "https://signer.example/sign", "token-b", nil, http.MethodPost, "https://grok.com/rest/test")
	if err != nil {
		t.Fatal(err)
	}
	if fetches != 1 || len(signedMeta) != 1 || first != second {
		t.Fatalf("cached fetches=%d signedMeta=%v first=%q second=%q", fetches, signedMeta, first, second)
	}

	now = now.Add(time.Hour)
	third, _, err := signer.Sign(context.Background(), "https://grok.com", "https://signer.example/sign", "token-b", nil, http.MethodPost, "https://grok.com/rest/test")
	if err != nil {
		t.Fatal(err)
	}
	if fetches != 2 || third == first {
		t.Fatalf("hourly refresh fetches=%d first=%q third=%q", fetches, first, third)
	}

	signer.Invalidate("https://grok.com", "https://signer.example/sign", http.MethodPost, "https://grok.com/rest/test")
	fourth, _, err := signer.Sign(context.Background(), "https://grok.com", "https://signer.example/sign", "token-a", nil, http.MethodPost, "https://grok.com/rest/test")
	if err != nil {
		t.Fatal(err)
	}
	if fetches != 3 || fourth == third {
		t.Fatalf("invalidation fetches=%d third=%q fourth=%q", fetches, third, fourth)
	}

	if _, _, err := signer.Sign(context.Background(), "https://grok.com", "https://signer.example/sign", "token-a", nil, http.MethodPost, "https://grok.com/rest/other"); err != nil {
		t.Fatal(err)
	}
	if fetches != 4 {
		t.Fatalf("different path reused signature: fetches=%d", fetches)
	}
}

func TestStatsigWarmupFetchesMetaOnceForSharedPaths(t *testing.T) {
	var fetches, signatures int
	signer := newStatsigSigner()
	signer.validateEndpoint = func(context.Context, string) error { return nil }
	signer.fetchMeta = func(context.Context, string, string, *infraegress.Lease) (string, error) {
		fetches++
		return "shared-meta", nil
	}
	signer.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		signatures++
		var payload struct {
			Environment struct {
				MetaContent string `json:"metaContent"`
			} `json:"environment"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Environment.MetaContent != "shared-meta" {
			t.Fatalf("meta = %q", payload.Environment.MetaContent)
		}
		raw := make([]byte, 70)
		raw[0] = byte(signatures)
		body, _ := json.Marshal(map[string]string{"x-statsig-id": base64.RawStdEncoding.EncodeToString(raw)})
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body))), Header: http.Header{}}, nil
	})}
	targets := []statsigWarmTarget{
		{method: http.MethodPost, target: "https://grok.com/rest/chat"},
		{method: http.MethodPost, target: "https://grok.com/rest/rate-limits"},
		{method: http.MethodPost, target: "https://grok.com/rest/media/post/create"},
	}
	warmed, err := signer.Warm(context.Background(), "https://grok.com", "https://signer.example/sign", "token", nil, targets)
	if err != nil {
		t.Fatal(err)
	}
	if warmed != len(targets) || fetches != 1 || signatures != len(targets) {
		t.Fatalf("warmed=%d fetches=%d signatures=%d", warmed, fetches, signatures)
	}
	if warmedAgain, err := signer.Warm(context.Background(), "https://grok.com", "https://signer.example/sign", "token", nil, targets); err != nil || warmedAgain != 0 || fetches != 1 {
		t.Fatalf("cached warmup=%d fetches=%d err=%v", warmedAgain, fetches, err)
	}
}

func TestApplySignedStatsigUsesManualValue(t *testing.T) {
	value := base64.RawStdEncoding.EncodeToString(make([]byte, 70))
	adapter := &Adapter{cfg: Config{BaseURL: "https://grok.com", StatsigMode: "manual", StatsigManualValue: value}}
	request, err := http.NewRequest(http.MethodPost, "https://grok.com/rest/test", nil)
	if err != nil {
		t.Fatal(err)
	}
	adapter.applySignedStatsig(context.Background(), request, "token", nil)
	if request.Header.Get("x-statsig-id") != value {
		t.Fatalf("x-statsig-id = %q", request.Header.Get("x-statsig-id"))
	}
}

func TestStatsigInvalidationDoesNotReuseRejectedValue(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	raw := make([]byte, 70)
	raw[0] = 1
	previous := base64.RawStdEncoding.EncodeToString(raw)
	signer := newStatsigSigner()
	signer.now = func() time.Time { return now }
	key, _, err := statsigSignatureKey("https://grok.com", "https://signer.example/sign", http.MethodPost, "https://grok.com/rest/test")
	if err != nil {
		t.Fatal(err)
	}
	signer.store(key, previous, now.Add(time.Hour), now)
	signer.fetchMeta = func(context.Context, string, string, *infraegress.Lease) (string, error) {
		return "", errors.New("signer unavailable")
	}
	signer.Invalidate("https://grok.com", "https://signer.example/sign", http.MethodPost, "https://grok.com/rest/test")
	value, source, err := signer.Sign(context.Background(), "https://grok.com", "https://signer.example/sign", "token", nil, http.MethodPost, "https://grok.com/rest/test")
	if err == nil || value != "" || source != "" {
		t.Fatalf("value=%q source=%q err=%v", value, source, err)
	}
}

func TestApplySignedStatsigNeverLeavesRandomFallback(t *testing.T) {
	adapter := &Adapter{cfg: Config{BaseURL: "https://grok.com", StatsigMode: "manual", StatsigManualValue: "invalid"}}
	request, err := http.NewRequest(http.MethodPost, "https://grok.com/rest/test", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("x-statsig-id", "random-fallback")
	adapter.applySignedStatsig(context.Background(), request, "token", nil)
	if value := request.Header.Get("x-statsig-id"); value != "" {
		t.Fatalf("x-statsig-id = %q", value)
	}
}

func TestStatsigInvalidationOnlyAppliesToURLMode(t *testing.T) {
	manual := &Adapter{cfg: Config{StatsigMode: "manual"}, statsig: newStatsigSigner()}
	if manual.invalidateSignedStatsig(http.MethodPost, "https://grok.com/rest/test") {
		t.Fatal("manual Statsig must not be invalidated automatically")
	}
	urlMode := &Adapter{cfg: Config{BaseURL: "https://grok.com", StatsigMode: "url", StatsigSignerURL: "https://signer.example/sign"}, statsig: newStatsigSigner()}
	if !urlMode.invalidateSignedStatsig(http.MethodPost, "https://grok.com/rest/test") {
		t.Fatal("URL Statsig must be invalidated after anti-bot rejection")
	}
}
