package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestApplyFreeWebVideoDurationCap(t *testing.T) {
	tests := []struct {
		name       string
		seconds    int
		cap        int
		credential account.Credential
		want       int
	}{
		{name: "basic over cap", seconds: 10, cap: 6, credential: account.Credential{WebTier: account.WebTierBasic}, want: 6},
		{name: "basic at cap", seconds: 6, cap: 6, credential: account.Credential{WebTier: account.WebTierBasic}, want: 6},
		{name: "basic under cap", seconds: 4, cap: 6, credential: account.Credential{WebTier: account.WebTierBasic}, want: 4},
		{name: "super not capped", seconds: 10, cap: 6, credential: account.Credential{WebTier: account.WebTierSuper}, want: 10},
		{name: "heavy not capped", seconds: 10, cap: 6, credential: account.Credential{WebTier: account.WebTierHeavy}, want: 10},
		{name: "auto without billing remains unknown", seconds: 10, cap: 6, credential: account.Credential{WebTier: account.WebTierAuto}, want: 10},
		{name: "zero cap uses default 6", seconds: 10, cap: 0, credential: account.Credential{WebTier: account.WebTierBasic}, want: 6},
		{name: "custom cap 8", seconds: 12, cap: 8, credential: account.Credential{WebTier: account.WebTierBasic}, want: 8},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := applyFreeWebVideoDurationCap(test.seconds, test.cap, test.credential)
			if got != test.want {
				t.Fatalf("got %d want %d", got, test.want)
			}
		})
	}
}

func TestGenerateVideoCapsBasicCredentialBeforeUpstream(t *testing.T) {
	observedDuration := 0
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/rest/app-chat/conversations/new" {
			http.NotFound(writer, request)
			return
		}
		requestCount++
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode video payload: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		mediaGenInput, _ := payload["mediaGenInput"].(map[string]any)
		textToVideo, _ := mediaGenInput["textToVideo"].(map[string]any)
		duration, _ := textToVideo["duration"].(float64)
		observedDuration = int(duration)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, `data: {"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoPostId":"post_1","videoUrl":"/videos/final.mp4"}}}}`+"\n\n")
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
	adapter := NewAdapter(Config{
		BaseURL: server.URL, StatsigMode: "manual", StatsigManualValue: "test",
		VideoTimeoutSeconds: 5, FreeVideoDurationCap: 6,
	}, infraegress.NewManager(egressRepositoryStub{}, cipher), cipher, nil, nil)

	credential := account.Credential{
		ID: 1, Provider: account.ProviderWeb, WebTier: account.WebTierBasic,
		EncryptedAccessToken: encryptedToken,
	}
	result, err := adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: credential,
		Prompt:     "test", Duration: 10,
	})
	if err != nil || result.URL != "https://assets.grok.com/videos/final.mp4" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if observedDuration != 6 {
		t.Fatalf("Basic Web request upstream duration = %d, want 6", observedDuration)
	}

	_, err = adapter.GenerateVideo(context.Background(), provider.VideoRequest{
		Credential: credential,
		Prompt:     "test",
		Duration:   16,
	})
	if err == nil {
		t.Fatal("duration above API range was accepted after free-tier cap")
	}
	var stageError *provider.VideoStageError
	if !errors.As(err, &stageError) || stageError.Stage != provider.VideoStagePrepare {
		t.Fatalf("error = %v, want prepare-stage validation failure", err)
	}
	if requestCount != 1 {
		t.Fatalf("out-of-range duration reached upstream; request count = %d, want 1", requestCount)
	}
}
