package inference

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// streamKeepaliveInterval is how long the upstream may stay silent before an
// SSE comment is written to the downstream connection.
//
// Cloudflare drops an origin connection after roughly 120s without bytes, and
// intermediate clients (relays, agents, terminals) often give up on an empty
// first byte much earlier, so long high-reasoning turns must keep writing
// during the silent window. 15s leaves a wide margin on both sides.
const streamKeepaliveInterval = 15 * time.Second

// streamKeepaliveComment is an SSE comment. SSE parsers must ignore comment
// lines, so it never reaches the message body of any protocol we proxy.
const streamKeepaliveComment = ": keepalive\n\n"

// streamKeepalive wraps a streaming response so an idle upstream cannot leave
// the downstream connection silent long enough to be dropped.
//
// Every write path takes the lock, which serves two purposes: the response keeps
// a single writer, and stop() can guarantee the heartbeat goroutine has exited
// before the handler returns, so no write can race the handler's completion.
type streamKeepalive struct {
	gin.ResponseWriter
	interval time.Duration
	mu       sync.Mutex
	once     sync.Once
	last     time.Time
	done     chan struct{}
	finished chan struct{}
}

func newStreamKeepalive(writer gin.ResponseWriter, interval time.Duration) *streamKeepalive {
	value := &streamKeepalive{
		ResponseWriter: writer,
		interval:       interval,
		last:           time.Now(),
		done:           make(chan struct{}),
		finished:       make(chan struct{}),
	}
	if interval > 0 {
		go value.loop()
	} else {
		close(value.finished)
	}
	return value
}

func (k *streamKeepalive) loop() {
	defer close(k.finished)
	ticker := time.NewTicker(k.interval)
	defer ticker.Stop()
	for {
		select {
		case <-k.done:
			return
		case now := <-ticker.C:
			k.mu.Lock()
			// Only fill a real gap: recent bytes mean the connection is alive.
			if now.Sub(k.last) >= k.interval {
				_, _ = k.ResponseWriter.WriteString(streamKeepaliveComment)
				k.ResponseWriter.Flush()
				k.last = time.Now()
			}
			k.mu.Unlock()
		}
	}
}

func (k *streamKeepalive) Write(payload []byte) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.last = time.Now()
	return k.ResponseWriter.Write(payload)
}

func (k *streamKeepalive) WriteString(value string) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.last = time.Now()
	return k.ResponseWriter.WriteString(value)
}

func (k *streamKeepalive) Flush() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.ResponseWriter.Flush()
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (k *streamKeepalive) Unwrap() http.ResponseWriter { return k.ResponseWriter }

// stop ends the heartbeat and waits for the goroutine, so no write can happen
// after the handler returns. It is safe to call more than once.
func (k *streamKeepalive) stop() {
	k.once.Do(func() { close(k.done) })
	<-k.finished
}
