package egress

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

func TestBuildClientUsesConfiguredResponseHeaderTimeout(t *testing.T) {
	client, err := newBuildClient("", 7*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	transport := client.Transport.(*http.Transport)
	if transport.ResponseHeaderTimeout != 7*time.Minute {
		t.Fatalf("response header timeout = %s", transport.ResponseHeaderTimeout)
	}
}

func TestBuildClientResponseHeaderTimeoutDoesNotLimitResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
		writer.(http.Flusher).Flush()
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(writer, "ok")
	}))
	defer server.Close()
	client, err := newBuildClient("", 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || string(body) != "ok" {
		t.Fatalf("body=%q err=%v", body, err)
	}
}

func TestBuildClientClassifiesDelayedResponseHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		time.Sleep(200 * time.Millisecond)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client, err := newBuildClient("", 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Get(server.URL)
	if !neterrorpkg.IsResponseHeaderTimeout(err) {
		t.Fatalf("error = %v", err)
	}
}

func TestNewBuildClientUsesStandardTransportForEveryProxyFamily(t *testing.T) {
	tests := []struct {
		name      string
		proxyURL  string
		httpProxy bool
	}{
		{name: "direct"},
		{name: "http", proxyURL: "http://user:password@127.0.0.1:8080", httpProxy: true},
		{name: "https", proxyURL: "https://proxy.example:8443", httpProxy: true},
		{name: "socks4", proxyURL: "socks4://127.0.0.1:1080"},
		{name: "socks4a", proxyURL: "socks4a://proxy.example:1080"},
		{name: "socks5", proxyURL: "socks5://127.0.0.1:1080"},
		{name: "socks5h warp", proxyURL: "socks5h://warp:1080"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, err := newBuildClient(test.proxyURL, 5*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			transport, ok := client.Transport.(*http.Transport)
			if !ok {
				t.Fatalf("transport = %T, want *http.Transport", client.Transport)
			}
			if transport.ForceAttemptHTTP2 != true || transport.DialContext == nil {
				t.Fatalf("standard transport not fully configured: %#v", transport)
			}
			if (transport.Proxy != nil) != test.httpProxy {
				t.Fatalf("http proxy callback configured=%v, want %v", transport.Proxy != nil, test.httpProxy)
			}
		})
	}
}

func TestNewBuildClientRejectsUnsupportedProxyScheme(t *testing.T) {
	if _, err := newBuildClient("ftp://proxy.example:21", 5*time.Minute); err == nil {
		t.Fatal("unsupported proxy scheme was accepted")
	}
}

func TestBuildClientRoutesThroughSOCKS5HWithRemoteDNS(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("User-Agent") != "grok-shell/0.2.102 (linux; x86_64)" {
			t.Errorf("User-Agent = %q", request.Header.Get("User-Agent"))
		}
		writer.Header().Set("Connection", "close")
		_, _ = io.WriteString(writer, "ok")
	}))
	defer target.Close()
	upstreamAddress := strings.TrimPrefix(target.URL, "http://")
	_, upstreamPort, err := net.SplitHostPort(upstreamAddress)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requestedHost := make(chan string, 1)
	proxyDone := make(chan error, 1)
	go func() { proxyDone <- serveSOCKS5TunnelOnce(listener, upstreamAddress, requestedHost) }()

	client, err := newBuildClient("socks5h://"+listener.Addr().String(), 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodGet, "http://cli-chat-proxy.grok.test:"+upstreamPort+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Close = true
	request.Header.Set("User-Agent", "grok-shell/0.2.102 (linux; x86_64)")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || string(body) != "ok" {
		t.Fatalf("body=%q err=%v", body, err)
	}
	select {
	case host := <-requestedHost:
		if host != "cli-chat-proxy.grok.test" {
			t.Fatalf("SOCKS target host = %q", host)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SOCKS proxy did not receive a target hostname")
	}
	select {
	case err := <-proxyDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SOCKS proxy did not finish")
	}
}

func serveSOCKS5TunnelOnce(listener net.Listener, upstreamAddress string, requestedHost chan<- string) error {
	connection, err := listener.Accept()
	if err != nil {
		return err
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	var greeting [2]byte
	if _, err := io.ReadFull(connection, greeting[:]); err != nil {
		return err
	}
	if greeting[0] != 5 {
		return fmt.Errorf("SOCKS version = %d", greeting[0])
	}
	methods := make([]byte, int(greeting[1]))
	if _, err := io.ReadFull(connection, methods); err != nil {
		return err
	}
	if _, err := connection.Write([]byte{5, 0}); err != nil {
		return err
	}
	var header [4]byte
	if _, err := io.ReadFull(connection, header[:]); err != nil {
		return err
	}
	if header[0] != 5 || header[1] != 1 {
		return fmt.Errorf("SOCKS request header = %v", header)
	}
	host, err := readSOCKS5Host(connection, header[3])
	if err != nil {
		return err
	}
	var port [2]byte
	if _, err := io.ReadFull(connection, port[:]); err != nil {
		return err
	}
	_ = binary.BigEndian.Uint16(port[:])
	requestedHost <- host

	upstream, err := net.DialTimeout("tcp", upstreamAddress, 2*time.Second)
	if err != nil {
		return err
	}
	defer upstream.Close()
	_ = upstream.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := connection.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}); err != nil {
		return err
	}
	copyDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(upstream, connection)
		copyDone <- copyErr
	}()
	_, downstreamErr := io.Copy(connection, upstream)
	upstreamErr := <-copyDone
	if downstreamErr != nil && !isClosedNetworkError(downstreamErr) {
		return downstreamErr
	}
	if upstreamErr != nil && !isClosedNetworkError(upstreamErr) {
		return upstreamErr
	}
	return nil
}

func readSOCKS5Host(reader io.Reader, addressType byte) (string, error) {
	switch addressType {
	case 1:
		value := make([]byte, net.IPv4len)
		_, err := io.ReadFull(reader, value)
		return net.IP(value).String(), err
	case 3:
		var size [1]byte
		if _, err := io.ReadFull(reader, size[:]); err != nil {
			return "", err
		}
		value := make([]byte, int(size[0]))
		_, err := io.ReadFull(reader, value)
		return string(value), err
	case 4:
		value := make([]byte, net.IPv6len)
		_, err := io.ReadFull(reader, value)
		return net.IP(value).String(), err
	default:
		return "", fmt.Errorf("SOCKS address type = %d", addressType)
	}
}

func isClosedNetworkError(err error) bool {
	return err == nil || strings.Contains(strings.ToLower(err.Error()), "closed network connection")
}
