package egress

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPinnedHTTPSClientPreservesHostAndTLSServerName(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Host != "example.com" {
			t.Errorf("Host = %q", request.Host)
		}
		_, _ = io.WriteString(writer, "ok")
	}))
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	client, err := newPinnedHTTPSClient("", "example.com", &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodGet, server.URL+"/image.png", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "example.com"
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil || string(body) != "ok" {
		t.Fatalf("body=%q err=%v", body, readErr)
	}
}

func TestPinnedHTTPSClientHTTPProxyConnectsToPinnedIP(t *testing.T) {
	connected := make(chan string, 1)
	proxyServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connected <- request.Host
		http.Error(writer, "stop", http.StatusBadGateway)
	}))
	defer proxyServer.Close()
	client, err := newPinnedHTTPSClient(proxyServer.URL, "images.example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodGet, "https://93.184.216.34:443/image.png", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "images.example.test"
	_, _ = client.Do(request)
	select {
	case host := <-connected:
		if host != "93.184.216.34:443" {
			t.Fatalf("proxy CONNECT host = %q", host)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not receive CONNECT")
	}
}

func TestPinnedHTTPSClientSOCKS5HReceivesPinnedIP(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, "ok")
	}))
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	upstreamAddress := serverURL.Host
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requestedHost := make(chan string, 1)
	proxyDone := make(chan error, 1)
	go func() { proxyDone <- serveSOCKS5TunnelOnce(listener, upstreamAddress, requestedHost) }()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	client, err := newPinnedHTTPSClient("socks5h://"+listener.Addr().String(), "example.com", &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(upstreamAddress)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodGet, "https://127.0.0.1:"+port+"/image.png", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "example.com"
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	select {
	case host := <-requestedHost:
		if host != "127.0.0.1" {
			t.Fatalf("SOCKS target host = %q", host)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SOCKS proxy did not receive target")
	}
	select {
	case err := <-proxyDone:
		if err != nil && !strings.Contains(strings.ToLower(err.Error()), "closed") && !strings.Contains(strings.ToLower(err.Error()), "reset by peer") {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SOCKS proxy did not finish")
	}
}

func TestLeaseDoPinnedHTTPSRejectsHostnameTarget(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "https://images.example.test:443/image.png", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&Lease{}).DoPinnedHTTPS(request, "images.example.test"); err == nil {
		t.Fatal("hostname target was accepted")
	}
}

func TestLeaseDoPinnedHTTPSRejectsPrivateTarget(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "https://127.0.0.1:443/image.png", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "images.example.test"
	if _, err := (&Lease{}).DoPinnedHTTPS(request, "images.example.test"); err == nil {
		t.Fatal("private target was accepted")
	}
}
