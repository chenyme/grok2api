package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
)

func TestDecodeVideoOptionHintsStrictValidation(t *testing.T) {
	valid, err := decodeVideoOptionHints("```json\n{\"duration\":10,\"aspect_ratio\":\"9:16\",\"resolution\":\"1080P\"}\n```")
	if err != nil || valid.Duration == nil || *valid.Duration != 10 || valid.AspectRatio == nil || *valid.AspectRatio != "9:16" || valid.Resolution == nil || *valid.Resolution != "1080p" {
		t.Fatalf("valid = %#v, err = %v", valid, err)
	}
	for _, value := range []string{
		`{"duration":16,"aspect_ratio":null,"resolution":null}`,
		`{"duration":8,"aspect_ratio":"21:9","resolution":"720p"}`,
		`{"duration":8,"aspect_ratio":"16:9","resolution":"4k"}`,
		`{"duration":8,"aspect_ratio":"16:9","resolution":"720p","prompt":"rewritten"}`,
		`{"duration":8,"aspect_ratio":"16:9","resolution":"720p"} trailing`,
	} {
		if _, err := decodeVideoOptionHints(value); err == nil {
			t.Fatalf("expected invalid hints for %s", value)
		}
	}
}

func TestBuildVideoOptionRequestPreservesArbitraryLanguageAndOnlyRequestedFields(t *testing.T) {
	prompt := "اصنع مقطعًا عموديًا مدته ١٠ ثوانٍ بدقة ١٠٨٠p عن المحيط"
	body, err := buildVideoOptionRequest(VideoOptionParseInput{
		Prompt: prompt, NeedDuration: true, NeedResolution: true,
	}, "Console/grok-4.3")
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Model != "Console/grok-4.3" || len(payload.Messages) != 2 || !strings.Contains(payload.Messages[1].Content, prompt) {
		t.Fatalf("payload = %#v", payload)
	}
	if strings.Contains(payload.Messages[1].Content, "aspect_ratio") {
		t.Fatalf("unrequested field leaked into extraction list: %q", payload.Messages[1].Content)
	}
}

func TestReadVideoOptionResultFinalizesUsage(t *testing.T) {
	finalized := false
	result := &Result{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(`{
			"id":"chatcmpl_options","model":"grok-4.6",
			"choices":[{"message":{"content":"{\"duration\":12,\"aspect_ratio\":null,\"resolution\":\"720p\"}"}}],
			"usage":{"prompt_tokens":20,"completion_tokens":8,"total_tokens":28}
		}`)),
		Finalize: func(usage Usage, responseID, errorCode string) {
			finalized = responseID == "chatcmpl_options" && errorCode == "" && usage.InputTokens == 20 && usage.OutputTokens == 8 && usage.TotalTokens == 28 && usage.ResponseModel == "grok-4.6"
		},
	}
	hints, err := readVideoOptionResult(result)
	if err != nil || hints.Duration == nil || *hints.Duration != 12 || hints.Resolution == nil || *hints.Resolution != "720p" || !finalized {
		t.Fatalf("hints = %#v, err = %v, finalized = %t", hints, err, finalized)
	}
}

func TestVideoOptionRoutePriorityPrefersQualityThenProvider(t *testing.T) {
	priorityValues := normalizeVideoOptionModelPriority(nil)
	priority := make(map[string]int, len(priorityValues))
	for index, value := range priorityValues {
		priority[strings.ToLower(value)] = index
	}
	grok46 := modeldomain.Route{Provider: account.ProviderBuild, UpstreamModel: "grok-4.6"}
	grok45 := modeldomain.Route{Provider: account.ProviderBuild, UpstreamModel: "grok-4.5"}
	unknownConsole := modeldomain.Route{Provider: account.ProviderConsole, UpstreamModel: "future-text"}
	unknownWeb := modeldomain.Route{Provider: account.ProviderWeb, UpstreamModel: "future-text"}
	if videoOptionRoutePriority(grok46, priority, len(priority)) >= videoOptionRoutePriority(grok45, priority, len(priority)) {
		t.Fatal("grok-4.6 must precede grok-4.5")
	}
	if videoOptionRoutePriority(unknownConsole, priority, len(priority)) != len(priority) || videoOptionRoutePriority(unknownWeb, priority, len(priority)) != len(priority) {
		t.Fatal("unknown enabled text models must remain in the fallback set")
	}
	if videoOptionProviderPriority(account.ProviderConsole) >= videoOptionProviderPriority(account.ProviderWeb) {
		t.Fatal("Console fallback must precede Web at equal quality")
	}
}
