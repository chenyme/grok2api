package web

import (
	"encoding/json"
	"errors"
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
	if parsed.HostedSearchCalls[1].Kind != "x_search" || parsed.HostedSearchCalls[1].Query == "" && parsed.HostedSearchCalls[1].Status != "completed" {
		// x call should be completed with sources
		if parsed.HostedSearchCalls[1].Kind != "x_search" || parsed.HostedSearchCalls[1].Status != "completed" {
			t.Fatalf("x call = %#v", parsed.HostedSearchCalls[1])
		}
	}
	if len(parsed.Annotations) != 2 {
		t.Fatalf("annotations = %#v", parsed.Annotations)
	}
	text := parsed.Text.String()
	if !strings.Contains(text, "预计发布") || !strings.Contains(text, "[[1]](https://x.com/elonmusk/status/2082707547203518569)") || !strings.Contains(text, "[[2]](https://www.ithome.com/0/981/947.htm)") {
		t.Fatalf("text = %q emitted = %q", text, emitted.String())
	}
	first := parsed.Annotations[0]
	// Annotation title = page/source title; [[N]] still carries the number in text.
	wantTitle := "Elon Musk: And Grok 4.6 comes out in a week"
	if first["type"] != "url_citation" || first["url"] != "https://x.com/elonmusk/status/2082707547203518569" || first["title"] != wantTitle {
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
	if flat["type"] != "url_citation" || flat["url"] == nil || flat["url_citation"] != nil || flat["title"] != wantTitle {
		t.Fatalf("responses annotation must be flat url_citation with page title, got %#v", flat)
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
	if err := writeStreamAnnotations(&respBuf, conversation.OperationResponses, "resp_1", "m", ann, 3); err != nil {
		t.Fatal(err)
	}
	out := respBuf.String()
	if !strings.Contains(out, "response.output_text.annotation.added") || !strings.Contains(out, `"annotation_index":3`) {
		t.Fatalf("responses stream = %s", out)
	}
	if strings.Contains(out, `"url_citation":{`) {
		t.Fatalf("responses annotation must stay flat: %s", out)
	}
}
