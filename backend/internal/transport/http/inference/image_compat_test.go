package inference

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestParseConversationImageInputUsesLatestUserTurn(t *testing.T) {
	for _, test := range []struct {
		name     string
		protocol streamProtocol
		body     string
		prompt   string
		image    string
	}{
		{"chat", streamProtocolChat, `{"model":"grok-imagine-image","messages":[{"role":"user","content":"old prompt"},{"role":"assistant","content":"![old](https://example.com/old.png)"},{"role":"user","content":[{"type":"text","text":"改成蓝色"},{"type":"image_url","image_url":{"url":"https://example.com/new.png"}}]}]}`, "改成蓝色", "https://example.com/new.png"},
		{"responses", streamProtocolResponses, `{"model":"grok-imagine-image","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"画成油画"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`, "画成油画", "data:image/png;base64,AAAA"},
		{"messages", streamProtocolAnthropic, `{"model":"grok-imagine-image","max_tokens":128,"messages":[{"role":"user","content":[{"type":"text","text":"改成素描"},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"AAAA"}}]}]}`, "改成素描", "data:image/jpeg;base64,AAAA"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input, err := parseConversationImageInput([]byte(test.body), test.protocol)
			if err != nil || input.prompt != test.prompt || len(input.images) != 1 || input.images[0] != test.image {
				t.Fatalf("input=%#v err=%v", input, err)
			}
		})
	}
}

func TestImageConversationOutputUsesRequestedProtocol(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		protocol streamProtocol
		stream   bool
		want     string
	}{
		{streamProtocolChat, false, `"object":"chat.completion"`},
		{streamProtocolChat, true, `"object":"chat.completion.chunk"`},
		{streamProtocolResponses, false, `"object":"response"`},
		{streamProtocolResponses, true, `event: response.completed`},
		{streamProtocolAnthropic, false, `"type":"message"`},
		{streamProtocolAnthropic, true, `event: message_stop`},
	} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		writeImageConversation(ctx, test.protocol, "grok-imagine-image", "![生成图片](https://example.com/result.png)", test.stream)
		if !strings.Contains(recorder.Body.String(), test.want) || !strings.Contains(recorder.Body.String(), "result.png") {
			t.Fatalf("protocol=%d stream=%v body=%s", test.protocol, test.stream, recorder.Body.String())
		}
	}
}
