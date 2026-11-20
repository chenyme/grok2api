package web

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	fhttptest "github.com/bogdanfinn/fhttp/httptest"
	"github.com/bogdanfinn/websocket"

	"github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

func TestGatewaySemanticIdle(t *testing.T) {
	tests := []struct {
		name     string
		events   []string
		wantIdle bool
		wantData bool
		cancel   bool
	}{
		{name: "control events", events: []string{`{"event":{"type":"pong"}}`}, wantIdle: true},
		{name: "empty deltas", events: []string{`{"event":{"type":"response.output_text.delta","delta":""}}`}, wantIdle: true},
		{
			name:     "repeated search start",
			events:   []string{`{"event":{"type":"response.chunk","chunk":{"tool_usage_card":{"id":"search-1","web_search":{"query":"release"}}}}}`},
			wantIdle: true, wantData: true,
		},
		{name: "generated text", events: []string{`{"event":{"type":"response.output_text.delta","delta":"answer"}}`}},
		{name: "reasoning", events: []string{`{"event":{"type":"response.chunk","chunk":{"text":{"channel":"CHANNEL_ANALYSIS","text":"checking"}}}}`}},
		{name: "search progress", events: []string{
			`{"event":{"type":"response.chunk","chunk":{"tool_usage_card":{"id":"search-1","web_search":{"query":"release"}}}}}`,
			`{"event":{"type":"response.chunk","chunk":{"tool_result":{"web_search":{"webpages":[{"url":"https://example.com/release"}]}}}}}`,
			`{"event":{"type":"response.chunk","chunk":{"tool_usage_card":{"id":"search-2","web_search":{"query":"date"}}}}}`,
		}},
		{name: "client cancellation", events: []string{`{"event":{"type":"pong"}}`}, cancel: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := fhttptest.NewServer(fhttp.HandlerFunc(func(writer fhttp.ResponseWriter, request *fhttp.Request) {
				connection, err := (&websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(writer, request, nil)
				if err != nil {
					return
				}
				defer connection.Close()
				if _, _, err := connection.ReadMessage(); err != nil {
					return
				}
				for index := range 9 {
					event := test.events[index%len(test.events)]
					if test.name == "search progress" {
						event = test.events[min(index/3, len(test.events)-1)]
					}
					if err := connection.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
						return
					}
					time.Sleep(50 * time.Millisecond)
				}
				if err := connection.WriteJSON(map[string]any{"event": map[string]any{"type": "response.done"}}); err != nil {
					return
				}
			}))
			defer server.Close()
			endpoint, _, err := gatewayEndpoint(server.URL, "test-user")
			if err != nil {
				t.Fatal(err)
			}
			connection, _, err := websocket.DefaultDialer.Dial(endpoint, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
			defer stop()
			if test.cancel {
				timer := time.AfterFunc(60*time.Millisecond, stop)
				defer timer.Stop()
			}
			err = runGatewayStream(ctx, connection, io.Discard, "fast", "test", nil, nil, 200*time.Millisecond)
			if test.wantIdle {
				if !errors.Is(err, neterror.ErrUpstreamStreamIdleTimeout) {
					t.Fatalf("error = %v, want idle timeout", err)
				}
				if neterror.IdleTimeoutObservedData(err) != test.wantData {
					t.Fatalf("observed data = %t, want %t", neterror.IdleTimeoutObservedData(err), test.wantData)
				}
			} else if test.cancel {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error = %v, want client cancellation", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGatewayActivityDeduplicatesSearchProgress(t *testing.T) {
	activity := newGatewayActivity()
	frames := []struct {
		name string
		data string
		want bool
	}{
		{name: "first start", data: `{"event":{"type":"response.chunk","chunk":{"tool_usage_card":{"id":"tool-1","web_search":{"query":"test"}}}}}`, want: true},
		{name: "duplicate start", data: `{"event":{"type":"response.chunk","chunk":{"tool_usage_card":{"id":"tool-1","web_search":{"query":"test"}}}}}`},
		{name: "result without ID", data: `{"event":{"type":"response.chunk","chunk":{"tool_result":{"web_search":{"webpages":[{"url":"https://example.com/first"}]}}}}}`, want: true},
		{name: "duplicate result", data: `{"event":{"type":"response.chunk","chunk":{"tool_result":{"web_search":{"webpages":[{"url":"https://example.com/first"}]}}}}}`},
		{name: "new result", data: `{"event":{"type":"response.chunk","chunk":{"tool_result":{"web_search":{"webpages":[{"url":"https://example.com/second"}]}}}}}`, want: true},
		{name: "pong", data: `{"event":{"type":"pong"}}`},
		{name: "empty delta", data: `{"event":{"type":"response.output_text.delta","delta":""}}`},
		{name: "malformed", data: `{"event":`},
	}
	for _, frame := range frames {
		t.Run(frame.name, func(t *testing.T) {
			if got := activity.observeFrame([]byte(frame.data)); got != frame.want {
				t.Fatalf("progress = %t, want %t", got, frame.want)
			}
		})
	}
}

func TestGatewaySemanticIdlePausesForDownstreamBackpressure(t *testing.T) {
	server := fhttptest.NewServer(fhttp.HandlerFunc(func(writer fhttp.ResponseWriter, request *fhttp.Request) {
		connection, err := (&websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		if _, _, err := connection.ReadMessage(); err != nil {
			return
		}
		if err := connection.WriteJSON(map[string]any{"event": map[string]any{"type": "response.output_text.delta", "delta": "answer"}}); err != nil {
			return
		}
		if err := connection.WriteJSON(map[string]any{"event": map[string]any{"type": "response.done"}}); err != nil {
			return
		}
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	endpoint, _, err := gatewayEndpoint(server.URL, "test-user")
	if err != nil {
		t.Fatal(err)
	}
	connection, _, err := websocket.DefaultDialer.Dial(endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	writer := &pausedGatewayWriter{started: make(chan struct{}), resume: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		result <- runGatewayStream(ctx, connection, writer, "fast", "test", nil, nil, 50*time.Millisecond)
	}()
	select {
	case <-writer.started:
	case <-ctx.Done():
		t.Fatal("gateway did not reach downstream write")
	}
	time.Sleep(100 * time.Millisecond)
	close(writer.resume)
	if err := <-result; err != nil {
		t.Fatalf("downstream pause consumed the upstream idle window: %v", err)
	}
}

type pausedGatewayWriter struct {
	started chan struct{}
	resume  chan struct{}
	paused  bool
}

func (writer *pausedGatewayWriter) Write(data []byte) (int, error) {
	if !writer.paused {
		writer.paused = true
		close(writer.started)
		<-writer.resume
	}
	return len(data), nil
}
