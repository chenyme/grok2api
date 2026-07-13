package egress

import (
	"context"
	"io"
	"net/http"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"github.com/bogdanfinn/websocket"
)

type browserClient struct{ inner tlsclient.HttpClient }

func (l *Lease) DialWebSocket(ctx context.Context, endpoint string, headers fhttp.Header, handshakeTimeout time.Duration) (*websocket.Conn, *fhttp.Response, error) {
	dialer := &websocket.Dialer{
		HandshakeTimeout:  handshakeTimeout,
		NetDialTLSContext: l.client.inner.GetTLSDialer(),
		NetDialContext:    l.client.inner.GetDialer().DialContext,
	}
	return dialer.DialContext(ctx, endpoint, headers)
}

func newBrowserClient(proxyURL string) (*browserClient, error) {
	options := []tlsclient.HttpClientOption{
		tlsclient.WithTimeoutSeconds(7200),
		tlsclient.WithClientProfile(profiles.Chrome_146),
		tlsclient.WithNotFollowRedirects(),
	}
	if proxyURL != "" {
		options = append(options, tlsclient.WithProxyUrl(proxyURL))
	}
	client, err := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(), options...)
	if err != nil {
		return nil, err
	}
	return &browserClient{inner: client}, nil
}

func (c *browserClient) Do(request *http.Request) (*http.Response, error) {
	frequest, err := toFHTTPRequest(request)
	if err != nil {
		return nil, err
	}
	fresponse, err := c.inner.Do(frequest)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		Status: fresponse.Status, StatusCode: fresponse.StatusCode, Proto: fresponse.Proto,
		ProtoMajor: fresponse.ProtoMajor, ProtoMinor: fresponse.ProtoMinor, Header: http.Header(fresponse.Header),
		Body: fresponse.Body, ContentLength: fresponse.ContentLength, TransferEncoding: fresponse.TransferEncoding,
		Close: fresponse.Close, Uncompressed: fresponse.Uncompressed, Trailer: http.Header(fresponse.Trailer),
	}, nil
}

func toFHTTPRequest(request *http.Request) (*fhttp.Request, error) {
	var body io.Reader
	if request.Body != nil {
		body = request.Body
	}
	result, err := fhttp.NewRequestWithContext(request.Context(), request.Method, request.URL.String(), body)
	if err != nil {
		return nil, err
	}
	result.ContentLength = request.ContentLength
	result.TransferEncoding = append([]string(nil), request.TransferEncoding...)
	result.Close = request.Close
	if request.Host != "" {
		result.Host = request.Host
	}
	if request.GetBody != nil {
		result.GetBody = request.GetBody
	}
	result.Trailer = fhttp.Header(request.Trailer.Clone())
	for name, values := range request.Header {
		for _, value := range values {
			result.Header.Add(name, value)
		}
	}
	return result, nil
}
