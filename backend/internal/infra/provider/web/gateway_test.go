package web

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
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
	if len(parsed.Annotations) != 2 {
		t.Fatalf("annotations = %#v", parsed.Annotations)
	}
	text := parsed.Text.String()
	if !strings.Contains(text, "预计发布") || !strings.Contains(text, "[[1]](https://x.com/elonmusk/status/2082707547203518569)") || !strings.Contains(text, "[[2]](https://www.ithome.com/0/981/947.htm)") {
		t.Fatalf("text = %q emitted = %q", text, emitted.String())
	}
	first := parsed.Annotations[0]
	if first["type"] != "url_citation" || first["url"] != "https://x.com/elonmusk/status/2082707547203518569" {
		t.Fatalf("first annotation = %#v", first)
	}
	chatPayload := buildOpenAIResult("chat", "resp_1", "grok-chat-fast", *parsed, false)
	msg := chatPayload["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["annotations"] == nil {
		t.Fatalf("chat annotations missing: %#v", msg)
	}
	if chatPayload["search_sources"] == nil {
		t.Fatalf("search_sources missing: %#v", chatPayload)
	}
}
