package egress

import (
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptrace"
	"strings"
)

// do retries only the connection phase of an account-bound proxy request.
// Once the request is written to the upstream tunnel, replaying a POST could
// duplicate generation or billing and is therefore never attempted here.
func (l *Lease) do(request *http.Request) (*http.Response, error) {
	if l == nil || l.client == nil {
		return nil, errors.New("出口客户端未初始化")
	}
	if !l.sticky {
		return l.client.Do(request)
	}
	current := request
	for attempt := 0; ; attempt++ {
		written := false
		trace := &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) {
			// The callback also fires when writing fails after a partial write;
			// treat that as submitted because the upstream may have received it.
			written = true
		}}
		traced := current.WithContext(httptrace.WithClientTrace(current.Context(), trace))
		response, err := l.client.Do(traced)
		if err == nil && !retryableResinResponse(response) {
			return response, nil
		}
		if attempt >= stickyProxyRetryLimit || written || !safeProxyConnectionFailure(err, response) {
			if err != nil {
				return nil, err
			}
			return response, nil
		}
		next, cloneErr := cloneRequestBody(request)
		if cloneErr != nil {
			if err != nil {
				return nil, err
			}
			return response, nil
		}
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		l.client.CloseIdleConnections()
		current = next
	}
}

func cloneRequestBody(request *http.Request) (*http.Request, error) {
	if request == nil {
		return nil, errors.New("请求为空")
	}
	if request.Body == nil || request.Body == http.NoBody {
		return request.Clone(request.Context()), nil
	}
	if request.GetBody == nil {
		return nil, errors.New("请求体不可重放")
	}
	body, err := request.GetBody()
	if err != nil {
		return nil, err
	}
	cloned := request.Clone(request.Context())
	cloned.Body = body
	return cloned, nil
}

func safeProxyConnectionFailure(err error, response *http.Response) bool {
	if response != nil {
		resinError := strings.ToUpper(strings.TrimSpace(response.Header.Get("X-Resin-Error")))
		return response.StatusCode >= http.StatusBadGateway && (resinError == "UPSTREAM_CONNECT_FAILED" || resinError == "NO_AVAILABLE_NODES")
	}
	if err == nil {
		return false
	}
	value := strings.ToLower(err.Error())
	for _, marker := range []string{
		"proxyconnect", "socks connect", "socks5: authentication", "tls handshake timeout",
		"connection refused", "no route to host",
	} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	var tlsError *tls.RecordHeaderError
	return errors.As(err, &tlsError)
}

func retryableResinResponse(response *http.Response) bool {
	if response == nil {
		return false
	}
	resinError := strings.ToUpper(strings.TrimSpace(response.Header.Get("X-Resin-Error")))
	return (resinError == "UPSTREAM_CONNECT_FAILED" || resinError == "NO_AVAILABLE_NODES") && response.StatusCode >= 502
}
