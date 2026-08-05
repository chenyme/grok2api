package console

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDPoPClockSkewFromDateHeaderAlwaysUsable(t *testing.T) {
	local := time.Date(2026, 8, 5, 17, 0, 0, 0, time.UTC)

	if got := dpopClockSkewFromDateHeader("", local, local); got != 0 {
		t.Fatalf("empty Date skew = %v, want 0", got)
	}
	if got := dpopClockSkewFromDateHeader("not-a-date", local, local); got != 0 {
		t.Fatalf("bad Date skew = %v, want 0", got)
	}

	server := local.Add(-80 * time.Second)
	got := dpopClockSkewFromDateHeader(server.Format(http.TimeFormat), local, local)
	if got != -80*time.Second {
		t.Fatalf("skew = %v, want -80s", got)
	}

	server = local.Add(90 * time.Second)
	got = dpopClockSkewFromDateHeader(server.Format(http.TimeFormat), local, local)
	if got != 90*time.Second {
		t.Fatalf("skew = %v, want 90s", got)
	}
}

func TestDPoPProofIATAppliesSessionClockSkew(t *testing.T) {
	local := time.Date(2026, 8, 5, 17, 0, 0, 0, time.UTC)
	session := dpopSession{clockSkew: -80 * time.Second}
	if got := dpopProofIAT(session, local); got != local.Add(-80*time.Second).Unix() {
		t.Fatalf("iat = %d, want skewed", got)
	}
	if got := dpopProofIAT(dpopSession{}, local); got != local.Unix() {
		t.Fatalf("zero skew iat = %d, want %d", got, local.Unix())
	}
}

func TestFetchDPoPSessionStoresClockSkewFromDateHeader(t *testing.T) {
	var sawIAT int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/v1/dpop/token":
			// Keep skew inside verifyTestDPoPProof's ±1m iat window.
			serverNow := time.Now().UTC().Add(-30 * time.Second)
			writer.Header().Set("Date", serverNow.Format(http.TimeFormat))
			if !serveTestDPoPToken(t, writer, request) {
				t.Fatalf("expected dpop token path")
			}
		case request.URL.Path == "/v1/usage":
			proof := strings.TrimSpace(request.Header.Get("DPoP"))
			parts := strings.Split(proof, ".")
			if len(parts) != 3 {
				t.Fatalf("bad DPoP proof")
			}
			payload, err := base64.RawURLEncoding.DecodeString(parts[1])
			if err != nil {
				t.Fatalf("decode proof payload: %v", err)
			}
			var claims map[string]any
			if err := json.Unmarshal(payload, &claims); err != nil {
				t.Fatalf("unmarshal proof claims: %v", err)
			}
			switch v := claims["iat"].(type) {
			case float64:
				sawIAT = int64(v)
			case json.Number:
				sawIAT, _ = v.Int64()
			default:
				t.Fatalf("iat type = %T", claims["iat"])
			}
			if request.Header.Get("Authorization") == "" || proof == "" {
				t.Fatalf("missing DPoP auth headers")
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"quotas":[{"kind":"chat","limit":10,"used":0,"remaining":10},{"kind":"image","limit":5,"used":0,"remaining":5},{"kind":"video","limit":2,"used":0,"remaining":2}]}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	adapter, credential := newConsoleTestAdapter(t, server.URL)
	if _, err := adapter.SyncQuota(t.Context(), credential); err != nil {
		t.Fatalf("SyncQuota: %v", err)
	}

	var sawSkew time.Duration
	adapter.dpop.mu.Lock()
	for _, entry := range adapter.dpop.sessions {
		sawSkew = entry.session.clockSkew
		break
	}
	adapter.dpop.mu.Unlock()
	if sawSkew > -20*time.Second || sawSkew < -40*time.Second {
		t.Fatalf("session clockSkew = %v, want about -30s", sawSkew)
	}
	if sawIAT == 0 {
		t.Fatal("did not capture iat")
	}
	localNow := time.Now().UTC().Unix()
	if delta := localNow - sawIAT; delta < 15 || delta > 45 {
		t.Fatalf("iat skew vs local: local=%d iat=%d delta=%d, want ~30s", localNow, sawIAT, delta)
	}
}

func TestApplyDPoPAuthorizationUsesClockSkewInIAT(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	access := "test-access-token"
	session := dpopSession{
		accessToken: access,
		privateKey:  privateKey,
		publicJWK:   publicDPoPJWK(&privateKey.PublicKey),
		clockSkew:   -80 * time.Second,
	}
	request := httptest.NewRequest(http.MethodGet, "https://console.x.ai/v1/usage", nil)
	if err := applyDPoPAuthorization(request, session); err != nil {
		t.Fatal(err)
	}
	proof := request.Header.Get("DPoP")
	parts := strings.Split(proof, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	_ = json.Unmarshal(payload, &claims)
	iat := int64(claims["iat"].(float64))
	want := time.Now().UTC().Add(-80 * time.Second).Unix()
	if iat < want-2 || iat > want+2 {
		t.Fatalf("iat = %d, want ~%d", iat, want)
	}
	digest := sha256.Sum256([]byte(access))
	if claims["ath"] != base64.RawURLEncoding.EncodeToString(digest[:]) {
		t.Fatalf("ath mismatch: %#v", claims["ath"])
	}
}
