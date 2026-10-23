package inference

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func newKeepaliveTestWriter(t *testing.T) (*httptest.ResponseRecorder, gin.ResponseWriter) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	return recorder, context.Writer
}

// An idle stream must receive SSE comment heartbeats so a proxy with a short
// origin read timeout does not drop a long high-reasoning turn.
func TestStreamKeepaliveWritesCommentWhileIdle(t *testing.T) {
	recorder, writer := newKeepaliveTestWriter(t)
	keepalive := newStreamKeepalive(writer, 10*time.Millisecond)
	defer keepalive.stop()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(recorder.Body.String(), streamKeepaliveComment) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no keepalive comment was written while idle: %q", recorder.Body.String())
}

// Recent writes mean the connection is alive, so no comment may be injected
// between them.
func TestStreamKeepaliveSkipsWhenStreamIsActive(t *testing.T) {
	recorder, writer := newKeepaliveTestWriter(t)
	keepalive := newStreamKeepalive(writer, 200*time.Millisecond)
	defer keepalive.stop()

	if _, err := keepalive.WriteString(`data: {"ok":true}` + "\n\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(120 * time.Millisecond)

	if body := recorder.Body.String(); strings.Contains(body, streamKeepaliveComment) {
		t.Fatalf("keepalive comment written into an active stream: %q", body)
	}
}

// stop() must be idempotent and must not emit anything after the handler has
// returned.
func TestStreamKeepaliveStopIsIdempotent(t *testing.T) {
	recorder, writer := newKeepaliveTestWriter(t)
	keepalive := newStreamKeepalive(writer, 10*time.Millisecond)

	keepalive.stop()
	keepalive.stop()

	recorded := recorder.Body.Len()
	time.Sleep(50 * time.Millisecond)
	if recorder.Body.Len() != recorded {
		t.Fatalf("keepalive wrote after stop: %q", recorder.Body.String())
	}
}

// Unwrap keeps http.ResponseController able to reach the real writer.
func TestStreamKeepaliveUnwrap(t *testing.T) {
	_, writer := newKeepaliveTestWriter(t)
	keepalive := newStreamKeepalive(writer, 0)
	defer keepalive.stop()

	if keepalive.Unwrap() != writer {
		t.Fatal("Unwrap must return the wrapped ResponseWriter")
	}
}
