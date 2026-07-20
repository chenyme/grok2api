package gateway

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestSummarizeResponseMediaScansMessagesAndFunctionOutputs(t *testing.T) {
	summary, err := summarizeResponseMedia([]byte(`{
		"input":[
			{"type":"message","role":"user","content":[
				{"type":"input_text","text":"hello"},
				{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}
			]},
			{"type":"function_call_output","call_id":"call_1","output":[
				{"type":"input_text","text":"tool"},
				{"type":"input_image","image_url":"data:image/jpeg;base64,QUJD"},
				{"type":"input_image","file_id":"file_1"}
			]}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	want := responseMediaSummary{InputImages: 3, ImageBytes: 8, ContentArrays: 2, TextBytes: 9}
	if summary != want {
		t.Fatalf("summary = %#v, want %#v", summary, want)
	}
}

func TestSummarizeResponseMediaSupportsChatAndAnthropicBlocks(t *testing.T) {
	summary, err := summarizeResponseMedia([]byte(`{
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"chat"},
				{"type":"image_url","image_url":{"url":"data:image/webp;base64,QUJDRA=="}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tool_1","content":[
					{"type":"text","text":"anthropic"},
					{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}
				]}
			]}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	want := responseMediaSummary{InputImages: 2, ImageBytes: 9, ContentArrays: 3, TextBytes: 13}
	if summary != want {
		t.Fatalf("summary = %#v, want %#v", summary, want)
	}
}

func TestSummarizeResponseMediaDoesNotInterpretArbitraryImageContent(t *testing.T) {
	summary, err := summarizeResponseMedia([]byte(`{
		"input":[{"type":"function_call_output","call_id":"call_1","output":{"ImageContent":{"data":"aGVsbG8="}}}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if summary != (responseMediaSummary{}) {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestResponseMediaSummaryLogContainsOnlyMetadata(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	logResponseMediaSummary(logger, "req-media", responseMediaSummary{
		InputImages: 42, ImageBytes: 33_030_144, ContentArrays: 42, TextBytes: 840,
	})
	logged := output.String()
	for _, expected := range []string{
		"request_media_input_summary", "request_id=req-media", "media_input_images=42",
		"media_input_image_bytes=33030144", "media_content_arrays=42", "media_text_bytes=840",
	} {
		if !strings.Contains(logged, expected) {
			t.Fatalf("log missing %q: %s", expected, logged)
		}
	}
	for _, forbidden := range []string{"base64", "Authorization", "Cookie", "account", `C:\\`, `D:\\`, `E:\\`} {
		if strings.Contains(logged, forbidden) {
			t.Fatalf("log contains forbidden value %q: %s", forbidden, logged)
		}
	}
}

func TestPureTextContentArraysSkipMediaAuditLog(t *testing.T) {
	body := []byte(`{
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"describe this image"}]},
			{"type":"function_call_output","call_id":"call_1","output":[{"type":"input_text","text":"done"}]}
		]
	}`)
	summary, err := summarizeResponseMedia(body)
	if err != nil {
		t.Fatal(err)
	}
	if summary.InputImages != 0 || summary.ContentArrays != 2 {
		t.Fatalf("summary = %#v", summary)
	}

	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	logResponseMediaSummary(logger, "req-text", summary)
	if output.Len() != 0 {
		t.Fatalf("unexpected media audit log: %s", output.String())
	}
}

func TestDecodedBase64Bytes(t *testing.T) {
	for encoded, want := range map[string]int64{
		"aGVsbG8=": 5,
		"QUJD":     3,
		"QUI":      2,
		"QQ":       1,
		"bad!":     0,
		"QQ=A":     0,
	} {
		if got := decodedBase64Bytes(encoded); got != want {
			t.Fatalf("decodedBase64Bytes(%q) = %d, want %d", encoded, got, want)
		}
	}
}
