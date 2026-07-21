package egress

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
)

type scriptedRequestClient struct {
	calls      int
	closedIdle int
	do         func(int, *http.Request) (*http.Response, error)
}

func (c *scriptedRequestClient) Do(request *http.Request) (*http.Response, error) {
	c.calls++
	return c.do(c.calls, request)
}

func (c *scriptedRequestClient) CloseIdleConnections() { c.closedIdle++ }

func TestStickyLeaseRetriesSafeProxyConnectFailure(t *testing.T) {
	client := &scriptedRequestClient{do: func(call int, _ *http.Request) (*http.Response, error) {
		if call == 1 {
			return nil, errors.New("proxyconnect tcp: connection refused")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
	}}
	lease := &Lease{client: client, sticky: true, proxyPool: true}
	request, err := http.NewRequest(http.MethodPost, "https://example.com/generate", bytes.NewReader([]byte("payload")))
	if err != nil {
		t.Fatal(err)
	}
	response, err := lease.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	if client.calls != 2 || client.closedIdle != 1 {
		t.Fatalf("calls=%d closedIdle=%d", client.calls, client.closedIdle)
	}
}

func TestStickyLeaseRetriesExplicitResinConnectResponse(t *testing.T) {
	client := &scriptedRequestClient{do: func(call int, _ *http.Request) (*http.Response, error) {
		if call == 1 {
			return &http.Response{
				StatusCode: http.StatusBadGateway,
				Header:     http.Header{"X-Resin-Error": []string{"UPSTREAM_CONNECT_FAILED"}},
				Body:       io.NopCloser(strings.NewReader("connect failed")),
			}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
	}}
	lease := &Lease{client: client, sticky: true, proxyPool: true}
	request, err := http.NewRequest(http.MethodGet, "https://example.com/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := lease.Do(request)
	if err != nil || response.StatusCode != http.StatusOK || client.calls != 2 {
		t.Fatalf("response=%#v calls=%d err=%v", response, client.calls, err)
	}
}

func TestStickyLeaseDoesNotRetryUnsafeUpstreamOutcomes(t *testing.T) {
	for _, test := range []struct {
		name     string
		response *http.Response
	}{
		{name: "unauthorized", response: &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: http.NoBody}},
		{name: "rate limited", response: &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header), Body: http.NoBody}},
		{name: "request may have reached upstream", response: &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{"X-Resin-Error": []string{"UPSTREAM_REQUEST_FAILED"}}, Body: http.NoBody}},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &scriptedRequestClient{do: func(int, *http.Request) (*http.Response, error) { return test.response, nil }}
			lease := &Lease{client: client, sticky: true, proxyPool: true}
			request, err := http.NewRequest(http.MethodPost, "https://example.com/generate", bytes.NewReader([]byte("payload")))
			if err != nil {
				t.Fatal(err)
			}
			response, err := lease.Do(request)
			if err != nil || response != test.response || client.calls != 1 {
				t.Fatalf("response=%#v calls=%d err=%v", response, client.calls, err)
			}
		})
	}
}

func TestStickyLeaseDoesNotRetryAfterRequestWasWritten(t *testing.T) {
	client := &scriptedRequestClient{do: func(_ int, request *http.Request) (*http.Response, error) {
		trace := httptrace.ContextClientTrace(request.Context())
		if trace != nil && trace.WroteRequest != nil {
			trace.WroteRequest(httptrace.WroteRequestInfo{})
		}
		return nil, errors.New("proxyconnect tcp: connection refused")
	}}
	lease := &Lease{client: client, sticky: true, proxyPool: true}
	request, err := http.NewRequest(http.MethodPost, "https://example.com/generate", bytes.NewReader([]byte("payload")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lease.Do(request); err == nil {
		t.Fatal("written request error was unexpectedly swallowed")
	}
	if client.calls != 1 || client.closedIdle != 1 {
		t.Fatalf("calls=%d closedIdle=%d", client.calls, client.closedIdle)
	}
}

func TestStickyLeaseKeepsNonReplayableResinResponseReadable(t *testing.T) {
	body := io.NopCloser(strings.NewReader("connect failed"))
	resinResponse := &http.Response{
		StatusCode: http.StatusBadGateway,
		Header:     http.Header{"X-Resin-Error": []string{"UPSTREAM_CONNECT_FAILED"}},
		Body:       body,
	}
	client := &scriptedRequestClient{do: func(int, *http.Request) (*http.Response, error) { return resinResponse, nil }}
	lease := &Lease{client: client, sticky: true, proxyPool: true}
	request, err := http.NewRequest(http.MethodPost, "https://example.com/generate", io.NopCloser(strings.NewReader("payload")))
	if err != nil {
		t.Fatal(err)
	}
	response, err := lease.Do(request)
	if err != nil || response != resinResponse || client.calls != 1 {
		t.Fatalf("response=%#v calls=%d err=%v", response, client.calls, err)
	}
	if client.closedIdle != 1 {
		t.Fatalf("closedIdle=%d, want 1", client.closedIdle)
	}
	data, err := io.ReadAll(response.Body)
	if err != nil || string(data) != "connect failed" {
		t.Fatalf("body=%q err=%v", data, err)
	}
}
