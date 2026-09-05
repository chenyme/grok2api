package conversation

import (
	"encoding/json"
	"testing"
)

func TestChatMultiTurnReasoningRestoration(t *testing.T) {
	// 模拟第一轮：上游返回 Responses 包含 reasoning 和 function_call
	callID := "call_integration_test_123"
	env := responseEnvelope{
		Output: []responseItem{
			{
				ID:        "rs_integ_001",
				Type:      "reasoning",
				Status:    "completed",
				Encrypted: "encrypted_secret_chain_proof",
			},
			{
				ID:        "fc_integ_001",
				Type:      "function_call",
				CallID:    callID,
				Name:      "list_dir",
				Arguments: `{"path":"/"}`,
			},
		},
	}

	// 触发出站记录
	RememberReasoningForEnvelope(env)

	// 模拟第二轮：下游客户端传回标准的 Chat Completions 历史（带有刚才的 tool_calls 和 tool output）
	chatReqJSON := []byte(`{
		"model": "grok-4.6",
		"messages": [
			{"role": "user", "content": "hello investigate"},
			{
				"role": "assistant",
				"content": null,
				"tool_calls": [
					{
						"id": "` + callID + `",
						"type": "function",
						"function": {
							"name": "list_dir",
							"arguments": "{\"path\":\"/\"}"
						}
					}
				]
			},
			{
				"role": "tool",
				"tool_call_id": "` + callID + `",
				"content": "{\"error\":\"not found\"}"
			}
		]
	}`)

	convertedBody, _, err := ConvertRequestWithOptions(chatReqJSON, "grok-4.6", OperationChat)
	if err != nil {
		t.Fatalf("ConvertRequestWithOptions failed: %v", err)
	}

	var convertedMap map[string]any
	if err := json.Unmarshal(convertedBody, &convertedMap); err != nil {
		t.Fatalf("Unmarshal convertedBody failed: %v", err)
	}

	inputs, ok := convertedMap["input"].([]any)
	if !ok || len(inputs) == 0 {
		t.Fatalf("expected non-empty input array, got: %v", convertedMap["input"])
	}

	// 验证 input 列表中必须包含我们注入的 reasoning 项！
	foundReasoning := false
	for _, itemRaw := range inputs {
		item, ok := itemRaw.(map[string]any)
		if !ok {
			continue
		}
		if item["type"] == "reasoning" {
			if item["id"] == "rs_integ_001" && item["encrypted_content"] == "encrypted_secret_chain_proof" {
				foundReasoning = true
				break
			}
		}
	}

	if !foundReasoning {
		t.Fatalf("FAIL: expected reasoning block with id rs_integ_001 to be restored in input, but not found. Inputs: %#v", inputs)
	}
}
