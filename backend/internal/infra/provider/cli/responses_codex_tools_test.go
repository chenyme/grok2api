package cli

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestLegacyLocalShellUsesNativeLocalShellAndRestoresJSON(t *testing.T) {
	normalized, compatibility, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":"show cwd",
		"tools":[{"type":"local_shell"}],
		"tool_choice":{"type":"local_shell"}
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if compatibility == nil || !compatibility.legacyLocalShell {
		t.Fatal("legacy local_shell 未启用兼容层")
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	tool := request["tools"].([]any)[0].(map[string]any)
	environment := tool["environment"].(map[string]any)
	if tool["type"] != "shell" || environment["type"] != "local" || request["tool_choice"] != "required" {
		t.Fatalf("upstream shell = %#v, choice = %#v", tool, request["tool_choice"])
	}

	restored, err := compatibility.normalizeResponseJSON([]byte(`{
		"id":"resp_1","object":"response",
		"tools":[{"type":"shell","environment":{"type":"local"}}],
		"output":[{"id":"sh_1","type":"shell_call","call_id":"call_1","status":"completed","action":{"type":"exec","commands":["pwd"]}}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(restored, &response); err != nil {
		t.Fatal(err)
	}
	call := response["output"].([]any)[0].(map[string]any)
	action := call["action"].(map[string]any)
	if call["type"] != "local_shell_call" || action["type"] != "exec" || action["command"] != "pwd" {
		t.Fatalf("legacy local_shell_call = %#v", call)
	}
	if response["tools"].([]any)[0].(map[string]any)["type"] != "local_shell" {
		t.Fatalf("visible tools = %#v", response["tools"])
	}
}

func TestLegacyLocalShellHistoryBecomesStructuredShellHistory(t *testing.T) {
	normalized, _, err := normalizeResponsesRequest([]byte(`{
		"model":"public","tools":[{"type":"local_shell"}],"input":[
			{"type":"local_shell_call","id":"sh_1","call_id":"call_1","status":"completed","action":{"type":"exec","command":["printf","a b"],"working_directory":"/workspace","env":{"MODE":"test"}}},
			{"type":"local_shell_call_output","call_id":"call_1","status":"failed","exit_code":7,"output":"failure"}
		]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	items := request["input"].([]any)
	call := items[0].(map[string]any)
	commands := call["action"].(map[string]any)["commands"].([]any)
	if call["type"] != "shell_call" || len(commands) != 1 || commands[0] != `cd /workspace && env MODE=test printf 'a b'` {
		t.Fatalf("shell call history = %#v", call)
	}
	output := items[1].(map[string]any)
	outcome := output["output"].([]any)[0].(map[string]any)["outcome"].(map[string]any)
	if output["type"] != "shell_call_output" || outcome["exit_code"] != float64(7) {
		t.Fatalf("shell output history = %#v", output)
	}
}

func TestNativeShellOutputHistoryIsSanitizedForBuild(t *testing.T) {
	normalized, _, err := normalizeResponsesRequest([]byte(`{
		"model":"public","tools":[{"type":"shell","environment":{"type":"local"}}],"input":[
			{"type":"shell_call_output","call_id":"call_1","status":"completed","output":[
				{"command":"pwd","stdout":"/workspace\n","stderr":"","outcome":{"type":"exit","exitCode":0}}
			],"max_output_length":2048}
		]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	item := request["input"].([]any)[0].(map[string]any)
	block := item["output"].([]any)[0].(map[string]any)
	outcome := block["outcome"].(map[string]any)
	if item["type"] != "shell_call_output" || block["command"] != nil || outcome["exit_code"] != float64(0) || outcome["exitCode"] != nil {
		t.Fatalf("shell output history = %#v", item)
	}
}

func TestFunctionCallOutputHistoryEncodesStructuredOutput(t *testing.T) {
	normalized, _, err := normalizeResponsesRequest([]byte(`{
		"model":"public","tools":[{"type":"function","name":"shell_command","parameters":{"type":"object"}}],"input":[
			{"type":"function_call_output","call_id":"call_1","status":"completed","output":{"exit_code":0,"stdout":"ok","stderr":""}}
		]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	item := request["input"].([]any)[0].(map[string]any)
	output, ok := item["output"].(string)
	if item["type"] != "function_call_output" || !ok || !strings.Contains(output, `"stdout":"ok"`) {
		t.Fatalf("function output history = %#v", item)
	}
}

func TestFunctionCallOutputHistoryPreservesContentBlocks(t *testing.T) {
	normalized, compatibility, err := normalizeResponsesRequest([]byte(`{
		"model":"public","tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}}],"input":[
			{"type":"function_call_output","call_id":"call_1","status":"completed","output":[
				{"type":"input_text","text":"Read image file: screenshot.png"},
				{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="},
				{"type":"input_image","detail":"original","file_id":"file_image_2"},
				{"type":"input_file","file_id":"file_document_1","filename":"notes.txt"}
			]}
		]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	item := request["input"].([]any)[0].(map[string]any)
	output, ok := item["output"].([]any)
	if !ok || len(output) != 4 {
		t.Fatalf("function output history = %#v", item)
	}
	text := output[0].(map[string]any)
	firstImage := output[1].(map[string]any)
	secondImage := output[2].(map[string]any)
	file := output[3].(map[string]any)
	if text["type"] != "input_text" || text["text"] != "Read image file: screenshot.png" ||
		firstImage["type"] != "input_image" || firstImage["detail"] != "auto" ||
		firstImage["image_url"] != "data:image/png;base64,aGVsbG8=" ||
		secondImage["type"] != "input_image" || secondImage["detail"] != "high" || secondImage["file_id"] != "file_image_2" ||
		file["type"] != "input_file" || file["file_id"] != "file_document_1" || file["filename"] != "notes.txt" ||
		item["status"] != nil {
		t.Fatalf("function output history = %#v", item)
	}
	if compatibility == nil || !strings.Contains(compatibility.warningHeader(), "image_detail_original_downgraded") {
		t.Fatalf("compatibility warnings = %#v", compatibility)
	}
}

func TestFunctionCallOutputHistoryKeepsStructuredArraysAsJSON(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{name: "empty", output: `[]`},
		{name: "scalars", output: `[1,2,true]`},
		{name: "objects", output: `[{"id":1,"type":"record"}]`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			normalized, _, err := normalizeResponsesRequest([]byte(`{
				"model":"public","input":[{"type":"function_call_output","call_id":"call_1","output":`+test.output+`}]
			}`), "grok-4.5")
			if err != nil {
				t.Fatal(err)
			}
			var request map[string]any
			if err := json.Unmarshal(normalized, &request); err != nil {
				t.Fatal(err)
			}
			output, ok := request["input"].([]any)[0].(map[string]any)["output"].(string)
			if !ok || output != test.output {
				t.Fatalf("output = %#v, want JSON string %q", output, test.output)
			}
		})
	}
}

func TestFunctionCallOutputHistoryRejectsInvalidContentBlocks(t *testing.T) {
	tests := []struct {
		name      string
		output    string
		wantParam string
		wantCode  string
	}{
		{name: "mixed_non_object", output: `[{"type":"input_text","text":"ok"},42]`, wantParam: "input[0].output[1]", wantCode: "invalid_parameter"},
		{name: "mixed_missing_type", output: `[{"type":"input_text","text":"ok"},{"text":"missing type"}]`, wantParam: "input[0].output[1].type", wantCode: "invalid_parameter"},
		{name: "missing_text", output: `[{"type":"input_text"}]`, wantParam: "input[0].output[0].text", wantCode: "invalid_parameter"},
		{name: "missing_image_source", output: `[{"type":"input_image","detail":"auto"}]`, wantParam: "input[0].output[0].image_url", wantCode: "invalid_parameter"},
		{name: "invalid_image_detail", output: `[{"type":"input_image","detail":"medium","image_url":"data:image/png;base64,AA=="}]`, wantParam: "input[0].output[0].detail", wantCode: "invalid_parameter"},
		{name: "missing_file_source", output: `[{"type":"input_file","filename":"empty.txt"}]`, wantParam: "input[0].output[0].file_data", wantCode: "invalid_parameter"},
		{name: "unknown_type", output: `[{"type":"input_audio","data":"AA=="}]`, wantParam: "input[0].output[0].type", wantCode: "unsupported_parameter"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`{"model":"public","input":[{"type":"function_call_output","call_id":"call_1","output":` + test.output + `}]}`)
			_, _, err := normalizeResponsesRequest(body, "grok-4.5")
			requestErr, ok := err.(*responsesRequestError)
			if !ok || requestErr.Param != test.wantParam || requestErr.Code != test.wantCode {
				t.Fatalf("error = %#v", err)
			}
		})
	}
}

func TestMessageImageDetailUsesSameCompatibilityPolicy(t *testing.T) {
	normalized, compatibility, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":[{"type":"message","role":"user","content":[
			{"type":"input_image","image_url":"data:image/png;base64,AA=="},
			{"type":"input_image","detail":"original","file_id":"file_1"}
		]}]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	content := request["input"].([]any)[0].(map[string]any)["content"].([]any)
	if content[0].(map[string]any)["detail"] != "auto" || content[1].(map[string]any)["detail"] != "high" {
		t.Fatalf("message content = %#v", content)
	}
	if compatibility == nil || !strings.Contains(compatibility.warningHeader(), "image_detail_original_downgraded") {
		t.Fatalf("compatibility warnings = %#v", compatibility)
	}
}

func TestCustomToolCallOutputHistoryStillEncodesArrayAsString(t *testing.T) {
	normalized, _, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":[{"type":"custom_tool_call_output","call_id":"call_1","output":[
			{"type":"input_text","text":"custom output"},
			{"type":"input_image","detail":"auto","image_url":"data:image/png;base64,AA=="}
		]}]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	item := request["input"].([]any)[0].(map[string]any)
	output, ok := item["output"].(string)
	if item["type"] != "function_call_output" || !ok || !strings.Contains(output, `"type":"input_image"`) {
		t.Fatalf("custom tool output history = %#v", item)
	}
}

func TestFunctionCallOutputHistoryPreservesFortyTwoImagesAtThirtyTwoMiB(t *testing.T) {
	const (
		imageCount       = 42
		totalPayloadSize = 32 << 20
	)
	perImagePayloadSize := (totalPayloadSize / imageCount) / 4 * 4
	imageURL := "data:image/png;base64," + strings.Repeat("A", perImagePayloadSize)
	blocks := make([]any, 0, imageCount)
	for range imageCount {
		blocks = append(blocks, map[string]any{"type": "input_image", "detail": "auto", "image_url": imageURL})
	}
	body, err := json.Marshal(map[string]any{
		"model": "public",
		"input": []any{map[string]any{
			"type": "function_call_output", "call_id": "call_images", "output": blocks,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) < totalPayloadSize {
		t.Fatalf("fixture size = %d", len(body))
	}
	normalized, _, err := normalizeResponsesRequest(body, "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	item := request["input"].([]any)[0].(map[string]any)
	output, ok := item["output"].([]any)
	if !ok || len(output) != imageCount {
		t.Fatalf("output type = %T, length = %d", item["output"], len(output))
	}
	for _, index := range []int{0, imageCount - 1} {
		image := output[index].(map[string]any)
		if image["type"] != "input_image" || image["detail"] != "auto" || image["image_url"] != imageURL {
			t.Fatalf("output[%d] was not preserved", index)
		}
	}
}

func TestAssistantOutputMessageHistoryUsesEasyMessageText(t *testing.T) {
	normalized, _, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":[
			{"type":"message","role":"system","content":"system instruction"},
			{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[
				{"type":"output_text","text":"Hi there","annotations":[]}
			]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"继续"}]}
		]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	items := request["input"].([]any)
	system := items[0].(map[string]any)
	assistant := items[1].(map[string]any)
	user := items[2].(map[string]any)
	userContent := user["content"].([]any)[0].(map[string]any)
	if system["role"] != "system" || system["content"] != "system instruction" {
		t.Fatalf("system message history = %#v", system)
	}
	if assistant["id"] != nil || assistant["status"] != nil || assistant["content"] != "Hi there" {
		t.Fatalf("assistant message history = %#v", assistant)
	}
	if userContent["type"] != "input_text" {
		t.Fatalf("user message history = %#v", user)
	}
}

func TestRoleMissingTypeBecomesMessageAndFunctionCallIsAllowlisted(t *testing.T) {
	normalized, _, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":[
			{"role":"user","content":"hello"},
			{"type":"function_call","id":"fc_1","status":"completed","call_id":"call_1","name":"shell_command","arguments":"{}","namespace":"","internal_chat_message_metadata_passthrough":{"turn_id":"t1"}},
			{"type":"function_call_output","id":"fco_1","status":"completed","call_id":"call_1","output":"ok"}
		]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	items := request["input"].([]any)
	user := items[0].(map[string]any)
	call := items[1].(map[string]any)
	output := items[2].(map[string]any)
	if user["type"] != "message" || user["content"] != "hello" {
		t.Fatalf("user history = %#v", user)
	}
	if call["type"] != "function_call" || call["id"] != nil || call["status"] != nil || call["internal_chat_message_metadata_passthrough"] != nil {
		t.Fatalf("function_call history = %#v", call)
	}
	if output["type"] != "function_call_output" || output["id"] != nil || output["status"] != nil {
		t.Fatalf("function_call_output history = %#v", output)
	}
}

func TestNativeBuildHistoryItemsArePreservedAndSanitized(t *testing.T) {
	normalized, _, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":[
			{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"Grok Build","sources":null},"phase":"commentary"},
			{"type":"shell_call","id":null,"call_id":"call_1","status":"completed","action":{"commands":["pwd"],"timeout_ms":null,"internal_chat_message_metadata_passthrough":{"turn_id":"t1"}}},
			{"type":"shell_call_output","call_id":"call_1","output":[{"stdout":"/workspace\n","stderr":"","outcome":{"type":"exit","exit_code":0}}]},
			{"type":"code_interpreter_call","id":"ci_1","container_id":"container_1","status":"completed","code":"print(1)","outputs":null},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
		]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	items := request["input"].([]any)
	webSearch := items[0].(map[string]any)
	searchAction := webSearch["action"].(map[string]any)
	if webSearch["type"] != "web_search_call" || webSearch["phase"] != nil || searchAction["sources"] != nil {
		t.Fatalf("web search history = %#v", webSearch)
	}
	shellCall := items[1].(map[string]any)
	shellAction := shellCall["action"].(map[string]any)
	if shellCall["type"] != "shell_call" || shellCall["id"] != nil || shellAction["timeout_ms"] != nil || shellAction["internal_chat_message_metadata_passthrough"] != nil {
		t.Fatalf("shell call history = %#v", shellCall)
	}
	if items[2].(map[string]any)["type"] != "shell_call_output" || items[3].(map[string]any)["type"] != "code_interpreter_call" || items[3].(map[string]any)["outputs"] != nil {
		t.Fatalf("native tool history = %#v", items[2:4])
	}
	if items[4].(map[string]any)["role"] != "user" {
		t.Fatalf("user message changed unexpectedly: %#v", items[4])
	}
}

func TestReasoningWithoutEncryptedContentRemainsNative(t *testing.T) {
	normalized, _, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":[
			{"type":"reasoning","id":"rs_1","status":"completed","summary":[{"type":"summary_text","text":"who am I"}],"content":null,"encrypted_content":null,"internal_chat_message_metadata_passthrough":{"turn_id":"t1"}},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"嗯"}]}
		]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	items := request["input"].([]any)
	first := items[0].(map[string]any)
	if first["type"] != "reasoning" || first["status"] != nil || first["encrypted_content"] != nil || first["content"] != nil {
		t.Fatalf("reasoning history = %#v", first)
	}
	summary := first["summary"].([]any)[0].(map[string]any)
	if summary["text"] != "who am I" {
		t.Fatalf("reasoning summary = %#v", first["summary"])
	}
	if items[1].(map[string]any)["content"] != "hi" {
		t.Fatalf("assistant history = %#v", items[1])
	}
}

func TestUnsupportedResponsesHistoryItemBecomesBoundary(t *testing.T) {
	normalized, _, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":[
			{"type":"future_codex_item","id":"future_1","status":"completed"},
			{"type":"message","role":"user","content":"continue"}
		]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	items := request["input"].([]any)
	boundary := items[0].(map[string]any)
	text := boundary["content"].([]any)[0].(map[string]any)["text"].(string)
	if boundary["role"] != "developer" || !strings.Contains(text, "future_codex_item") || items[1].(map[string]any)["role"] != "user" {
		t.Fatalf("normalized history = %#v", items)
	}
}

func TestMessageImageAndFileNullFieldsAreRemoved(t *testing.T) {
	normalized, _, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":[{"type":"message","role":"user","content":[
			{"type":"input_image","image_url":"https://example.com/image.png","detail":null,"file_id":null},
			{"type":"input_file","file_id":"file_1","file_url":null,"filename":null}
		]}]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	content := request["input"].([]any)[0].(map[string]any)["content"].([]any)
	image := content[0].(map[string]any)
	file := content[1].(map[string]any)
	if image["image_url"] == nil || image["detail"] != "auto" || image["file_id"] != nil || file["file_id"] != "file_1" || file["file_url"] != nil || file["filename"] != nil {
		t.Fatalf("message content = %#v", content)
	}
}

func TestCodexPrivateMetadataIsStrippedFromHistory(t *testing.T) {
	normalized, _, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":[
			{"type":"reasoning","id":"rs_1","status":"completed","summary":[{"type":"summary_text","text":"plan"}],"encrypted_content":"cipher","internal_chat_message_metadata_passthrough":{"turn_id":"t1"}},
			{"type":"function_call","id":"fc_1","call_id":"call_1","name":"shell_command","arguments":"{}","internal_chat_message_metadata_passthrough":{"turn_id":"t1"}},
			{"type":"function_call_output","id":"fco_1","call_id":"call_1","output":"ok","internal_chat_message_metadata_passthrough":{"turn_id":"t1"}},
			{"type":"custom_tool_call","id":"ctc_1","call_id":"call_2","name":"apply_patch","input":"patch","status":"completed","internal_chat_message_metadata_passthrough":{"turn_id":"t1"}},
			{"type":"custom_tool_call_output","id":"ctco_1","call_id":"call_2","output":"done","internal_chat_message_metadata_passthrough":{"turn_id":"t1"}},
			{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"hi"}],"internal_chat_message_metadata_passthrough":{"turn_id":"t1"}}
		]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	for index, raw := range request["input"].([]any) {
		item := raw.(map[string]any)
		if item["internal_chat_message_metadata_passthrough"] != nil || item["phase"] != nil {
			t.Fatalf("input[%d] still has private fields: %#v", index, item)
		}
	}
	items := request["input"].([]any)
	if items[0].(map[string]any)["type"] != "reasoning" || items[0].(map[string]any)["encrypted_content"] != "cipher" || items[0].(map[string]any)["status"] != nil {
		t.Fatalf("reasoning history = %#v", items[0])
	}
	if items[1].(map[string]any)["type"] != "function_call" || items[3].(map[string]any)["type"] != "function_call" {
		t.Fatalf("tool call history = %#v", items)
	}
	if items[5].(map[string]any)["role"] != "assistant" {
		t.Fatalf("assistant message history = %#v", items[5])
	}
}

func TestRequestRejectsAmbiguousShellDeclarations(t *testing.T) {
	_, _, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":"hello",
		"tools":[{"type":"shell","environment":{"type":"local"}},{"type":"local_shell"}]
	}`), "grok-4.5")
	requestErr, ok := err.(*responsesRequestError)
	if !ok || requestErr.Code != "invalid_parameter" || requestErr.Param != "tools[1].type" {
		t.Fatalf("error = %#v", err)
	}
}

func TestOptionalCompatibilityControlsAreIgnored(t *testing.T) {
	normalized, compatibility, err := normalizeResponsesRequest([]byte(`{
		"model":"public",
		"tools":[{"type":"local_shell","display_width":120},{"type":"apply_patch","mode":"auto"}],
		"input":[{"type":"additional_tools","role":"user","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if compatibility == nil {
		t.Fatal("兼容字段未触发归一化")
	}
	warnings := compatibility.warningHeader()
	for _, expected := range []string{"legacy_local_shell_controls_ignored", "apply_patch_controls_ignored", "additional_tools_role_approximated"} {
		if !strings.Contains(warnings, expected) {
			t.Fatalf("warnings = %q", warnings)
		}
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	if len(request["tools"].([]any)) != 3 {
		t.Fatalf("tools = %#v", request["tools"])
	}
}

func TestApplyPatchToolRequestHistoryAndJSONResponse(t *testing.T) {
	normalized, compatibility, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":"edit file",
		"tools":[{"type":"apply_patch"}],
		"tool_choice":{"type":"apply_patch"}
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if compatibility == nil {
		t.Fatal("apply_patch 未启用兼容层")
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	tool := request["tools"].([]any)[0].(map[string]any)
	choice := request["tool_choice"].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "grok2api_apply_patch" || choice["type"] != "function" || choice["name"] != "grok2api_apply_patch" {
		t.Fatalf("apply patch wrapper = %#v, choice = %#v", tool, choice)
	}

	restored, err := compatibility.normalizeResponseJSON([]byte(`{
		"id":"resp_1","object":"response",
		"tools":[{"type":"function","name":"grok2api_apply_patch"}],
		"output":[{"id":"fc_1","type":"function_call","call_id":"call_1","status":"completed","name":"grok2api_apply_patch","arguments":"{\"operation\":{\"type\":\"update_file\",\"path\":\"main.go\",\"diff\":\"@@\\n-old\\n+new\"}}"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(restored, &response); err != nil {
		t.Fatal(err)
	}
	call := response["output"].([]any)[0].(map[string]any)
	operation := call["operation"].(map[string]any)
	if call["type"] != "apply_patch_call" || call["name"] != nil || call["arguments"] != nil || operation["type"] != "update_file" || operation["path"] != "main.go" {
		t.Fatalf("apply_patch_call = %#v", call)
	}
	if response["tools"].([]any)[0].(map[string]any)["type"] != "apply_patch" {
		t.Fatalf("visible tools = %#v", response["tools"])
	}

	history, _, err := normalizeResponsesRequest([]byte(`{
		"model":"public","tools":[{"type":"apply_patch"}],"input":[
			{"type":"apply_patch_call","id":"apc_1","call_id":"call_1","status":"completed","operation":{"type":"delete_file","path":"old.txt"}},
			{"type":"apply_patch_call_output","call_id":"call_1","status":"failed","output":"permission denied"}
		]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(history, &request); err != nil {
		t.Fatal(err)
	}
	items := request["input"].([]any)
	if items[0].(map[string]any)["type"] != "function_call" || items[0].(map[string]any)["id"] != nil || items[0].(map[string]any)["status"] != nil || !strings.Contains(items[0].(map[string]any)["arguments"].(string), `"delete_file"`) {
		t.Fatalf("apply patch call history = %#v", items[0])
	}
	if items[1].(map[string]any)["type"] != "function_call_output" || !strings.Contains(items[1].(map[string]any)["output"].(string), "failed") {
		t.Fatalf("apply patch output history = %#v", items[1])
	}
}

func TestApplyPatchStreamBuffersFunctionProtocolAndRestoresItems(t *testing.T) {
	_, compatibility, err := normalizeResponsesRequest([]byte(`{
		"model":"public","input":"edit","stream":true,"tools":[{"type":"apply_patch"}]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.Join([]string{
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"sequence_number":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","status":"in_progress","name":"grok2api_apply_patch","arguments":""}}`,
		``,
		`event: response.function_call_arguments.delta`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"sequence_number":2,"delta":"{\"operation\":{\"type\":\"delete_file\","}`,
		``,
		`event: response.function_call_arguments.done`,
		`data: {"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":0,"sequence_number":3,"arguments":"{\"operation\":{\"type\":\"delete_file\",\"path\":\"old.txt\"}}"}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":0,"sequence_number":4,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","status":"completed","name":"grok2api_apply_patch","arguments":"{\"operation\":{\"type\":\"delete_file\",\"path\":\"old.txt\"}}"}}`,
		``,
	}, "\n")
	stream := compatibility.normalizeResponseStream(io.NopCloser(strings.NewReader(source)))
	converted, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)
	if strings.Contains(text, "function_call_arguments") || strings.Contains(text, "grok2api_apply_patch") || strings.Contains(text, `"arguments"`) {
		t.Fatalf("内部 function 协议泄露:\n%s", text)
	}
	for _, expected := range []string{
		`event: response.output_item.added`, `event: response.output_item.done`,
		`"type":"apply_patch_call"`, `"type":"delete_file"`, `"path":"old.txt"`,
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("apply patch SSE 缺少 %s:\n%s", expected, text)
		}
	}
}

func TestAdditionalToolsAndRemoteCompactionTrigger(t *testing.T) {
	normalized, compatibility, err := normalizeResponsesRequest([]byte(`{
		"model":"public","tools":[{"type":"function","name":"lookup","description":"old","parameters":{"type":"object"}}],
		"input":[
			{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"lookup","description":"new","parameters":{"type":"object"}},{"type":"apply_patch"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]},
			{"type":"compaction_trigger"}
		]
	}`), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(normalized, &request); err != nil {
		t.Fatal(err)
	}
	tools := request["tools"].([]any)
	if len(tools) != 2 || tools[0].(map[string]any)["description"] != "new" || tools[1].(map[string]any)["name"] != "grok2api_apply_patch" {
		t.Fatalf("normalized tools = %#v", tools)
	}
	items := request["input"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["role"] != "developer" || items[1].(map[string]any)["role"] != "user" {
		t.Fatalf("compaction items = %#v", items)
	}
	first := items[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(first, "lookup, apply_patch") || !compatibility.compactionRequested {
		t.Fatalf("additional tools = %q, compaction = %t", first, compatibility.compactionRequested)
	}
}
