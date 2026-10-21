package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"google.golang.org/protobuf/encoding/protowire"
)

type recordingCacheStore struct {
	mu       sync.Mutex
	apiKeys  map[uint64]string
	sessions map[uint64]string
}

func newRecordingCacheStore() *recordingCacheStore {
	return &recordingCacheStore{apiKeys: map[uint64]string{}, sessions: map[uint64]string{}}
}

func (s *recordingCacheStore) UpdateConsoleAPIKeyID(_ context.Context, accountID uint64, apiKeyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.apiKeys[accountID] = apiKeyID
	return nil
}

func (s *recordingCacheStore) UpdateConsoleWorkOSSession(_ context.Context, accountID uint64, encrypted string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[accountID] = encrypted
	return nil
}

func (s *recordingCacheStore) apiKey(accountID uint64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.apiKeys[accountID]
}

func (s *recordingCacheStore) session(accountID uint64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[accountID]
}

// consoleHomeHTML mimics the Next.js flight payload, which embeds the key
// objects as escaped JSON string literals.
func consoleHomeHTML(teamID string, keys ...string) string {
	var builder strings.Builder
	builder.WriteString(`<html><a href="/team/` + teamID + `/chat"></a><script>self.__next_f.push([1,"`)
	for _, key := range keys {
		builder.WriteString(key)
	}
	builder.WriteString(`"])</script></html>`)
	return builder.String()
}

func consoleHomeKey(teamID, apiKeyID string, disabled bool) string {
	state := "false"
	if disabled {
		state = "true"
	}
	return `{\"name\":\"xAI Chat Playground\",\"teamId\":\"` + teamID + `\",\"apiKeyId\":\"` + apiKeyID + `\",\"apiKey\":\"\",\"disabled\":` + state + `,\"aclStrings\":[\"api-key:endpoint:*\",\"api-key:model:*\"]}`
}

const (
	testTeamID   = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	testNewKeyID = "99999999-8888-4777-8666-555555555555"
)

func TestParseConsoleHomeSkipsDisabledKeysAndReadsTeam(t *testing.T) {
	t.Parallel()
	disabledKey := "00000000-0000-4000-8000-000000000001"
	html := consoleHomeHTML(testTeamID, consoleHomeKey(testTeamID, disabledKey, true), ",", consoleHomeKey(testTeamID, testNewKeyID, false))
	home := parseConsoleHome([]byte(html))
	if home.apiKeyID != testNewKeyID || home.teamID != testTeamID {
		t.Fatalf("home = %#v", home)
	}
	empty := parseConsoleHome([]byte(consoleHomeHTML(testTeamID)))
	if empty.apiKeyID != "" || empty.teamID != testTeamID {
		t.Fatalf("empty home = %#v", empty)
	}
}

func TestConsoleCreateAPIKeyBodyUsesConnectEnvelope(t *testing.T) {
	t.Parallel()
	body := consoleCreateAPIKeyBody(testTeamID)
	if body[0] != 0 {
		t.Fatalf("flag = %d", body[0])
	}
	length := int(body[1])<<24 | int(body[2])<<16 | int(body[3])<<8 | int(body[4])
	payload := body[5:]
	if length != len(payload) {
		t.Fatalf("length = %d, payload = %d", length, len(payload))
	}
	fields := map[protowire.Number][]string{}
	for len(payload) > 0 {
		number, kind, n := protowire.ConsumeTag(payload)
		if n < 0 || kind != protowire.BytesType {
			t.Fatalf("bad tag")
		}
		payload = payload[n:]
		value, m := protowire.ConsumeString(payload)
		if m < 0 {
			t.Fatalf("bad string")
		}
		payload = payload[m:]
		fields[number] = append(fields[number], value)
	}
	if fields[1][0] != consoleAPIKeyName || fields[2][0] != testTeamID || strings.Join(fields[3], ",") != "api-key:endpoint:*,api-key:model:*" {
		t.Fatalf("fields = %#v", fields)
	}
}

// TestConsoleAPIKeyReadThenCreate covers the idempotent sequence: a team
// without a key gets exactly one CreateApiKey, the id is confirmed by a
// re-read, injected as x-apikey, persisted, and reused without another read.
func TestConsoleAPIKeyReadThenCreate(t *testing.T) {
	var created atomic.Bool
	var homeReads, createCalls, responseCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/v1/dpop/token":
			serveTestDPoPToken(t, writer, request)
		case request.URL.Path == "/" && request.Method == http.MethodGet:
			homeReads.Add(1)
			if request.Header.Get(consoleAPIKeyHeader) != "" {
				t.Errorf("home read must not send x-apikey")
			}
			if created.Load() {
				_, _ = io.WriteString(writer, consoleHomeHTML(testTeamID, consoleHomeKey(testTeamID, testNewKeyID, false)))
				return
			}
			_, _ = io.WriteString(writer, consoleHomeHTML(testTeamID))
		case request.URL.Path == consoleCreateAPIKeyPath:
			createCalls.Add(1)
			verifyTestDPoPProof(t, request)
			if request.Header.Get("Content-Type") != "application/connect+proto" || request.Header.Get(consoleAPIKeyHeader) != "" {
				t.Errorf("create headers = %#v", request.Header)
			}
			if request.Header.Get("Referer") != "http://"+request.Host+"/team/"+testTeamID+"/chat" {
				t.Errorf("create referer = %q", request.Header.Get("Referer"))
			}
			body, _ := io.ReadAll(request.Body)
			if string(body) != string(consoleCreateAPIKeyBody(testTeamID)) {
				t.Errorf("create body mismatch")
			}
			created.Store(true)
			writer.Header().Set("Content-Type", "application/connect+proto")
			_, _ = writer.Write([]byte{0, 0, 0, 0, 0})
		case request.URL.Path == "/v1/responses":
			responseCalls.Add(1)
			if got := request.Header.Get(consoleAPIKeyHeader); got != testNewKeyID {
				t.Errorf("x-apikey = %q", got)
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"id":"resp_1","object":"response","status":"completed","output":[]}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	credential.ConsoleAPIKeyID = ""
	store := newRecordingCacheStore()
	adapter.SetCredentialCacheStore(store)
	for range 2 {
		response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
			Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "grok-4.3",
			Operation: conversation.OperationResponses, Body: []byte(`{"model":"grok-4.3","input":"hi"}`), NormalizeBody: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", response.StatusCode)
		}
	}
	if createCalls.Load() != 1 || homeReads.Load() != 2 || responseCalls.Load() != 2 {
		t.Fatalf("create=%d home=%d responses=%d", createCalls.Load(), homeReads.Load(), responseCalls.Load())
	}
	if store.apiKey(credential.ID) != testNewKeyID {
		t.Fatalf("stored key = %q", store.apiKey(credential.ID))
	}
}

// TestConsoleAPIKeyRepairsRejectedCachedKey verifies that the repairable 429
// ("provision an API key") re-reads the home page once and retries with the
// fresh key instead of surfacing a rate limit.
func TestConsoleAPIKeyRepairsRejectedCachedKey(t *testing.T) {
	var responseCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/dpop/token":
			serveTestDPoPToken(t, writer, request)
		case "/":
			_, _ = io.WriteString(writer, consoleHomeHTML(testTeamID, consoleHomeKey(testTeamID, testNewKeyID, false)))
		case "/v1/responses":
			responseCalls.Add(1)
			if request.Header.Get(consoleAPIKeyHeader) != testNewKeyID {
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(writer, `{"code":"Some resource has been exhausted","error":"Free usage quota exceeded. Purchase credits or provision an API key"}`)
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"id":"resp_1","object":"response","status":"completed","output":[]}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	store := newRecordingCacheStore()
	adapter.SetCredentialCacheStore(store)
	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "grok-4.3",
		Operation: conversation.OperationResponses, Body: []byte(`{"model":"grok-4.3","input":"hi"}`), NormalizeBody: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || responseCalls.Load() != 2 || store.apiKey(credential.ID) != testNewKeyID {
		t.Fatalf("status=%d calls=%d stored=%q", response.StatusCode, responseCalls.Load(), store.apiKey(credential.ID))
	}
}

func TestConsoleHTMLUnauthorizedIsTransient(t *testing.T) {
	var responseCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/dpop/token":
			serveTestDPoPToken(t, writer, request)
		case "/v1/responses":
			responseCalls.Add(1)
			writer.Header().Set("Content-Type", "text/html")
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(writer, `<html><head><title>401 Authorization Required</title></head></html>`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{
		Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "grok-4.3",
		Body: []byte(`{"model":"grok-4.3","input":"hi"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("X-Grok2API-Upstream-Status") != "401" || responseCalls.Load() != 2 {
		t.Fatalf("status=%d header=%q calls=%d", response.StatusCode, response.Header.Get("X-Grok2API-Upstream-Status"), responseCalls.Load())
	}
}

type dashboardStub struct {
	t             *testing.T
	logins        atomic.Int32
	usageCalls    atomic.Int32
	rejectSession string
	usageBody     string
	unlinked      bool
	expired       bool
	cursorMerged  string
}

func (s *dashboardStub) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case workosLoginPath:
		s.logins.Add(1)
		if cookie, err := request.Cookie("sso"); err != nil || cookie.Value != "test-sso" {
			s.t.Errorf("login sso cookie = %v", err)
		}
		if s.unlinked {
			http.Redirect(writer, request, "https://authenticator.cursor.sh/?authorization_session_id=x", http.StatusFound)
			return
		}
		if s.expired {
			_, _ = io.WriteString(writer, "<html>sign in</html>")
			return
		}
		http.SetCookie(writer, &http.Cookie{Name: "WorkosCursorAuthNonce", Value: "nonce-1", Path: "/"})
		http.Redirect(writer, request, "/dashboard/api/auth/callback?code=abc", http.StatusFound)
	case "/dashboard/api/auth/callback":
		if cookie, err := request.Cookie("WorkosCursorAuthNonce"); err != nil || cookie.Value != "nonce-1" {
			s.t.Errorf("callback nonce = %v", err)
		}
		suffix := s.logins.Load()
		for _, cookie := range []*http.Cookie{
			{Name: workosCookieSessionToken, Value: "session-" + string(rune('0'+suffix)), Path: "/", HttpOnly: true},
			{Name: workosCookieXAIOrigin, Value: "origin", Path: "/", HttpOnly: true},
			{Name: workosCookieID, Value: "user_1", Path: "/", HttpOnly: true},
		} {
			http.SetCookie(writer, cookie)
		}
		http.Redirect(writer, request, "/dashboard/usage", http.StatusFound)
	case "/dashboard/api/dashboard/v2/usage-status":
		s.usageCalls.Add(1)
		if request.URL.Query().Get("scope") != "user" || request.Header.Get("x-cursor-dashboard-surface") != "v2" {
			s.t.Errorf("usage request = %s %#v", request.URL, request.Header)
		}
		token, err := request.Cookie(workosCookieSessionToken)
		_, originErr := request.Cookie(workosCookieXAIOrigin)
		_, idErr := request.Cookie(workosCookieID)
		if err != nil || originErr != nil || idErr != nil || token.Value == s.rejectSession {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(writer, `{"error":"not_authenticated"}`)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, s.usageBody)
	case "/api/auth/session":
		writer.Header().Set("Content-Type", "application/json")
		if s.expired {
			_, _ = io.WriteString(writer, `{"status":"unauthenticated"}`)
			return
		}
		_, _ = io.WriteString(writer, `{"status":"authenticated","session":{"userId":"u1"`+s.cursorMerged+`}}`)
	default:
		http.NotFound(writer, request)
	}
}

func newDashboardTestAdapter(t *testing.T, stub *dashboardStub) (*Adapter, *recordingCacheStore, func()) {
	t.Helper()
	server := httptest.NewServer(stub)
	adapter, _ := newConsoleTestAdapter(t, server.URL)
	adapter.UpdateConfig(Config{BaseURL: server.URL, SessionBaseURL: server.URL, TimeoutSeconds: 5})
	store := newRecordingCacheStore()
	adapter.SetCredentialCacheStore(store)
	return adapter, store, server.Close
}

func TestSyncQuotaReadsWeeklyPoolAndCachesWorkOSSession(t *testing.T) {
	stub := &dashboardStub{t: t, usageBody: `{"weeklyIncludedUsage":{"budgetCents":100,"usedCents":"37","resetsAt":"2026-10-27T08:15:30Z","period":"INCLUDED_USAGE_PERIOD_WEEKLY"}}`}
	adapter, store, closeServer := newDashboardTestAdapter(t, stub)
	defer closeServer()
	_, credential := newConsoleTestAdapter(t, "http://unused")
	snapshot, err := adapter.SyncQuota(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	wantReset := time.Date(2026, 10, 27, 8, 15, 30, 0, time.UTC)
	if len(snapshot.Windows) != 3 {
		t.Fatalf("windows = %#v", snapshot.Windows)
	}
	for index, mode := range []string{QuotaMode, QuotaModeImage, QuotaModeVideo} {
		window := snapshot.Windows[index]
		if window.Mode != mode || window.Total != 100 || window.Remaining != 63 || window.UsagePercent != 37 ||
			window.ResetAt == nil || !window.ResetAt.Equal(wantReset) || window.WindowSeconds != 7*24*3600 || window.Source != "upstream" {
			t.Fatalf("window[%d] = %#v", index, window)
		}
	}
	encrypted := store.session(credential.ID)
	if encrypted == "" {
		t.Fatal("WorkOS session was not persisted")
	}
	plain, err := adapter.cipher.Decrypt(encrypted)
	if err != nil || !strings.Contains(plain, "session-1") {
		t.Fatalf("stored session = %q, %v", plain, err)
	}
	// A cached session skips the login exchange entirely.
	credential.EncryptedConsoleWorkOSSession = encrypted
	if _, err := adapter.SyncQuota(context.Background(), credential); err != nil {
		t.Fatal(err)
	}
	if stub.logins.Load() != 1 || stub.usageCalls.Load() != 2 {
		t.Fatalf("logins=%d usage=%d", stub.logins.Load(), stub.usageCalls.Load())
	}
	// A rejected cached session is replaced by exactly one fresh login.
	stub.rejectSession = "session-1"
	if _, err := adapter.SyncQuota(context.Background(), credential); err != nil {
		t.Fatal(err)
	}
	refreshed, _ := adapter.cipher.Decrypt(store.session(credential.ID))
	if stub.logins.Load() != 2 || !strings.Contains(refreshed, "session-2") {
		t.Fatalf("logins=%d session=%q", stub.logins.Load(), refreshed)
	}
}

func TestSyncQuotaClampsOverspentBudget(t *testing.T) {
	stub := &dashboardStub{t: t, usageBody: `{"weeklyIncludedUsage":{"budgetCents":"100","usedCents":563}}`}
	adapter, _, closeServer := newDashboardTestAdapter(t, stub)
	defer closeServer()
	_, credential := newConsoleTestAdapter(t, "http://unused")
	window, err := adapter.SyncQuotaMode(context.Background(), credential, QuotaModeVideo)
	if err != nil {
		t.Fatal(err)
	}
	if window.Remaining != 0 || window.Total != 100 || window.UsagePercent != 100 || window.ResetAt != nil {
		t.Fatalf("window = %#v", window)
	}
}

func TestSyncQuotaUnlinkedAccountIsUnauthorized(t *testing.T) {
	stub := &dashboardStub{t: t, unlinked: true, cursorMerged: `,"cursorMerged":false`}
	adapter, _, closeServer := newDashboardTestAdapter(t, stub)
	defer closeServer()
	_, credential := newConsoleTestAdapter(t, "http://unused")
	_, err := adapter.SyncQuota(context.Background(), credential)
	if !errors.Is(err, provider.ErrUnauthorized) || !strings.Contains(err.Error(), "cursorMerged=false") {
		t.Fatalf("err = %v", err)
	}
	// Pre-unification accounts have no cursorMerged field at all.
	stub.cursorMerged = ""
	if _, err := adapter.SyncQuota(context.Background(), credential); !errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("missing field err = %v", err)
	}
	// A merged account landing on the sign-in page is a transient fault.
	stub.cursorMerged = `,"cursorMerged":true`
	if _, err := adapter.SyncQuota(context.Background(), credential); err == nil || errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("merged err = %v", err)
	}
}

func TestParseConsoleUsageStatusRejectsMissingPool(t *testing.T) {
	t.Parallel()
	if _, err := parseConsoleUsageStatus([]byte(`{"other":{}}`)); err == nil {
		t.Fatal("missing weeklyIncludedUsage accepted")
	}
	status, err := parseConsoleUsageStatus([]byte(`{"weeklyIncludedUsage":{"budgetCents":100,"resetsAt":{"seconds":"1793088000"}}}`))
	if err != nil || status.UsedCents != 0 || status.ResetsAt == nil || status.ResetsAt.Unix() != 1793088000 {
		t.Fatalf("status = %#v, %v", status, err)
	}
	merged, known, err := parseCursorMerged([]byte(`{"session":{"cursorMerged":true}}`))
	if err != nil || !merged || !known {
		t.Fatalf("merged = %v known = %v err = %v", merged, known, err)
	}
	var session workosSession
	if json.Unmarshal([]byte(`{"session_token":"a","xai_origin":"b"}`), &session) != nil || session.complete() {
		t.Fatal("incomplete session accepted")
	}
}

func TestIsAllowedWorkOSHost(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]bool{
		"https://grok.com/dashboard":                true,
		"https://cursor.com/unify/upgrade":          true,
		"https://api.workos.com/user_management":    true,
		"https://authenticator.cursor.sh/bootstrap": true,
		"http://cursor.com/unify":                   false,
		"https://evil.example.com/dashboard":        false,
		"https://grok.com.evil.example/dashboard":   false,
		"https://notcursor.com/unify":               false,
	} {
		target, _ := url.Parse(raw)
		if got := isAllowedWorkOSHost(target, "grok.com"); got != want {
			t.Fatalf("isAllowedWorkOSHost(%s) = %v, want %v", raw, got, want)
		}
	}
}

func TestNormalizeRequestStripsSearchToolsForGrok45(t *testing.T) {
	t.Parallel()
	spec, _ := Resolve("grok-4.5")
	body, err := normalizeRequest([]byte(`{"input":"hi","tools":[{"type":"web_search"},{"type":"x_search"}],"tool_choice":"auto"}`), spec)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	if _, ok := payload["tools"]; ok {
		t.Fatalf("tools kept: %s", body)
	}
	if _, ok := payload["tool_choice"]; ok {
		t.Fatalf("tool_choice kept: %s", body)
	}
	body, err = normalizeRequest([]byte(`{"input":"hi","tools":[{"type":"web_search"},{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":"auto"}`), spec)
	if err != nil {
		t.Fatal(err)
	}
	payload = nil
	_ = json.Unmarshal(body, &payload)
	tools, _ := payload["tools"].([]any)
	if len(tools) != 1 || payload["tool_choice"] != "auto" {
		t.Fatalf("function tool payload = %s", body)
	}
	other, _ := Resolve("grok-4.20-multi-agent-0309")
	body, _ = normalizeRequest([]byte(`{"input":"hi","tools":[{"type":"web_search"}]}`), other)
	if !strings.Contains(string(body), "web_search") {
		t.Fatalf("multi-agent lost web_search: %s", body)
	}
}

// TestParseConsoleUsageStatusLiveShape pins the usage-status payload captured
// from a live free account on 2026-10-20 (ids omitted).
func TestParseConsoleUsageStatusLiveShape(t *testing.T) {
	t.Parallel()
	body := `{"availability":"available","monthlyUsage":{"usedCents":0,"limitCents":null,"creditsUsedCents":0,"periodStart":"2026-10-01T00:00:00.000Z","resetsAt":"2026-11-01T00:00:00.000Z"},"weeklyIncludedStatus":{"status":"metered"},"weeklyIncludedUsage":{"usedCents":0,"budgetCents":100,"resetCounter":2,"periodStart":"2026-10-21T03:02:43.807Z","resetsAt":"2026-10-28T03:02:43.807Z"}}`
	status, err := parseConsoleUsageStatus([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 10, 28, 3, 2, 43, 807_000_000, time.UTC)
	if status.BudgetCents != 100 || status.UsedCents != 0 || status.ResetsAt == nil || !status.ResetsAt.Equal(want) {
		t.Fatalf("status = %#v", status)
	}
}

// TestParseConsoleHomeLiveEmptyKeyList mirrors a fresh team page: the key list
// is serialized as ListApiKeysResponse with an empty apiKeys array.
func TestParseConsoleHomeLiveEmptyKeyList(t *testing.T) {
	t.Parallel()
	html := `self.__next_f.push([1,"{\"state\":{\"data\":{\"$typeName\":\"auth_mgmt.ListApiKeysResponse\",\"apiKeys\":[]}},\"team\":{\"$typeName\":\"prod_auth.Team\",\"teamId\":\"` + testTeamID + `\",\"aclStrings\":[\"team:flag:cursor-legacy-cleared\"]}}"])`
	home := parseConsoleHome([]byte(html))
	if home.apiKeyID != "" || home.teamID != testTeamID {
		t.Fatalf("home = %#v", home)
	}
}

// TestReadConsoleHomeFollowsTeamRedirect covers the live 307 / → /team/<id>.
func TestReadConsoleHomeFollowsTeamRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/":
			http.SetCookie(writer, &http.Cookie{Name: "console_hint", Value: "1", Path: "/"})
			http.Redirect(writer, request, "/team/"+testTeamID, http.StatusTemporaryRedirect)
		case "/team/" + testTeamID:
			if cookie, err := request.Cookie("console_hint"); err != nil || cookie.Value != "1" {
				t.Errorf("redirect cookie not carried: %v", err)
			}
			_, _ = io.WriteString(writer, consoleHomeHTML("00000000-0000-4000-8000-000000000000", consoleHomeKey(testTeamID, testNewKeyID, false)))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	lease, err := adapter.egress.AcquireCredential(context.Background(), "grok_console", credential)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	home, err := adapter.readConsoleHome(context.Background(), "test-sso", lease)
	if err != nil {
		t.Fatal(err)
	}
	if home.apiKeyID != testNewKeyID || home.teamID != testTeamID {
		t.Fatalf("home = %#v", home)
	}
}

// TestSyncQuotaExpiredSSOIsUnauthorized mirrors a live expired SSO: the login
// route answers 200 without WorkOS cookies and the session is unauthenticated.
func TestSyncQuotaExpiredSSOIsUnauthorized(t *testing.T) {
	stub := &dashboardStub{t: t, expired: true}
	adapter, _, closeServer := newDashboardTestAdapter(t, stub)
	defer closeServer()
	_, credential := newConsoleTestAdapter(t, "http://unused")
	_, err := adapter.SyncQuota(context.Background(), credential)
	if !errors.Is(err, provider.ErrUnauthorized) || errors.Is(err, provider.ErrConsoleUnlinked) {
		t.Fatalf("err = %v", err)
	}
}

func TestConsoleImportSeparatedLineFormat(t *testing.T) {
	t.Parallel()
	const password = "Secr3t-Passw0rd"
	full := "a@example.com----" + password + "----sso-full----wos-token----wos-origin----user_01----" + strings.ToUpper(testNewKeyID)
	minimal := "b@example.com----" + password + "----sso-min"
	badCaches := "c@example.com----" + password + "----sso-bad----wos-token--------user_01----not-a-uuid"
	seeds, err := parseImportedCredentials([]byte(full + "\n" + minimal + "\n\n" + badCaches + "\n" + full))
	if err != nil {
		t.Fatal(err)
	}
	if len(seeds) != 3 {
		t.Fatalf("seeds = %d, want 3 (duplicate SSO skipped)", len(seeds))
	}
	first := seeds[0]
	if first.AccessToken != "sso-full" || first.Email != "a@example.com" || first.Name != "a@example.com" || first.ConsoleAPIKeyID != testNewKeyID {
		t.Fatalf("full seed = %#v", first)
	}
	var session workosSession
	if err := json.Unmarshal([]byte(first.ConsoleWorkOSSession), &session); err != nil || session != (workosSession{SessionToken: "wos-token", XAIOrigin: "wos-origin", WorkOSID: "user_01"}) {
		t.Fatalf("workos session = %q, %v", first.ConsoleWorkOSSession, err)
	}
	if seeds[1].AccessToken != "sso-min" || seeds[1].ConsoleAPIKeyID != "" || seeds[1].ConsoleWorkOSSession != "" {
		t.Fatalf("minimal seed = %#v", seeds[1])
	}
	if seeds[2].ConsoleAPIKeyID != "" || seeds[2].ConsoleWorkOSSession != "" {
		t.Fatalf("malformed caches kept: %#v", seeds[2])
	}
	for _, seed := range seeds {
		if strings.Contains(fmt.Sprintf("%#v", seed), password) {
			t.Fatal("password leaked into a credential seed")
		}
	}
	if _, err := parseImportedCredentials([]byte("a@example.com----only-two")); err == nil {
		t.Fatal("line without sso accepted")
	}
}

func TestConsoleImportJSONCaches(t *testing.T) {
	t.Parallel()
	seeds, err := parseImportedCredentials([]byte(`{"provider":"grok_console","accounts":[{"sso_token":"sso-json","x_apikey":"` + testNewKeyID + `","workos_session_token":"t","workos_xai_origin":"o","workos_id":"i"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(seeds) != 1 || seeds[0].ConsoleAPIKeyID != testNewKeyID || seeds[0].ConsoleWorkOSSession == "" {
		t.Fatalf("seeds = %#v", seeds)
	}
}

func TestCloudflareRejectionNamesThePage(t *testing.T) {
	t.Parallel()
	if got := cloudflareRejection([]byte(`<!DOCTYPE html><html><head><title>Attention Required! | Cloudflare</title>`)); !strings.Contains(got, "Attention Required") {
		t.Fatalf("block page = %q", got)
	}
	if got := cloudflareRejection([]byte(`<title>Just a moment...</title>`)); !strings.Contains(got, "Just a moment") {
		t.Fatalf("challenge page = %q", got)
	}
	if cloudflareRejection([]byte(`{"error":"forbidden"}`)) != "" {
		t.Fatal("JSON 403 misclassified as Cloudflare")
	}
}

// TestSyncQuotaCloudflareBlockIsNotAccountFailure guards against retiring or
// deleting healthy accounts while the egress IP is blocked by Cloudflare.
func TestSyncQuotaCloudflareBlockIsNotAccountFailure(t *testing.T) {
	block := `<!DOCTYPE html><html><head><title>Attention Required! | Cloudflare</title></head></html>`
	var requests atomic.Int32
	blocked := atomic.Bool{}
	blocked.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if !blocked.Load() && request.URL.Path == "/dashboard/api/dashboard/v2/usage-status" {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"weeklyIncludedUsage":{"budgetCents":100,"usedCents":1}}`)
			return
		}
		writer.Header().Set("Content-Type", "text/html")
		writer.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(writer, block)
	}))
	defer server.Close()
	adapter, credential := newConsoleTestAdapter(t, server.URL)
	adapter.UpdateConfig(Config{BaseURL: server.URL, SessionBaseURL: server.URL, TimeoutSeconds: 5})
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	adapter.breaker.now = func() time.Time { return now }
	encrypted, _ := adapter.cipher.Encrypt(`{"session_token":"t","xai_origin":"o","workos_id":"i"}`)
	credential.EncryptedConsoleWorkOSSession = encrypted

	_, err := adapter.SyncQuota(context.Background(), credential)
	if !errors.Is(err, provider.ErrEgressBlocked) || errors.Is(err, provider.ErrUnauthorized) || errors.Is(err, provider.ErrConsoleUnlinked) {
		t.Fatalf("first err = %v", err)
	}
	if provider.ErrorRetryAfter(err) != egressBreakerBase {
		t.Fatalf("retry after = %s", provider.ErrorRetryAfter(err))
	}
	// While the breaker is open, other accounts on the same egress make no
	// upstream request at all.
	before := requests.Load()
	credential.EncryptedConsoleWorkOSSession = ""
	for range 5 {
		if _, err := adapter.SyncQuota(context.Background(), credential); !errors.Is(err, provider.ErrEgressBlocked) {
			t.Fatalf("gated err = %v", err)
		}
	}
	if requests.Load() != before {
		t.Fatalf("breaker leaked %d upstream requests", requests.Load()-before)
	}
	// After the cool-down a still-blocked exit doubles the pause.
	now = now.Add(egressBreakerBase + time.Second)
	credential.EncryptedConsoleWorkOSSession = encrypted
	_, err = adapter.SyncQuota(context.Background(), credential)
	if provider.ErrorRetryAfter(err) != 2*egressBreakerBase {
		t.Fatalf("second trip retry after = %s (%v)", provider.ErrorRetryAfter(err), err)
	}
	// A recovered exit closes the breaker on the first success.
	blocked.Store(false)
	now = now.Add(2*egressBreakerBase + time.Second)
	if _, err := adapter.SyncQuota(context.Background(), credential); err != nil {
		t.Fatalf("recovered err = %v", err)
	}
	if adapter.breaker.blocked(0) != 0 {
		t.Fatal("breaker not reset after success")
	}
}

func TestEgressBreakerCapsBackoff(t *testing.T) {
	t.Parallel()
	breaker := newEgressBreaker()
	now := time.Now()
	breaker.now = func() time.Time { return now }
	var last time.Duration
	for range 10 {
		delay, opened := breaker.trip(3)
		if !opened {
			t.Fatal("expired breaker did not reopen")
		}
		last = delay
		now = now.Add(delay + time.Second)
	}
	if last != egressBreakerMax {
		t.Fatalf("max delay = %s", last)
	}
	if breaker.blocked(4) != 0 {
		t.Fatal("breakers must be per egress node")
	}
}
