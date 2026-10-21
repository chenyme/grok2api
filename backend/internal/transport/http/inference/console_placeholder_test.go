package inference

import (
	"io"
	"strings"
	"testing"
)

func TestConsolePlaceholderJSONPreservesRealReasoning(t *testing.T) {
	without := addConsolePlaceholderJSON([]byte(`{"choices":[{"message":{"role":"assistant","content":"答案"}}]}`), streamProtocolChat)
	if !strings.Contains(string(without), `"reasoning_content":"深度思考中"`) {
		t.Fatalf("placeholder missing: %s", without)
	}
	withReasoning := addConsolePlaceholderJSON([]byte(`{"choices":[{"message":{"role":"assistant","reasoning_content":"真实推理","content":"答案"}}]}`), streamProtocolChat)
	if strings.Contains(string(withReasoning), consoleThinkingPlaceholder) {
		t.Fatalf("real reasoning replaced: %s", withReasoning)
	}
	emptyReasoning := addConsolePlaceholderJSON([]byte(`{"output":[{"id":"rs_1","type":"reasoning","summary":[]},{"type":"message"}]}`), streamProtocolResponses)
	if strings.Count(string(emptyReasoning), `"type":"reasoning"`) != 1 || !strings.Contains(string(emptyReasoning), consoleThinkingPlaceholder) {
		t.Fatalf("empty reasoning item was not filled in place: %s", emptyReasoning)
	}
}

func TestConsolePlaceholderStreamKeepsProtocolIndexes(t *testing.T) {
	for _, test := range []struct {
		name     string
		protocol streamProtocol
		stream   string
		want     string
	}{
		{"chat", streamProtocolChat, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"答案\"}}]}\n\n", `"reasoning_content":"深度思考中"`},
		{"responses", streamProtocolResponses, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\"}}\n\n", `"output_index":1`},
		{"responses empty reasoning", streamProtocolResponses, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"summary\":[]}}\n\nevent: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"summary\":[]}}\n\n", `"id":"rs_1"`},
		{"messages", streamProtocolAnthropic, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n", `"index":1`},
	} {
		t.Run(test.name, func(t *testing.T) {
			decorated := newConsolePlaceholderStream(strings.NewReader(test.stream), test.protocol)
			defer decorated.Close()
			data, err := io.ReadAll(decorated)
			if err != nil || !strings.Contains(string(data), consoleThinkingPlaceholder) || !strings.Contains(string(data), test.want) {
				t.Fatalf("output=%s err=%v", data, err)
			}
		})
	}
}

func TestConsolePlaceholderStreamLeavesVisibleReasoningAlone(t *testing.T) {
	source := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"真实推理\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"答案\"}}]}\n\n"
	decorated := newConsolePlaceholderStream(strings.NewReader(source), streamProtocolChat)
	defer decorated.Close()
	data, err := io.ReadAll(decorated)
	if err != nil || strings.Contains(string(data), consoleThinkingPlaceholder) || !strings.Contains(string(data), "真实推理") {
		t.Fatalf("output=%s err=%v", data, err)
	}
}
