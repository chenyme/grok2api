package middleware

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
	"github.com/gin-gonic/gin"
)

type bodyMemoryObserver struct {
	active   atomic.Int64
	registry *perfmetrics.Registry
}

type observedRequestBody struct {
	io.ReadCloser
	observer *bodyMemoryObserver
	read     atomic.Int64
	peak     atomic.Int64
}

// ObserveBodyMemory records request-body pressure without delaying or
// rejecting requests. The tracked bytes remain active until the handler
// returns because JSON handlers retain their decoded body for that lifetime.
func ObserveBodyMemory() gin.HandlerFunc {
	return newBodyMemoryObserver().Middleware()
}

func newBodyMemoryObserver() *bodyMemoryObserver {
	registry := perfmetrics.Default
	observer := &bodyMemoryObserver{registry: registry}
	registry.RegisterDynamicGauge("http_request_body_active_bytes", perfmetrics.Labels{Subsystem: "http"}, observer.active.Load)
	return observer
}

func (o *bodyMemoryObserver) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body == nil || c.Request.Body == http.NoBody {
			c.Next()
			return
		}
		mode := "unknown_length"
		if c.Request.ContentLength >= 0 {
			mode = "known_length"
		}
		operation := inferenceBodyOperation(c.Request.URL.Path)
		labels := perfmetrics.Labels{Subsystem: "http", Operation: operation, Stage: mode}
		if c.Request.ContentLength > 0 {
			o.registry.Add("http_request_body_declared_bytes", labels, c.Request.ContentLength)
		}
		body := &observedRequestBody{ReadCloser: c.Request.Body, observer: o}
		c.Request.Body = body
		defer func() {
			read := body.read.Load()
			o.active.Add(-read)
			o.registry.Inc("http_request_body_total", labels)
			o.registry.Add("http_request_body_read_bytes", labels, read)
			o.registry.Add("http_request_body_active_peak_bytes", labels, body.peak.Load())
		}()
		c.Next()
	}
}

func (b *observedRequestBody) Read(buffer []byte) (int, error) {
	read, err := b.ReadCloser.Read(buffer)
	if read <= 0 {
		return read, err
	}
	b.read.Add(int64(read))
	current := b.observer.active.Add(int64(read))
	for {
		peak := b.peak.Load()
		if current <= peak || b.peak.CompareAndSwap(peak, current) {
			break
		}
	}
	return read, err
}

func inferenceBodyOperation(path string) string {
	switch strings.TrimSuffix(path, "/") {
	case "/v1/responses":
		return "responses"
	case "/v1/responses/compact":
		return "responses_compact"
	case "/v1/chat/completions":
		return "chat"
	case "/v1/messages":
		return "messages"
	case "/v1/images/generations":
		return "image_generation"
	case "/v1/images/edits":
		return "image_edit"
	case "/v1/videos/generations":
		return "video_generation"
	default:
		return "other"
	}
}
