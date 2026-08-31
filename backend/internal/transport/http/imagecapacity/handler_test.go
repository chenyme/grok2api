package imagecapacity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	imagecapacityapp "github.com/chenyme/grok2api/backend/internal/application/imagecapacity"
	"github.com/chenyme/grok2api/backend/internal/buildinfo"
	"github.com/gin-gonic/gin"
)

type stubAttester struct {
	value imagecapacityapp.Attestation
	err   error
}

func (s stubAttester) Attest(context.Context, imagecapacityapp.Request) (imagecapacityapp.Attestation, error) {
	return s.value, s.err
}

func TestImageCapacityAttestationHTTPPrivacyContractIsExact(t *testing.T) {
	gin.SetMode(gin.TestMode)
	observedAt := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	value := imagecapacityapp.Attestation{
		SchemaVersion: "grok-image-capacity-attestation-v2", ObservedAt: observedAt,
		ClientKeyFingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Route: imagecapacityapp.RouteAttestation{
			ID: "7", PublicID: "grok-image", UpstreamModel: "grok-image-upstream", Capability: "image", BindingMode: true,
			TopologySHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
		EligibleImageIdentityCount:     2,
		EligibleImageIdentitySetSHA256: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		Routing: imagecapacityapp.RoutingAttestation{
			RetryPolicy: imagecapacityapp.RetryPolicy, MaxAttempts: 999,
			EligibleEgressCount: 2, FairnessPolicy: "image_pro_oldest_selection_first_v1",
		},
		Build: buildinfo.Attestation{
			SourceCommit:       "0123456789abcdef0123456789abcdef01234567",
			RuntimeImageDigest: "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
			BuildFingerprint:   "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		},
		Coverage: &imagecapacityapp.CoverageAttestation{
			Operation: imagecapacityapp.CoverageOperation,
			Since:     observedAt.Add(-time.Hour), RunMarker: "3b36292091cb9b4d9b27cc37",
			SelectedSuccessfulIdentityCount:     2,
			SelectedSuccessfulIdentitySetSHA256: "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
			TerminalSuccessCount:                3,
		},
	}
	router := gin.New()
	(&Handler{service: stubAttester{value: value}}).Register(router.Group(""))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/client-keys/9/image-capacity-attestation?routeId=7&since=2026-08-30T09:00:00Z&runMarker=3b36292091cb9b4d9b27cc37", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	want := `{"data":{"schemaVersion":"grok-image-capacity-attestation-v2","observedAt":"2026-08-30T10:00:00Z","clientKeyFingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","route":{"id":"7","publicId":"grok-image","upstreamModel":"grok-image-upstream","capability":"image","bindingMode":true,"topologySha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"eligibleImageIdentityCount":2,"eligibleImageIdentitySetSha256":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","routing":{"retryPolicy":"image_pre_submit_cross_egress_v1","maxAttempts":999,"eligibleEgressCount":2,"fairnessPolicy":"image_pro_oldest_selection_first_v1"},"build":{"sourceCommit":"0123456789abcdef0123456789abcdef01234567","runtimeImageDigest":"sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","buildFingerprint":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"},"coverage":{"operation":"image_generation","since":"2026-08-30T09:00:00Z","runMarker":"3b36292091cb9b4d9b27cc37","selectedSuccessfulIdentityCount":2,"selectedSuccessfulIdentitySetSha256":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff","terminalSuccessCount":3}}}`
	if strings.TrimSpace(recorder.Body.String()) != want {
		t.Fatalf("body = %s", recorder.Body.String())
	}
	for _, forbidden := range []string{"accountId", "accountName", "email", "requestId", "sourceKey", "audit"} {
		if strings.Contains(recorder.Body.String(), forbidden) {
			t.Fatalf("response leaked %q: %s", forbidden, recorder.Body.String())
		}
	}
}

func TestImageCapacityAttestationExplainsImageProRestriction(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	(&Handler{service: stubAttester{err: imagecapacityapp.ErrRouteNotAttestable}}).Register(router.Group(""))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/client-keys/9/image-capacity-attestation?routeId=7", nil))
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), `"code":"imageRouteNotAttestable"`) || !strings.Contains(recorder.Body.String(), "image_pro") {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}
