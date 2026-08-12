package inference

import (
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

var voiceWSUpgrader = websocket.Upgrader{
	ReadBufferSize:  32 << 10,
	WriteBufferSize: 32 << 10,
	CheckOrigin: func(*http.Request) bool {
		return true
	},
	EnableCompression: true,
}

func (h *Handler) proxyRealtimeWebSocket(c *gin.Context) {
	h.proxyVoiceWebSocket(c, "/realtime")
}

func (h *Handler) proxyTTSWebSocket(c *gin.Context) {
	// POST /tts remains batch synthesis; GET with Upgrade is streaming TTS.
	if !websocket.IsWebSocketUpgrade(c.Request) {
		writeOpenAIError(c, http.StatusMethodNotAllowed, "invalid_request", "TTS 流式接口需要 WebSocket Upgrade")
		return
	}
	h.proxyVoiceWebSocket(c, "/tts")
}

func (h *Handler) proxySTTWebSocket(c *gin.Context) {
	if !websocket.IsWebSocketUpgrade(c.Request) {
		writeOpenAIError(c, http.StatusMethodNotAllowed, "invalid_request", "STT 流式接口需要 WebSocket Upgrade")
		return
	}
	h.proxyVoiceWebSocket(c, "/stt")
}

func (h *Handler) proxyVoiceWebSocket(c *gin.Context, pathValue string) {
	if !websocket.IsWebSocketUpgrade(c.Request) {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "请求不是有效的 WebSocket Upgrade")
		return
	}
	clientKey, requestID, ok := requestIdentity(c)
	if !ok {
		return
	}
	model := strings.TrimSpace(c.Query("model"))
	query := c.Request.URL.RawQuery
	session, err := h.gateway.OpenVoiceWebSocket(c.Request.Context(), gateway.VoiceWebSocketInput{
		RequestID: requestID, ClientKey: clientKey, PublicModel: model, Path: pathValue, Query: query,
	})
	if err != nil {
		writeGatewayError(c, err)
		return
	}

	clientConn, err := voiceWSUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		session.Close()
		return
	}

	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			_ = clientConn.Close()
			if session.Conn != nil {
				_ = session.Conn.Close()
			}
			session.Close()
		})
	}
	defer closeAll()

	errCh := make(chan error, 2)
	go func() {
		errCh <- proxyVoiceWSPump(func() (int, []byte, error) {
			return clientConn.ReadMessage()
		}, session.Conn.WriteMessage)
	}()
	go func() {
		errCh <- proxyVoiceWSPump(session.Conn.ReadMessage, clientConn.WriteMessage)
	}()
	<-errCh
}

func proxyVoiceWSPump(read func() (int, []byte, error), write func(int, []byte) error) error {
	for {
		messageType, payload, err := read()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if err := write(messageType, payload); err != nil {
			return err
		}
	}
}
