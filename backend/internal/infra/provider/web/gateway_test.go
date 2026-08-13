package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
)

func TestGatewayEndpointAndHeadersMatchBrowserProtocol(t *testing.T) {
	const userID = "497f19f8-49d4-458a-bee4-43ec3dcaf8ca"
	endpoint, origin, err := gatewayEndpoint("https://grok.com", userID)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "wss://grok.com/ws/mgw/?uid="+userID || origin != "https://grok.com" {
		t.Fatalf("endpoint=%q origin=%q", endpoint, origin)
	}
	headers := gatewayHeaders(origin, userID, "test-sso", &infraegress.Lease{UserAgent: "test-agent", CFCookies: "cf_clearance=test-cf"})
	cookie := headers.Get("Cookie")
	for _, expected := range []string{"sso=test-sso", "sso-rw=test-sso", "x-userid=" + userID, "cf_clearance=test-cf"} {
		if !strings.Contains(cookie, expected) {
			t.Fatalf("cookie %q missing %q", cookie, expected)
		}
	}
	if headers.Get("Authorization") != "" || headers.Get("DPoP") != "" {
		t.Fatalf("unexpected authorization headers: %#v", headers)
	}
}

func TestGatewaySessionSupportsNewAndExistingConversations(t *testing.T) {
	fresh := gatewaySession("fast", nil)
	freshXGrok := fresh["x_grok"].(map[string]any)
	if fresh["model"] != "fast" || freshXGrok["is_temporary"] != true || freshXGrok["load_existing"] != nil {
		t.Fatalf("fresh session = %#v", fresh)
	}
	previous := &inferencedomain.WebResponseState{ConversationID: "conversation-1", UpstreamParentResponseID: "response-1"}
	existing := gatewaySession("expert", previous)
	existingXGrok := existing["x_grok"].(map[string]any)
	if existing["model"] != "expert" || existingXGrok["conversation_id"] != "conversation-1" || existingXGrok["load_existing"] != true || existingXGrok["needs_history"] != false {
		t.Fatalf("existing session = %#v", existing)
	}
}

func TestGatewayTurnEventsOmitCastleAndPreserveAttachments(t *testing.T) {
	previous := &inferencedomain.WebResponseState{UpstreamParentResponseID: "response-1"}
	item, response := gatewayTurnEvents("conversation-1", "hello", []string{"file-1"}, previous)
	itemEvent := item["event"].(map[string]any)
	if item["session_id"] != "conversation-1" || itemEvent["parent_response_id"] != "response-1" {
		t.Fatalf("item event = %#v", item)
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, expected := range []string{`"file_attachment_ids":["file-1"]`, `"file_mention":{"file_id":"file-1"}`, `"text":{"text":"hello"}`} {
		if !strings.Contains(text, expected) {
			t.Fatalf("item JSON %s missing %s", text, expected)
		}
	}
	responseJSON, _ := json.Marshal(response)
	if strings.Contains(string(responseJSON), "castle_request_token") {
		t.Fatalf("response.create unexpectedly contains Castle token: %s", responseJSON)
	}
}

func TestParseGatewayEventsCollectsConversationTextAndParent(t *testing.T) {
	parsed := &parsedChat{}
	frames := []string{
		`{"event":{"type":"conversation.attached","conversation":{"id":"conversation-1"}}}`,
		`{"event":{"type":"response.chunk","chunk":{"text":{"text":"TOKEN","channel":"CHANNEL_ASSISTANT_RESPONSE"}}}}`,
		`{"event":{"type":"response.chunk","chunk":{"text":{"text":"LESS","channel":"CHANNEL_ASSISTANT_RESPONSE"}}}}`,
		`{"event":{"type":"response.chunk","chunk":{"text":{"text":"thought","channel":"CHANNEL_ANALYSIS"}}}}`,
		`{"event":{"type":"response.done","response":{"id":"response-1","status":"completed"}}}`,
	}
	var emitted strings.Builder
	for _, frame := range frames {
		kind, delta, err := parseUpstreamFrame([]byte(frame), parsed)
		if err != nil {
			t.Fatal(err)
		}
		if kind == "text" {
			emitted.WriteString(delta)
		}
	}
	if parsed.ConversationID != "conversation-1" || parsed.ParentID != "response-1" || parsed.Text.String() != "TOKENLESS" || emitted.String() != "TOKENLESS" || parsed.Reasoning.String() != "thought" {
		t.Fatalf("parsed = conversation=%q parent=%q text=%q emitted=%q reasoning=%q", parsed.ConversationID, parsed.ParentID, parsed.Text.String(), emitted.String(), parsed.Reasoning.String())
	}
}

func TestParseGatewayErrorUsesExistingClassification(t *testing.T) {
	_, _, err := parseUpstreamFrame([]byte(`{"event":{"type":"error","error":{"code":"anti_bot","message":"anti-bot rejected"}}}`), &parsedChat{})
	if !errors.Is(err, errWebAntiBot) {
		t.Fatalf("error = %v", err)
	}
}

func TestParseGatewayChunkCollectsToolResultsAndRenderCitations(t *testing.T) {
	parsed := &parsedChat{}
	frames := []string{
		`{"event":{"type":"response.chunk","chunk":{"tool_usage_card":{"tool_usage_card_id":"tool-1","web_search":{"args":{"query":"grok 4.6"}}}}}}`,
		`{"event":{"type":"response.chunk","chunk":{"tool_result":{"tool_call_id":"tool-1","web_search":{"webpages":[{"url":"https://www.ithome.com/0/981/947.htm","title":"IT之家 Grok 4.6","snippet":"..."}]}}}}}`,
		`{"event":{"type":"response.chunk","chunk":{"tool_usage_card":{"tool_usage_card_id":"tool-2","x_search":{"args":{"query":"from:elonmusk"}}}}}}`,
		`{"event":{"type":"response.chunk","chunk":{"tool_result":{"tool_call_id":"tool-2","x_post":{"posts":[{"userhandle":"elonmusk","name":"Elon Musk","text":"And Grok 4.6 comes out in a week","post_id":"2082707547203518569"}]}}}}}`,
		`{"event":{"type":"response.chunk","chunk":{"text":{"text":"预计发布","channel":"CHANNEL_ASSISTANT_RESPONSE"}}}}`,
		`{"event":{"type":"response.chunk","chunk":{"render_citation":{"id":"c1","kind":"CITATION_KIND_X_POST","url":"https://x.com/elonmusk/status/2082707547203518569","citation_id":1}}}}`,
		`{"event":{"type":"response.chunk","chunk":{"render_citation":{"id":"c2","kind":"CITATION_KIND_WEB_PAGE","url":"https://www.ithome.com/0/981/947.htm","citation_id":0}}}}`,
		`{"event":{"type":"response.chunk","chunk":{"text":{"text":"。","channel":"CHANNEL_ASSISTANT_RESPONSE"}}}}`,
	}
	var emitted strings.Builder
	for _, frame := range frames {
		kind, delta, err := parseUpstreamFrame([]byte(frame), parsed)
		if err != nil {
			t.Fatal(err)
		}
		if kind == "text" {
			emitted.WriteString(delta)
		}
	}
	if parsed.ServerTools != 2 || parsed.WebSearchTools != 1 {
		t.Fatalf("tools server=%d web=%d", parsed.ServerTools, parsed.WebSearchTools)
	}
	if len(parsed.SearchSources) < 2 {
		t.Fatalf("search sources = %#v", parsed.SearchSources)
	}
	if len(parsed.HostedSearchCalls) < 2 {
		t.Fatalf("hosted search calls = %#v", parsed.HostedSearchCalls)
	}
	if parsed.HostedSearchCalls[0].Kind != "web_search" || parsed.HostedSearchCalls[0].Status != "completed" {
		t.Fatalf("web call = %#v", parsed.HostedSearchCalls[0])
	}
	if parsed.HostedSearchCalls[1].Kind != "x_search" || parsed.HostedSearchCalls[1].Query == "" || parsed.HostedSearchCalls[1].Status != "completed" {
		t.Fatalf("x call = %#v", parsed.HostedSearchCalls[1])
	}
	if len(parsed.Annotations) != 2 {
		t.Fatalf("annotations = %#v", parsed.Annotations)
	}
	text := parsed.Text.String()
	if !strings.Contains(text, "预计发布") || !strings.Contains(text, "[[1]](https://x.com/elonmusk/status/2082707547203518569)") || !strings.Contains(text, "[[2]](https://www.ithome.com/0/981/947.htm)") {
		t.Fatalf("text = %q emitted = %q", text, emitted.String())
	}
	first := parsed.Annotations[0]
	// xAI Responses title is the visible citation number; source_title is kept
	// internally so Chat Completions can expose the page title.
	wantTitle := "Elon Musk: And Grok 4.6 comes out in a week"
	if first["type"] != "url_citation" || first["url"] != "https://x.com/elonmusk/status/2082707547203518569" || first["title"] != "1" || first["source_title"] != wantTitle {
		t.Fatalf("first annotation = %#v want title %q", first, wantTitle)
	}
	chatPayload := buildOpenAIResult("chat", "resp_1", "grok-chat-fast", *parsed, false)
	msg := chatPayload["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["annotations"] == nil {
		t.Fatalf("chat annotations missing: %#v", msg)
	}
	// Chat Completions: nested url_citation; title is page name.
	firstAnn := msg["annotations"].([]any)[0].(map[string]any)
	nested, _ := firstAnn["url_citation"].(map[string]any)
	if firstAnn["type"] != "url_citation" || nested == nil || nested["url"] == nil || nested["title"] != wantTitle {
		t.Fatalf("chat annotation shape = %#v", firstAnn)
	}
	citations, _ := chatPayload["citations"].([]string)
	if len(citations) < 2 {
		// JSON marshal may use []any after rebuild — accept both
		if raw, ok := chatPayload["citations"].([]any); !ok || len(raw) < 2 {
			t.Fatalf("citations missing: %#v", chatPayload["citations"])
		}
	}
	if chatPayload["search_sources"] != nil {
		t.Fatalf("search_sources must not be emitted: %#v", chatPayload["search_sources"])
	}

	respPayload := buildOpenAIResult(conversation.OperationResponses, "resp_1", "grok-chat-fast", *parsed, false)
	if respPayload["search_sources"] != nil {
		t.Fatalf("responses search_sources must not be emitted")
	}
	if respPayload["citations"] == nil {
		t.Fatalf("responses citations missing: %#v", respPayload)
	}
	if respPayload["server_side_tool_usage"] == nil {
		t.Fatalf("server_side_tool_usage missing: %#v", respPayload)
	}
	out := respPayload["output"].([]any)
	if len(out) < 3 {
		t.Fatalf("output should include search calls + message: %#v", out)
	}
	if out[0].(map[string]any)["type"] != "web_search_call" {
		t.Fatalf("first output item = %#v", out[0])
	}
	if out[1].(map[string]any)["type"] != "x_search_call" {
		t.Fatalf("second output item = %#v", out[1])
	}
	webAction := out[0].(map[string]any)["action"].(map[string]any)
	if webAction["type"] != "search" || webAction["query"] == nil {
		t.Fatalf("web action = %#v", webAction)
	}
	webSources, _ := webAction["sources"].([]map[string]any)
	if len(webSources) == 0 {
		// buildOpenAIResult may re-box as []any depending on path
		if raw, ok := webAction["sources"].([]any); ok && len(raw) > 0 {
			firstSrc, _ := raw[0].(map[string]any)
			if firstSrc["type"] != "url" || firstSrc["url"] == nil {
				t.Fatalf("web action.sources[0] want type=url: %#v", firstSrc)
			}
			if firstSrc["title"] == nil || firstSrc["title"] == "" {
				t.Fatalf("web action.sources[0] should keep title extension: %#v", firstSrc)
			}
		} else {
			t.Fatalf("web action.sources missing: %#v", webAction["sources"])
		}
	} else if webSources[0]["type"] != "url" || webSources[0]["url"] == nil || webSources[0]["title"] == nil {
		t.Fatalf("web action.sources[0] = %#v", webSources[0])
	}
	outMsg := out[len(out)-1].(map[string]any)
	part := outMsg["content"].([]any)[0].(map[string]any)
	flat := part["annotations"].([]any)[0].(map[string]any)
	if flat["type"] != "url_citation" || flat["url"] == nil || flat["url_citation"] != nil || flat["title"] != "1" {
		t.Fatalf("responses annotation must be flat url_citation with numeric label, got %#v", flat)
	}
}

func TestCitationTitleFallsBackToIndex(t *testing.T) {
	parsed := &parsedChat{}
	// render_citation without prior tool_result titles → title should be "1"
	frames := []string{
		`{"event":{"type":"response.chunk","chunk":{"text":{"text":"hi","channel":"CHANNEL_ASSISTANT_RESPONSE"}}}}`,
		`{"event":{"type":"response.chunk","chunk":{"render_citation":{"url":"https://example.com/no-title","kind":"CITATION_KIND_WEB_PAGE"}}}}`,
	}
	for _, frame := range frames {
		if _, _, err := parseUpstreamFrame([]byte(frame), parsed); err != nil {
			t.Fatal(err)
		}
	}
	if len(parsed.Annotations) != 1 {
		t.Fatalf("annotations = %#v", parsed.Annotations)
	}
	if parsed.Annotations[0]["title"] != "1" {
		t.Fatalf("want title fallback to index, got %#v", parsed.Annotations[0])
	}
	if !strings.Contains(parsed.Text.String(), "[[1]](https://example.com/no-title)") {
		t.Fatalf("text = %q", parsed.Text.String())
	}
}

func TestGatewayCitationReusesNumberAfterInterveningText(t *testing.T) {
	parsed := &parsedChat{}
	frames := []string{
		`{"event":{"type":"response.chunk","chunk":{"text":{"text":"甲","channel":"CHANNEL_ASSISTANT_RESPONSE"}}}}`,
		`{"event":{"type":"response.chunk","chunk":{"render_citation":{"url":"https://example.com/a"}}}}`,
		// A duplicate frame with no intervening text is transport noise and is collapsed.
		`{"event":{"type":"response.chunk","chunk":{"render_citation":{"url":"https://example.com/a"}}}}`,
		`{"event":{"type":"response.chunk","chunk":{"text":{"text":"，乙","channel":"CHANNEL_ASSISTANT_RESPONSE"}}}}`,
		// A later citation of the same source remains visible and reuses [[1]].
		`{"event":{"type":"response.chunk","chunk":{"render_citation":{"url":"https://example.com/a"}}}}`,
	}
	for _, frame := range frames {
		if _, _, err := parseUpstreamFrame([]byte(frame), parsed); err != nil {
			t.Fatal(err)
		}
	}
	want := "甲[[1]](https://example.com/a)，乙[[1]](https://example.com/a)"
	if got := parsed.Text.String(); got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
	if len(parsed.Annotations) != 2 {
		t.Fatalf("annotations = %#v", parsed.Annotations)
	}
	first, second := parsed.Annotations[0], parsed.Annotations[1]
	if first["start_index"] != 1 || first["end_index"] != 29 {
		t.Fatalf("first annotation uses character offsets: %#v", first)
	}
	if second["start_index"] != 31 || second["end_index"] != 59 {
		t.Fatalf("second annotation uses character offsets: %#v", second)
	}
}

func TestGatewayCitationStateIsBounded(t *testing.T) {
	parsed := &parsedChat{citationIndex: make(map[string]int, maxTrackedCitationSources)}
	for i := 0; i < maxTrackedCitationSources; i++ {
		parsed.citationIndex[fmt.Sprintf("https://example.com/%d", i)] = i + 1
	}
	if kind, delta, err := applyGatewayRenderCitation(parsed, map[string]any{"url": "https://example.com/overflow"}); err != nil || kind != "" || delta != "" {
		t.Fatalf("overflow citation kind=%q delta=%q err=%v", kind, delta, err)
	}
	if len(parsed.citationIndex) != maxTrackedCitationSources {
		t.Fatalf("citation sources grew to %d", len(parsed.citationIndex))
	}

	parsed = &parsedChat{
		citationIndex: map[string]int{"https://example.com/a": 1},
		Annotations:   make([]map[string]any, maxTrackedAnnotations),
	}
	kind, delta, err := applyGatewayRenderCitation(parsed, map[string]any{"url": "https://example.com/a"})
	if err != nil || kind != "text" || delta != "[[1]](https://example.com/a)" {
		t.Fatalf("existing citation kind=%q delta=%q err=%v", kind, delta, err)
	}
	if len(parsed.Annotations) != maxTrackedAnnotations {
		t.Fatalf("annotations grew to %d", len(parsed.Annotations))
	}
}

func TestGatewayToolResultWithoutIDCompletesSingleMatchingCall(t *testing.T) {
	parsed := &parsedChat{}
	collectGatewayToolUsageCard(parsed, map[string]any{
		"tool_usage_card_id": "tool-1",
		"web_search":         map[string]any{"args": map[string]any{"query": "query"}},
	})
	collectGatewayToolResult(parsed, map[string]any{
		"web_search": map[string]any{"webpages": []any{map[string]any{"url": "https://example.com"}}},
	})
	if len(parsed.HostedSearchCalls) != 1 {
		t.Fatalf("hosted calls = %#v", parsed.HostedSearchCalls)
	}
	call := parsed.HostedSearchCalls[0]
	if call.ID != "tool-1" || call.Status != "completed" || len(call.Sources) != 1 {
		t.Fatalf("call = %#v", call)
	}
	if parsed.ServerTools != 1 || parsed.WebSearchTools != 1 {
		t.Fatalf("tool counters server=%d web=%d", parsed.ServerTools, parsed.WebSearchTools)
	}
}

func TestGatewayResultOnlyStillRecordsSuccessfulTool(t *testing.T) {
	parsed := &parsedChat{}
	collectGatewayToolResult(parsed, map[string]any{
		"tool_call_id": "result-only",
		"x_post": map[string]any{"posts": []any{map[string]any{
			"userhandle": "xai", "post_id": "1", "text": "news",
		}}},
	})
	if parsed.ServerTools != 1 || parsed.XSearchTools != 1 {
		t.Fatalf("tool counters server=%d x=%d", parsed.ServerTools, parsed.XSearchTools)
	}
	usage := xaiServerSideToolUsage(*parsed)
	if usage["SERVER_SIDE_TOOL_X_SEARCH"] != int64(1) {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestXAIToolUsageCountsCompletedGatewayCallsOnly(t *testing.T) {
	parsed := parsedChat{
		WebSearchTools: 2,
		XSearchTools:   1,
		HostedSearchCalls: []hostedSearchCall{
			{ID: "pending-web", Kind: "web_search", Status: "in_progress"},
			{ID: "done-x", Kind: "x_search", Status: "completed"},
		},
	}
	usage := xaiServerSideToolUsage(parsed)
	if usage["SERVER_SIDE_TOOL_WEB_SEARCH"] != nil || usage["SERVER_SIDE_TOOL_X_SEARCH"] != int64(1) {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestXAICitationsUnionSearchResultsAndRenderedSources(t *testing.T) {
	parsed := parsedChat{
		SearchSources: []map[string]any{{"url": "https://example.com/from-result"}},
		Annotations: []map[string]any{
			{"url": "https://example.com/from-result"},
			{"url": "https://example.com/render-only"},
		},
	}
	want := []string{"https://example.com/from-result", "https://example.com/render-only"}
	got := xaiCitationURLs(parsed)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("citations = %#v, want %#v", got, want)
	}
}

func TestNoInlineCitationsOmitsMarkdownMarkers(t *testing.T) {
	parsed := &parsedChat{DisableInlineCitations: true}
	frames := []string{
		`{"event":{"type":"response.chunk","chunk":{"tool_result":{"web_search":{"webpages":[{"url":"https://example.com/a","title":"A"}]}}}}}`,
		`{"event":{"type":"response.chunk","chunk":{"text":{"text":"hello","channel":"CHANNEL_ASSISTANT_RESPONSE"}}}}`,
		`{"event":{"type":"response.chunk","chunk":{"render_citation":{"url":"https://example.com/a","kind":"CITATION_KIND_WEB_PAGE"}}}}`,
	}
	for _, frame := range frames {
		if _, _, err := parseUpstreamFrame([]byte(frame), parsed); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Contains(parsed.Text.String(), "[[") {
		t.Fatalf("inline markers should be absent: %q", parsed.Text.String())
	}
	if len(parsed.Annotations) != 1 {
		t.Fatalf("annotations = %#v", parsed.Annotations)
	}
	if _, ok := parsed.Annotations[0]["start_index"]; ok {
		t.Fatalf("no_inline annotations must omit positions: %#v", parsed.Annotations[0])
	}
	payload := buildOpenAIResult(conversation.OperationResponses, "resp_1", "m", *parsed, false)
	if payload["citations"] == nil {
		t.Fatalf("citations still required: %#v", payload)
	}
}

func TestInlineCitationsIncludeSwitch(t *testing.T) {
	opts := conversation.ResponseOptions{}
	if !opts.InlineCitationsEnabled() {
		t.Fatal("default should enable inline citations")
	}
	opts.Include = []string{"no_inline_citations"}
	if opts.InlineCitationsEnabled() {
		t.Fatal("no_inline_citations should disable")
	}
	opts.Include = []string{"no_inline_citations", "inline_citations"}
	if !opts.InlineCitationsEnabled() {
		t.Fatal("later inline_citations should re-enable")
	}
	off := false
	opts.InlineCitations = &off
	if opts.InlineCitationsEnabled() {
		t.Fatal("explicit false should win")
	}
}

func TestWriteStreamAnnotationsShapes(t *testing.T) {
	ann := []map[string]any{{
		"type": "url_citation", "url": "https://example.com", "title": "1",
		"start_index": 1, "end_index": 10,
	}}
	var chatBuf strings.Builder
	if err := writeStreamAnnotations(&chatBuf, "chat", "resp_1", "m", ann, 0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(chatBuf.String(), `"url_citation"`) || !strings.Contains(chatBuf.String(), `"annotations"`) {
		t.Fatalf("chat stream = %s", chatBuf.String())
	}
	var respBuf strings.Builder
	responsesStream := newWebResponsesStream(&respBuf, "resp_1")
	if err := responsesStream.Annotations(ann, 3); err != nil {
		t.Fatal(err)
	}
	out := respBuf.String()
	if !strings.Contains(out, "response.output_text.annotation.added") || !strings.Contains(out, `"annotation_index":3`) {
		t.Fatalf("responses stream = %s", out)
	}
	if strings.Contains(out, `"url_citation":{`) {
		t.Fatalf("responses annotation must stay flat: %s", out)
	}
	if !strings.Contains(out, `"item_id":"msg_`) || strings.Contains(out, `"item_id":""`) {
		t.Fatalf("responses annotation needs a stable message item id: %s", out)
	}
}

func TestResponsesStreamUsesOneStableOutputSequence(t *testing.T) {
	var buf strings.Builder
	stream := newWebResponsesStream(&buf, "resp_1")
	if err := stream.Delta("reasoning", "thinking"); err != nil {
		t.Fatal(err)
	}
	if err := stream.HostedSearch(hostedSearchCall{ID: "search_1", Kind: "web_search", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	if err := stream.Delta("text", "hello"); err != nil {
		t.Fatal(err)
	}
	call := parsedToolCall{ID: "call_1", Name: "lookup", Arguments: `{}`}
	if err := stream.ToolCalls([]parsedToolCall{call}); err != nil {
		t.Fatal(err)
	}
	parsed := parsedChat{ToolCalls: []parsedToolCall{call}}
	parsed.Reasoning.WriteString("thinking")
	parsed.Text.WriteString("hello")
	if err := stream.Finish(&parsed); err != nil {
		t.Fatal(err)
	}
	wantTypes := []string{"reasoning", "web_search_call", "message", "function_call"}
	if len(parsed.ResponseOutput) != len(wantTypes) {
		t.Fatalf("output = %#v", parsed.ResponseOutput)
	}
	seenIDs := make(map[string]struct{}, len(wantTypes))
	for index, wantType := range wantTypes {
		item := parsed.ResponseOutput[index].(map[string]any)
		if item["type"] != wantType || item["status"] != "completed" {
			t.Fatalf("output[%d] = %#v", index, item)
		}
		id, _ := item["id"].(string)
		if id == "" {
			t.Fatalf("output[%d] missing id: %#v", index, item)
		}
		if _, duplicate := seenIDs[id]; duplicate {
			t.Fatalf("duplicate output id %q", id)
		}
		seenIDs[id] = struct{}{}
	}
	payload := buildOpenAIResult(conversation.OperationResponses, "resp_1", "m", parsed, false)
	payloadOutput := payload["output"].([]any)
	for index := range parsed.ResponseOutput {
		wantID := parsed.ResponseOutput[index].(map[string]any)["id"]
		if gotID := payloadOutput[index].(map[string]any)["id"]; gotID != wantID {
			t.Fatalf("completed output[%d] id=%v, want %v", index, gotID, wantID)
		}
	}
	out := buf.String()
	addedIDs := make(map[int]string, len(wantTypes))
	doneIDs := make(map[int]string, len(wantTypes))
	for _, event := range decodeSSEPayloads(t, out) {
		typeName, _ := event["type"].(string)
		if typeName != "response.output_item.added" && typeName != "response.output_item.done" {
			continue
		}
		index, ok := numberAsInt(event["output_index"])
		if !ok {
			t.Fatalf("event missing output_index: %#v", event)
		}
		item := event["item"].(map[string]any)
		id, _ := item["id"].(string)
		if typeName == "response.output_item.added" {
			if item["status"] != "in_progress" {
				t.Fatalf("added item[%d] = %#v", index, item)
			}
			addedIDs[index] = id
		} else {
			if item["status"] != "completed" {
				t.Fatalf("done item[%d] = %#v", index, item)
			}
			doneIDs[index] = id
		}
	}
	for index := range wantTypes {
		if addedIDs[index] == "" || doneIDs[index] != addedIDs[index] {
			t.Fatalf("item[%d] lifecycle added=%q done=%q\n%s", index, addedIDs[index], doneIDs[index], out)
		}
	}
}

func TestResponsesStreamLateReasoningDoesNotRenumberEarlierItems(t *testing.T) {
	var buf strings.Builder
	stream := newWebResponsesStream(&buf, "resp_1")
	if err := stream.HostedSearch(hostedSearchCall{ID: "search_1", Kind: "web_search", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	if err := stream.Delta("text", "answer"); err != nil {
		t.Fatal(err)
	}
	parsed := parsedChat{}
	parsed.Text.WriteString("answer")
	parsed.Reasoning.WriteString("late reasoning")
	if err := stream.Finish(&parsed); err != nil {
		t.Fatal(err)
	}
	wantTypes := []string{"web_search_call", "message", "reasoning"}
	for index, wantType := range wantTypes {
		item := parsed.ResponseOutput[index].(map[string]any)
		if item["type"] != wantType {
			t.Fatalf("output[%d] = %#v", index, item)
		}
	}
	if !strings.Contains(buf.String(), `"output_index":0`) || !strings.Contains(buf.String(), `"output_index":1`) || !strings.Contains(buf.String(), `"output_index":2`) {
		t.Fatalf("stream indices = %s", buf.String())
	}
}

func TestWriteStreamDoneChatIncludesServerSideToolUsage(t *testing.T) {
	parsed := parsedChat{
		HostedSearchCalls: []hostedSearchCall{
			{ID: "w1", Kind: "web_search", Status: "completed"},
			{ID: "x1", Kind: "x_search", Status: "completed"},
		},
		Annotations: []map[string]any{
			{"type": "url_citation", "url": "https://example.com", "title": "Example", "start_index": 0, "end_index": 5},
		},
	}
	payload := buildOpenAIResult("chat", "resp_test", "m", parsed, false)
	if payload["server_side_tool_usage"] == nil {
		t.Fatalf("payload missing server_side_tool_usage: %#v", payload)
	}
	var buf strings.Builder
	writeStreamDone(&buf, "chat", "resp_test", "m", parsed, payload)
	out := buf.String()
	if !strings.Contains(out, `"server_side_tool_usage"`) {
		t.Fatalf("final chat chunk missing server_side_tool_usage: %s", out)
	}
	if !strings.Contains(out, `"SERVER_SIDE_TOOL_WEB_SEARCH"`) || !strings.Contains(out, `"SERVER_SIDE_TOOL_X_SEARCH"`) {
		t.Fatalf("tool usage keys missing: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("expected finish chunk: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("missing DONE: %s", out)
	}
	if strings.Contains(out, `"annotations"`) {
		t.Fatalf("final chunk must not repeat progressive annotations: %s", out)
	}
}

func decodeSSEPayloads(t *testing.T, stream string) []map[string]any {
	t.Helper()
	var payloads []map[string]any
	for _, block := range strings.Split(stream, "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
				continue
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
				t.Fatalf("decode SSE payload %q: %v", line, err)
			}
			payloads = append(payloads, payload)
		}
	}
	return payloads
}
