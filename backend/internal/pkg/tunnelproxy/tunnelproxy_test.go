package tunnelproxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Asutorufa/yuhaiin/pkg/net/netapi"
	"github.com/Asutorufa/yuhaiin/pkg/protos/node/protocol"
	"github.com/coder/websocket"
)

const testUUID = "123e4567-e89b-12d3-a456-426614174000"

func TestNormalizeStripsRemarksAndBuildsDialers(t *testing.T) {
	vmessJSON := fmt.Sprintf(`{"v":"2","ps":"node one","add":"proxy.example","port":"443","id":%q,"aid":"0","scy":"auto","net":"ws","tls":"tls","sni":"edge.example","host":"edge.example","path":"/ws"}`, testUUID)
	vmessURL := "vmess://" + base64.RawStdEncoding.EncodeToString([]byte(vmessJSON))
	tests := []struct {
		name string
		raw  string
	}{
		{name: "trojan", raw: "trojan://secret@proxy.example:443?security=tls&sni=edge.example&type=ws&host=edge.example&path=%2Fws#one"},
		{name: "vless", raw: "vless://" + testUUID + "@proxy.example:443?encryption=none&security=tls&sni=edge.example&type=ws&host=edge.example&path=%2Fws#one"},
		{name: "shadowsocks", raw: "ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-128-gcm:secret")) + "@proxy.example:8388#one"},
		{name: "vmess", raw: vmessURL},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			normalized, err := Normalize(test.raw)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(normalized, "#") || strings.Contains(normalized, "node one") {
				t.Fatalf("remark leaked into normalized URL: %q", normalized)
			}
			if _, err := NewDialer(normalized); err != nil {
				t.Fatalf("construct dialer: %v", err)
			}
		})
	}
}

func TestNormalizeRemarksDoNotChangeIdentity(t *testing.T) {
	base := "trojan://secret@proxy.example:443?security=tls&sni=edge.example"
	one, err := Normalize(base + "#one")
	if err != nil {
		t.Fatal(err)
	}
	two, err := Normalize(base + "#two")
	if err != nil {
		t.Fatal(err)
	}
	if one != two {
		t.Fatalf("remark changed identity: %q != %q", one, two)
	}
}

func TestNormalizeEquivalentShareLinksHaveOneIdentity(t *testing.T) {
	trojanOne, err := Normalize("trojan://secret@proxy.example:443?type=websocket&security=tls&sni=edge.example&host=edge.example&path=ws")
	if err != nil {
		t.Fatal(err)
	}
	trojanTwo, err := Normalize("trojan://secret@proxy.example:443?network=ws&peer=edge.example&host=edge.example&path=%2Fws")
	if err != nil {
		t.Fatal(err)
	}
	if trojanOne != trojanTwo {
		t.Fatalf("equivalent Trojan links have different identities: %q != %q", trojanOne, trojanTwo)
	}

	vmessOne := vmessTestURL(t, map[string]any{
		"v": "2", "add": "proxy.example", "port": "443", "id": testUUID,
		"aid": "0", "scy": "auto", "net": "ws", "tls": "tls",
		"sni": "edge.example", "host": "edge.example", "path": "ws",
	})
	vmessTwo := vmessTestURL(t, map[string]any{
		"v": "2", "add": "proxy.example", "port": "443", "id": testUUID,
		"aid": "0", "scy": "auto", "net": "ws", "tls": "tls",
		"sni": "edge.example", "host": "edge.example", "path": "/ws",
	})
	normalizedOne, err := Normalize(vmessOne)
	if err != nil {
		t.Fatal(err)
	}
	normalizedTwo, err := Normalize(vmessTwo)
	if err != nil {
		t.Fatal(err)
	}
	if normalizedOne != normalizedTwo {
		t.Fatalf("equivalent VMess links have different identities: %q != %q", normalizedOne, normalizedTwo)
	}
}

func TestOwnedVLESSProxyClosesConnectionWhenInitialWriteFails(t *testing.T) {
	client, server := net.Pipe()
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	tracked := &closeTrackingConn{Conn: client}
	config := &protocol.Vless{}
	config.SetUuid(testUUID)
	proxy, err := newOwnedVLESSProxy(config, &singleConnectionProxy{connection: tracked})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proxy.Conn(context.Background(), netapi.ParseDomainPort("tcp", "target.example", 443)); err == nil {
		t.Fatal("VLESS initial write unexpectedly succeeded")
	}
	if !tracked.closed {
		t.Fatal("failed VLESS connection was not closed")
	}
}

func TestTunnelHandshakeContextHasIndependentDeadline(t *testing.T) {
	ctx, cancel := newTunnelHandshakeContext(context.Background())
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("tunnel handshake context has no deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > tunnelHandshakeTimeout {
		t.Fatalf("tunnel handshake deadline remaining = %v", remaining)
	}
}

func TestNormalizeSupportsLegacyShadowsocks(t *testing.T) {
	raw := "ss://" + base64.RawStdEncoding.EncodeToString([]byte("chacha20-ietf-poly1305:secret@proxy.example:8388")) + "#legacy"
	normalized, err := Normalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(normalized, "ss://") || strings.Contains(normalized, "#") {
		t.Fatalf("normalized legacy URL = %q", normalized)
	}
}

func TestParseRejectsUnsupportedOrMalformedLinks(t *testing.T) {
	for _, raw := range []string{
		"hysteria2://secret@proxy.example:443",
		"tuic://user:secret@proxy.example:443",
		"vless://not-a-uuid@proxy.example:443?encryption=none",
		"vless://" + testUUID + "@proxy.example:443?encryption=none&flow=xtls-rprx-vision",
		"trojan://secret@proxy.example:443?type=grpc",
		"trojan://secret@proxy.example:443/unexpected",
		"ss://" + base64.RawURLEncoding.EncodeToString([]byte("rc4-md5:secret")) + "@proxy.example:8388",
		"ss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-128-gcm:secret")) + "@proxy.example:8388/unexpected",
		"vmess://not-base64",
	} {
		if _, err := Normalize(raw); err == nil {
			t.Fatalf("invalid tunnel accepted: %q", raw)
		}
	}
}

func TestWebSocketTransportCarriesBinaryStream(t *testing.T) {
	handlerResult := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Host != "edge.example" || request.URL.Path != "/ws" {
			handlerResult <- fmt.Errorf("request host=%q path=%q", request.Host, request.URL.Path)
			return
		}
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			handlerResult <- err
			return
		}
		stream := websocket.NetConn(context.Background(), connection, websocket.MessageBinary)
		defer stream.Close()
		_, err = io.CopyN(stream, stream, 4)
		handlerResult <- err
	}))
	defer server.Close()
	serverAddress := strings.TrimPrefix(server.URL, "http://")
	proxy := &websocketProxy{
		config: Config{Server: serverAddress, WebSocketHost: "edge.example", WebSocketPath: "/ws"},
		dialer: &serverDialer{address: serverAddress},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	connection, err := proxy.Conn(ctx, netapi.ParseDomainPort("tcp", "target.example", 443))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte("pong")); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 4)
	if _, err := io.ReadFull(connection, response); err != nil || string(response) != "pong" {
		t.Fatalf("response=%q err=%v", response, err)
	}
	if err := <-handlerResult; err != nil {
		t.Fatal(err)
	}
}

type closeTrackingConn struct {
	net.Conn
	closed bool
}

func (c *closeTrackingConn) Close() error {
	c.closed = true
	return c.Conn.Close()
}

func vmessTestURL(t *testing.T, config map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	return "vmess://" + base64.RawStdEncoding.EncodeToString(payload)
}
