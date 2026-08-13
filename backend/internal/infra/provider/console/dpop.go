package console

import (
	"bytes"
	"container/list"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"
)

const (
	dpopSessionCacheLimit = 4096
	dpopRefreshSkew       = 20 * time.Second
	maxDPoPTokenLifetime  = time.Hour
)

type dpopJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

type dpopSession struct {
	accessToken string
	privateKey  *ecdsa.PrivateKey
	publicJWK   dpopJWK
	expiresAt   time.Time
	// clockSkew is (server time − local time) learned from the DPoP mint
	// response Date header. Zero is a valid value meaning "no correction".
	// Every session always carries an explicit skew so proof iat never depends
	// on an uninitialized field.
	clockSkew time.Duration
}

type dpopSessionManager struct {
	mu       sync.Mutex
	sessions map[string]*dpopSessionEntry
	lru      list.List
	loads    singleflight.Group
	now      func() time.Time
}

type dpopSessionEntry struct {
	key     string
	session dpopSession
	element *list.Element
}

type dpopTokenEndpointError struct {
	status        int
	statusText    string
	header        http.Header
	body          []byte
	bodyTruncated bool
	request       *http.Request
}

func (e *dpopTokenEndpointError) Error() string {
	suffix := ""
	if e.bodyTruncated {
		suffix = " (response truncated)"
	}
	return fmt.Sprintf("Console DPoP token 接口返回 %d%s", e.status, suffix)
}

func (e *dpopTokenEndpointError) response() *http.Response {
	header := e.header.Clone()
	header.Set("Content-Length", strconv.Itoa(len(e.body)))
	if e.bodyTruncated {
		header.Set("X-Grok2API-Body-Truncated", "1")
	}
	return &http.Response{
		StatusCode: e.status, Status: e.statusText, Header: header,
		Body: io.NopCloser(bytes.NewReader(e.body)), ContentLength: int64(len(e.body)), Request: e.request,
	}
}

func newDPoPSessionManager() *dpopSessionManager {
	return &dpopSessionManager{sessions: make(map[string]*dpopSessionEntry), now: time.Now}
}

func (m *dpopSessionManager) get(
	ctx context.Context,
	adapter *Adapter,
	credential account.Credential,
	ssoToken string,
	lease *infraegress.Lease,
) (dpopSession, string, error) {
	key := dpopSessionCacheKey(adapter.config().BaseURL, credential, ssoToken, lease)
	if session, ok := m.cached(key); ok {
		return session, key, nil
	}
	value, err, _ := m.loads.Do(key, func() (any, error) {
		if session, ok := m.cached(key); ok {
			return session, nil
		}
		session, fetchErr := adapter.fetchDPoPSession(ctx, ssoToken, lease)
		if fetchErr != nil {
			return dpopSession{}, fetchErr
		}
		m.store(key, session)
		return session, nil
	})
	if err != nil {
		return dpopSession{}, key, err
	}
	session, ok := value.(dpopSession)
	if !ok {
		return dpopSession{}, key, errors.New("Console DPoP session 类型无效")
	}
	return session, key, nil
}

func (m *dpopSessionManager) cached(key string) (dpopSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.sessions[key]
	if !ok {
		return dpopSession{}, false
	}
	if !entry.session.expiresAt.After(m.now().UTC().Add(dpopRefreshSkew)) {
		m.removeLocked(entry)
		return dpopSession{}, false
	}
	m.lru.MoveToFront(entry.element)
	return entry.session, true
}

func (m *dpopSessionManager) store(key string, session dpopSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry := m.sessions[key]; entry != nil {
		entry.session = session
		m.lru.MoveToFront(entry.element)
		return
	}
	if len(m.sessions) >= dpopSessionCacheLimit {
		if oldest := m.lru.Back(); oldest != nil {
			m.removeLocked(oldest.Value.(*dpopSessionEntry))
		}
	}
	entry := &dpopSessionEntry{key: key, session: session}
	entry.element = m.lru.PushFront(entry)
	m.sessions[key] = entry
}

func (m *dpopSessionManager) invalidate(key, accessToken string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.sessions[key]
	if current == nil || (accessToken != "" && current.session.accessToken != accessToken) {
		return
	}
	m.removeLocked(current)
	// Do not call singleflight.Forget here. A concurrent refresh must remain
	// coalesced; forgetting it can start a second token exchange for the same
	// account and egress after an expired token produces a burst of 401s.
}

func (m *dpopSessionManager) removeLocked(entry *dpopSessionEntry) {
	if entry == nil {
		return
	}
	delete(m.sessions, entry.key)
	if entry.element != nil {
		m.lru.Remove(entry.element)
		entry.element = nil
	}
}

func dpopSessionCacheKey(baseURL string, credential account.Credential, ssoToken string, lease *infraegress.Lease) string {
	nodeID := uint64(0)
	if lease != nil {
		nodeID = lease.NodeID
	}
	return strings.TrimRight(strings.TrimSpace(baseURL), "/") + "|" +
		strconv.FormatUint(credential.ID, 10) + "|" + strconv.FormatUint(nodeID, 10) + "|" + security.HashToken(ssoToken)
}

func (a *Adapter) fetchDPoPSession(ctx context.Context, ssoToken string, lease *infraegress.Lease) (dpopSession, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return dpopSession{}, fmt.Errorf("生成 Console DPoP 密钥: %w", err)
	}
	publicJWK := publicDPoPJWK(&privateKey.PublicKey)
	payload, err := json.Marshal(map[string]any{"jwk": publicJWK})
	if err != nil {
		return dpopSession{}, err
	}
	endpoint := consoleV1Endpoint(a.config().BaseURL, "/dpop/token")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return dpopSession{}, err
	}
	applyBrowserHeaders(request, ssoToken, lease)
	request.Header.Set("Content-Type", "application/json")
	localBefore := time.Now().UTC()
	response, err := lease.DoDeferredForbidden(request)
	if err != nil {
		return dpopSession{}, err
	}
	localAfter := time.Now().UTC()
	data, truncated, readErr := provider.ReadDiagnosticBody(response.Body)
	_ = response.Body.Close()
	if readErr != nil {
		return dpopSession{}, readErr
	}
	// Always materialize a skew value for this session. Missing/unparseable Date
	// yields 0 (no correction) — still a defined, usable value for every proof.
	clockSkew := dpopClockSkewFromDateHeader(response.Header.Get("Date"), localBefore, localAfter)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusForbidden {
			if shouldInvalidateConsoleClearance(data) {
				lease.InvalidateClearance()
			}
		}
		return dpopSession{}, &dpopTokenEndpointError{
			status: response.StatusCode, statusText: response.Status, header: response.Header.Clone(),
			body: data, bodyTruncated: truncated, request: response.Request,
		}
	}
	var tokenResponse struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &tokenResponse); err != nil {
		return dpopSession{}, fmt.Errorf("解析 Console DPoP token: %w", err)
	}
	if strings.TrimSpace(tokenResponse.AccessToken) == "" || !strings.EqualFold(strings.TrimSpace(tokenResponse.TokenType), "DPoP") {
		return dpopSession{}, errors.New("Console DPoP token 响应无效")
	}
	if tokenResponse.ExpiresIn <= 0 || time.Duration(tokenResponse.ExpiresIn)*time.Second > maxDPoPTokenLifetime {
		return dpopSession{}, errors.New("Console DPoP token 有效期无效")
	}
	thumbprint, err := dpopJWKThumbprint(publicJWK)
	if err != nil {
		return dpopSession{}, err
	}
	tokenExpiry, tokenThumbprint, err := parseDPoPAccessToken(tokenResponse.AccessToken)
	if err != nil {
		return dpopSession{}, err
	}
	if tokenThumbprint != thumbprint {
		return dpopSession{}, errors.New("Console DPoP token 与本地密钥不匹配")
	}
	// Expiry bookkeeping stays on the local monotonic wall clock so cache checks
	// remain self-consistent; proof iat alone is shifted by clockSkew.
	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(tokenResponse.ExpiresIn) * time.Second)
	if tokenExpiry.Before(expiresAt) {
		expiresAt = tokenExpiry
	}
	if !expiresAt.After(now.Add(dpopRefreshSkew)) {
		return dpopSession{}, errors.New("Console DPoP token 已过期或即将过期")
	}
	return dpopSession{
		accessToken: tokenResponse.AccessToken,
		privateKey:  privateKey,
		publicJWK:   publicJWK,
		expiresAt:   expiresAt,
		clockSkew:   clockSkew,
	}, nil
}

func (a *Adapter) doDPoPRequest(
	ctx context.Context,
	credential account.Credential,
	ssoToken string,
	lease *infraegress.Lease,
	method, endpoint string,
	body []byte,
	accept string,
) (*http.Response, error) {
	contentType := ""
	if len(body) > 0 {
		contentType = "application/json"
	}
	return a.doDPoPRequestWithContentType(ctx, credential, ssoToken, lease, method, endpoint, body, contentType, accept)
}

func (a *Adapter) doDPoPRequestWithContentType(
	ctx context.Context,
	credential account.Credential,
	ssoToken string,
	lease *infraegress.Lease,
	method, endpoint string,
	body []byte,
	contentType string,
	accept string,
) (*http.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		session, cacheKey, err := a.dpop.get(ctx, a, credential, ssoToken, lease)
		if err != nil {
			var endpointErr *dpopTokenEndpointError
			if errors.As(err, &endpointErr) {
				return endpointErr.response(), nil
			}
			return nil, err
		}
		request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		applyBrowserHeaders(request, ssoToken, lease)
		if len(body) > 0 {
			if strings.TrimSpace(contentType) == "" {
				contentType = "application/json"
			}
			request.Header.Set("Content-Type", contentType)
		}
		if strings.TrimSpace(accept) != "" {
			request.Header.Set("Accept", accept)
		}
		if strings.HasSuffix(request.URL.Path, "/responses") {
			request.Header.Set("x-cluster", "https://us-east-1.api.x.ai")
		}
		if err := applyDPoPAuthorization(request, session); err != nil {
			return nil, err
		}
		response, err := lease.DoDeferredForbidden(request)
		if err != nil {
			return nil, err
		}
		if response.StatusCode != http.StatusUnauthorized || attempt > 0 {
			return response, nil
		}
		_, _, _ = provider.ReadDiagnosticBody(response.Body)
		_ = response.Body.Close()
		a.dpop.invalidate(cacheKey, session.accessToken)
	}
	return nil, errors.New("Console DPoP 重试状态无效")
}

func publicDPoPJWK(key *ecdsa.PublicKey) dpopJWK {
	return dpopJWK{
		Kty: "EC", Crv: "P-256",
		X: base64.RawURLEncoding.EncodeToString(key.X.FillBytes(make([]byte, 32))),
		Y: base64.RawURLEncoding.EncodeToString(key.Y.FillBytes(make([]byte, 32))),
	}
}

func dpopJWKThumbprint(jwk dpopJWK) (string, error) {
	canonical := struct {
		Crv string `json:"crv"`
		Kty string `json:"kty"`
		X   string `json:"x"`
		Y   string `json:"y"`
	}{Crv: jwk.Crv, Kty: jwk.Kty, X: jwk.X, Y: jwk.Y}
	data, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

func parseDPoPAccessToken(value string) (time.Time, string, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return time.Time{}, "", errors.New("Console DPoP access token 格式无效")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, "", errors.New("Console DPoP access token payload 无效")
	}
	var claims struct {
		ExpiresAt int64 `json:"exp"`
		CNF       struct {
			JKT string `json:"jkt"`
		} `json:"cnf"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.ExpiresAt <= 0 || strings.TrimSpace(claims.CNF.JKT) == "" {
		return time.Time{}, "", errors.New("Console DPoP access token claims 无效")
	}
	return time.Unix(claims.ExpiresAt, 0).UTC(), claims.CNF.JKT, nil
}

func applyDPoPAuthorization(request *http.Request, session dpopSession) error {
	if request == nil || request.URL == nil || session.privateKey == nil || strings.TrimSpace(session.accessToken) == "" {
		return errors.New("Console DPoP 请求参数无效")
	}
	digest := sha256.Sum256([]byte(session.accessToken))
	claims := jwt.MapClaims{
		"jti": uuid.NewString(),
		"htm": strings.ToUpper(request.Method),
		"htu": dpopHTU(request),
		"iat": dpopProofIAT(session, time.Now().UTC()),
		"ath": base64.RawURLEncoding.EncodeToString(digest[:]),
	}
	proof := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	proof.Header["typ"] = "dpop+jwt"
	proof.Header["jwk"] = session.publicJWK
	signed, err := proof.SignedString(session.privateKey)
	if err != nil {
		return fmt.Errorf("签名 Console DPoP proof: %w", err)
	}
	request.Header.Set("Authorization", "DPoP "+session.accessToken)
	request.Header.Set("DPoP", signed)
	return nil
}

// dpopProofIAT returns the Unix seconds used for DPoP proof iat.
// session.clockSkew is always defined on minted sessions (0 if Date was absent).
func dpopProofIAT(session dpopSession, localNow time.Time) int64 {
	if localNow.IsZero() {
		localNow = time.Now().UTC()
	}
	return localNow.Add(session.clockSkew).UTC().Unix()
}

// dpopClockSkewFromDateHeader mirrors console.x.ai frontend behavior:
// skewSeconds ≈ round((serverDate − localNow) / 1s), applied later as
// iat = floor((localNow + skew) / 1s). localBefore/localAfter bound the RTT
// around the mint response so we estimate local time at header receipt without
// an extra time API call. Returns 0 when Date is missing or unparseable so
// callers always receive a usable skew value.
func dpopClockSkewFromDateHeader(dateHeader string, localBefore, localAfter time.Time) time.Duration {
	dateHeader = strings.TrimSpace(dateHeader)
	if dateHeader == "" {
		return 0
	}
	serverTime, err := http.ParseTime(dateHeader)
	if err != nil {
		return 0
	}
	if localAfter.IsZero() || localAfter.Before(localBefore) {
		localAfter = localBefore
	}
	if localBefore.IsZero() {
		localBefore = time.Now().UTC()
		localAfter = localBefore
	}
	// Mid-point of the local observation window approximates "now" when Date was set.
	localMid := localBefore.Add(localAfter.Sub(localBefore) / 2)
	// Store whole seconds because DPoP iat has second precision. Duration.Round
	// makes the half-away-from-zero behavior explicit for both skew directions.
	return serverTime.UTC().Sub(localMid.UTC()).Round(time.Second)
}

func dpopHTU(request *http.Request) string {
	path := request.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	return request.URL.Scheme + "://" + request.URL.Host + path
}

func consoleV1Endpoint(baseURL, path string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	path = "/" + strings.TrimLeft(strings.TrimSpace(path), "/")
	if strings.HasSuffix(baseURL, "/v1") {
		return baseURL + path
	}
	return baseURL + "/v1" + path
}
