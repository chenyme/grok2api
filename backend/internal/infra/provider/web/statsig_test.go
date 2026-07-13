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
		if payload.Method != "POST" || payload.Path != "/rest/app-chat/conversations/id/responses" || payload.Environment.MetaContent != "meta-value" {
			t.Fatalf("payload=%#v", payload)
		}
		body, _ := json.Marshal(map[string]string{"x-statsig-id": encoded})
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body))), Header: http.Header{}}, nil
	})}
	value, err := signer.requestSignature(context.Background(), "https://signer.example/sign", "post", "/rest/app-chat/conversations/id/responses", "meta-value")
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
	for _, endpoint := range []string{"https://grok.wodf.de/sign", "https://signer.example/sign"} {
		if err := validateStatsigSignerEndpoint(context.Background(), endpoint); err != nil {
			t.Fatalf("endpoint %q rejected: %v", endpoint, err)
		}
	}
	for _, endpoint := range []string{
		"http://grok.wodf.de/sign",
		"https://user:pass@grok.wodf.de/sign",
		"https://localhost/sign",
		"https://127.0.0.1/sign",
		"https://10.0.0.1/sign",
		"https://grok.wodf.de:8443/sign",
		"https://grok.wodf.de/sign?token=value",
	} {
		if err := validateStatsigSignerEndpoint(context.Background(), endpoint); err == nil {
			t.Fatalf("unsafe endpoint %q accepted", endpoint)
		}
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
