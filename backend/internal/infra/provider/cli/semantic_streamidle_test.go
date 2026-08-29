package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

func TestBuildSemanticIdleIgnoresNonGeneratedEvents(t *testing.T) {
	tests := []struct {
		name  string
		chunk string
	}{
		{name: "keepalive comment", chunk: ": keep-alive\n\n"},
		{name: "doom loop control", chunk: "event: response.doom_loop_check\ndata: {\"type\":\"response.doom_loop_check\"}\n\n"},
		{name: "lifecycle event", chunk: "event: response.in_progress\ndata: {\"type\":\"response.in_progress\"}\n\n"},
		{name: "empty generated delta", chunk: "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"\"}\n\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader, writer := io.Pipe()
			body := wrapBuildSemanticIdle(reader, 60*time.Millisecond)
			readDone := make(chan error, 1)
			go func() {
				_, err := io.Copy(io.Discard, body)
				readDone <- err
			}()
			writeDone := make(chan struct{})
			go func() {
				defer close(writeDone)
				defer writer.Close()
				for {
					if _, err := io.WriteString(writer, test.chunk); err != nil {
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
			}()

			select {
			case err := <-readDone:
				if !errors.Is(err, neterror.ErrUpstreamStreamIdleTimeout) {
					t.Fatalf("body read error = %v, want ErrUpstreamStreamIdleTimeout", err)
				}
			case <-time.After(time.Second):
				t.Fatal("semantic idle timeout did not interrupt the stream")
			}
			<-writeDone
		})
	}
}

func TestBuildSemanticIdleResetsOnGeneratedDelta(t *testing.T) {
	reader, writer := io.Pipe()
	body := wrapBuildSemanticIdle(reader, 300*time.Millisecond)
	readDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, body)
		readDone <- err
	}()

	chunk := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n"
	if _, err := io.WriteString(writer, chunk); err != nil {
		t.Fatal(err)
	}
	time.Sleep(180 * time.Millisecond)
	if _, err := io.WriteString(writer, chunk); err != nil {
		t.Fatal(err)
	}
	time.Sleep(180 * time.Millisecond)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("body read error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("generated deltas did not keep the stream alive")
	}
}

func TestBuildSemanticIdleResetsOnGeneratedOutputItem(t *testing.T) {
	reader, writer := io.Pipe()
	body := wrapBuildSemanticIdle(reader, 300*time.Millisecond)
	readDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, body)
		readDone <- err
	}()

	time.Sleep(180 * time.Millisecond)
	searchStarted := "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"ws_1\",\"type\":\"web_search_call\",\"status\":\"in_progress\",\"action\":{\"type\":\"search\",\"query\":\"current docs\"}}}\n\n"
	if _, err := io.WriteString(writer, searchStarted); err != nil {
		t.Fatal(err)
	}
	time.Sleep(180 * time.Millisecond)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("body read error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("generated output item did not keep the stream alive")
	}
}

func TestBuildSSEActivityDetectorRecognizesSplitGeneratedEvents(t *testing.T) {
	for kind := range buildGeneratedDeltaEvents {
		t.Run(kind, func(t *testing.T) {
			var detector buildSSEActivityDetector
			event := fmt.Sprintf("event: %s\r\ndata: {\"type\":%q,\"delta\":\"x\"}\r\n\r\n", kind, kind)
			active := false
			for index := range event {
				active = detector.Observe([]byte(event[index:index+1])) || active
			}
			if !active {
				t.Fatal("split generated event was not recognized")
			}
		})
	}
}

func TestBuildSSEActivityDetectorRequiresJSONGeneratedEvent(t *testing.T) {
	var detector buildSSEActivityDetector
	if detector.Observe([]byte("event: response.output_text.delta\ndata: not-json\n\n")) {
		t.Fatal("malformed event unexpectedly counted as generated activity")
	}
	if detector.Observe([]byte(": keep-alive\n\n")) {
		t.Fatal("keepalive unexpectedly counted as generated activity")
	}
	if detector.Observe([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"\"}\n\n")) {
		t.Fatal("empty delta unexpectedly counted as generated activity")
	}
	if detector.Observe([]byte("event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"web_search_call\"}}\n\n")) {
		t.Fatal("unidentified output item unexpectedly counted as generated activity")
	}
	if detector.Observe([]byte("event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"web_search_call\",\"status\":\"in_progress\"}}\n\n")) {
		t.Fatal("anonymous output item unexpectedly counted as generated activity")
	}
	if detector.Observe([]byte("event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"control_1\",\"type\":\"private_control\"}}\n\n")) {
		t.Fatal("unknown output item unexpectedly counted as generated activity")
	}
}

func TestBuildSSEActivityDetectorRecognizesGeneratedOutputItems(t *testing.T) {
	tests := []string{
		`{"type":"response.output_item.added","item":{"id":"ws_1","type":"web_search_call","status":"in_progress"}}`,
		`{"type":"response.output_item.added","item":{"id":"reasoning_1","type":"reasoning"}}`,
		`{"type":"response.output_item.done","item":{"call_id":"call_1","type":"function_call","name":"lookup","status":"completed"}}`,
	}
	for _, payload := range tests {
		var detector buildSSEActivityDetector
		if !detector.Observe([]byte("data: " + payload + "\n\n")) {
			t.Fatalf("generated output item was not recognized: %s", payload)
		}
	}
}

func TestBuildSemanticIdlePausesWhileNotReading(t *testing.T) {
	reader, writer := io.Pipe()
	body := wrapBuildSemanticIdle(reader, 60*time.Millisecond)
	chunk := "event: response.reasoning_summary_text.delta\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"x\"}\n\n"
	writeErr := make(chan error, 2)
	go func() {
		_, err := io.WriteString(writer, chunk)
		writeErr <- err
	}()
	buf := make([]byte, len(chunk))
	n, err := body.Read(buf)
	if err != nil || n != len(chunk) {
		t.Fatalf("first read n=%d err=%v", n, err)
	}
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if body.(*semanticIdleReadCloser).TimedOut() {
		t.Fatal("idle timer fired while nobody was reading upstream")
	}
	go func() {
		_, err := io.WriteString(writer, chunk)
		writeErr <- err
		_ = writer.Close()
	}()
	n, err = body.Read(buf)
	if err != nil || n != len(chunk) {
		t.Fatalf("resume read n=%d err=%v", n, err)
	}
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, body); err != nil {
		t.Fatal(err)
	}
}

func TestBuildSemanticIdleKeepalivesStillTimeoutWhileReading(t *testing.T) {
	reader, writer := io.Pipe()
	body := wrapBuildSemanticIdle(reader, 60*time.Millisecond)
	readDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, body)
		readDone <- err
	}()
	go func() {
		defer writer.Close()
		for {
			if _, err := io.WriteString(writer, ": keep-alive\n\n"); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	select {
	case err := <-readDone:
		if !errors.Is(err, neterror.ErrUpstreamStreamIdleTimeout) {
			t.Fatalf("body read error = %v, want ErrUpstreamStreamIdleTimeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("keepalives while reading did not idle-timeout")
	}
}

func TestBuildSemanticIdleStopsAfterEOFOrClose(t *testing.T) {
	t.Run("EOF", func(t *testing.T) {
		inner := &countingReadCloser{Reader: strings.NewReader("done")}
		body := wrapBuildSemanticIdle(inner, 20*time.Millisecond).(*semanticIdleReadCloser)
		if _, err := io.ReadAll(body); err != nil {
			t.Fatal(err)
		}
		time.Sleep(40 * time.Millisecond)
		if body.TimedOut() {
			t.Fatal("timer fired after EOF")
		}
		if err := body.Close(); err != nil {
			t.Fatal(err)
		}
		if got := inner.closes.Load(); got != 1 {
			t.Fatalf("inner Close calls = %d, want 1", got)
		}
	})

	t.Run("Close", func(t *testing.T) {
		inner := &countingReadCloser{Reader: strings.NewReader("")}
		body := wrapBuildSemanticIdle(inner, 20*time.Millisecond).(*semanticIdleReadCloser)
		if err := body.Close(); err != nil {
			t.Fatal(err)
		}
		if err := body.Close(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(40 * time.Millisecond)
		if body.TimedOut() {
			t.Fatal("timer fired after Close")
		}
		if got := inner.closes.Load(); got != 1 {
			t.Fatalf("inner Close calls = %d, want 1", got)
		}
	})
}

type countingReadCloser struct {
	io.Reader
	closes atomic.Int32
}

func (r *countingReadCloser) Close() error {
	r.closes.Add(1)
	return nil
}
