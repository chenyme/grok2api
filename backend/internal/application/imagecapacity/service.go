package imagecapacity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/buildinfo"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

const SchemaVersion = "grok-image-capacity-attestation-v2"

const CoverageOperation = "image_generation"

const RetryPolicy = "image_pre_submit_cross_egress_v1"

const (
	identitySetHashDomain   = "grok2api-image-identity-set-v1"
	routeTopologyHashDomain = "grok2api-image-route-topology-v1"
)

var (
	ErrInvalidInput        = errors.New("image capacity attestation input is invalid")
	ErrClientKeyNotFound   = errors.New("client key is unavailable")
	ErrRouteNotFound       = errors.New("image route does not exist")
	ErrRouteNotAttestable  = errors.New("image route is not attestable")
	ErrBuildNotAttestable  = errors.New("runtime build is not attestable")
	ErrEvidenceUnavailable = errors.New("image capacity evidence is unavailable")
)

type keyReader interface {
	GetAttestationIdentity(context.Context, uint64, time.Time) (clientkeyapp.AttestationIdentity, error)
}

type routeReader interface {
	Get(context.Context, uint64) (modeldomain.Route, error)
}

type candidateProjector interface {
	ImageCapacityEligibilityForKey(context.Context, account.Provider, uint64, string, string, clientkeydomain.AccountScope) ([]uint64, []uint64, bool, error)
	ImageCapacityFairnessPolicy() string
}

type routingPolicyReader interface {
	ImageCapacityMaxAttempts() int
}

type quotaModeResolver interface {
	QuotaMode(account.Provider, string) string
}

type Request struct {
	ClientKeyID uint64
	RouteID     uint64
	Since       *time.Time
	RunMarker   string
}

type RouteAttestation struct {
	ID             string `json:"id"`
	PublicID       string `json:"publicId"`
	UpstreamModel  string `json:"upstreamModel"`
	Capability     string `json:"capability"`
	BindingMode    bool   `json:"bindingMode"`
	TopologySHA256 string `json:"topologySha256"`
}

type CoverageAttestation struct {
	Operation                           string    `json:"operation"`
	Since                               time.Time `json:"since"`
	RunMarker                           string    `json:"runMarker"`
	SelectedSuccessfulIdentityCount     int       `json:"selectedSuccessfulIdentityCount"`
	SelectedSuccessfulIdentitySetSHA256 string    `json:"selectedSuccessfulIdentitySetSha256"`
	TerminalSuccessCount                int64     `json:"terminalSuccessCount"`
}

type RoutingAttestation struct {
	RetryPolicy         string `json:"retryPolicy"`
	MaxAttempts         int    `json:"maxAttempts"`
	EligibleEgressCount int    `json:"eligibleEgressCount"`
	FairnessPolicy      string `json:"fairnessPolicy"`
}

type Attestation struct {
	SchemaVersion                  string                `json:"schemaVersion"`
	ObservedAt                     time.Time             `json:"observedAt"`
	ClientKeyFingerprint           string                `json:"clientKeyFingerprint"`
	Route                          RouteAttestation      `json:"route"`
	EligibleImageIdentityCount     int                   `json:"eligibleImageIdentityCount"`
	EligibleImageIdentitySetSHA256 string                `json:"eligibleImageIdentitySetSha256"`
	Routing                        RoutingAttestation    `json:"routing"`
	Build                          buildinfo.Attestation `json:"build"`
	Coverage                       *CoverageAttestation  `json:"coverage,omitempty"`
}

type Service struct {
	keys       keyReader
	routes     routeReader
	quotaModes quotaModeResolver
	candidates candidateProjector
	routing    routingPolicyReader
	coverage   repository.SuccessfulImageCoverageRepository
	now        func() time.Time
	build      func() (buildinfo.Attestation, error)
}

func NewService(keys keyReader, routes routeReader, quotaModes quotaModeResolver, candidates candidateProjector, routing routingPolicyReader, coverage repository.SuccessfulImageCoverageRepository) *Service {
	return &Service{keys: keys, routes: routes, quotaModes: quotaModes, candidates: candidates, routing: routing, coverage: coverage, now: time.Now, build: buildinfo.CurrentAttestation}
}

func (s *Service) Attest(ctx context.Context, request Request) (Attestation, error) {
	if request.ClientKeyID == 0 || request.RouteID == 0 || (request.Since == nil) != (request.RunMarker == "") {
		return Attestation{}, ErrInvalidInput
	}
	observedAt := s.now().UTC()
	if request.Since != nil && (request.Since.IsZero() || request.Since.After(observedAt) || !validRunMarker(request.RunMarker)) {
		return Attestation{}, ErrInvalidInput
	}
	build, err := s.build()
	if err != nil {
		return Attestation{}, errors.Join(ErrBuildNotAttestable, err)
	}
	identity, err := s.keys.GetAttestationIdentity(ctx, request.ClientKeyID, observedAt)
	if err != nil {
		if errors.Is(err, clientkeyapp.ErrNotFound) {
			return Attestation{}, ErrClientKeyNotFound
		}
		return Attestation{}, errors.Join(ErrEvidenceUnavailable, err)
	}
	route, err := s.routes.Get(ctx, request.RouteID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return Attestation{}, ErrRouteNotFound
		}
		return Attestation{}, errors.Join(ErrEvidenceUnavailable, err)
	}
	if !route.Enabled || route.Provider != account.ProviderWeb || route.Capability != modeldomain.CapabilityImage || len(route.BoundAccountIDs) == 0 || !identity.AllowsModel(route.ID) {
		return Attestation{}, ErrRouteNotAttestable
	}
	quotaMode := s.quotaModes.QuotaMode(route.Provider, route.UpstreamModel)
	if quotaMode != account.QuotaModeWebImagePro {
		return Attestation{}, ErrRouteNotAttestable
	}
	eligibleIDs, eligibleEgressIDs, allEgressBound, err := s.candidates.ImageCapacityEligibilityForKey(ctx, route.Provider, route.ID, route.UpstreamModel, quotaMode, identity.AccountScope)
	if err != nil {
		return Attestation{}, errors.Join(ErrEvidenceUnavailable, err)
	}
	eligibleIDs = normalizedIDs(eligibleIDs)
	eligibleEgressIDs = normalizedIDs(eligibleEgressIDs)
	maxAttempts := s.routing.ImageCapacityMaxAttempts()
	fairnessPolicy := s.candidates.ImageCapacityFairnessPolicy()
	if !allEgressBound || fairnessPolicy != account.ImageProFairnessPolicy || (maxAttempts != -1 && (maxAttempts <= 0 || maxAttempts < len(eligibleEgressIDs))) {
		return Attestation{}, ErrRouteNotAttestable
	}
	result := Attestation{
		SchemaVersion:                  SchemaVersion,
		ObservedAt:                     observedAt,
		ClientKeyFingerprint:           identity.KeyFingerprint,
		Route:                          routeAttestation(route),
		EligibleImageIdentityCount:     len(eligibleIDs),
		EligibleImageIdentitySetSHA256: identitySetSHA256(eligibleIDs),
		Routing: RoutingAttestation{
			RetryPolicy: RetryPolicy, MaxAttempts: maxAttempts, EligibleEgressCount: len(eligibleEgressIDs), FairnessPolicy: fairnessPolicy,
		},
		Build: build,
	}
	if request.Since == nil {
		return result, nil
	}
	coverage, err := s.coverage.SummarizeSuccessfulImageCoverage(ctx, repository.SuccessfulImageCoverageQuery{
		ClientKeyID: request.ClientKeyID, ModelRouteID: route.ID, Since: request.Since.UTC(), Until: observedAt, RunMarker: request.RunMarker,
	})
	if err != nil {
		return Attestation{}, errors.Join(ErrEvidenceUnavailable, err)
	}
	selectedIDs := normalizedIDs(coverage.AccountIDs)
	result.Coverage = &CoverageAttestation{
		Operation: CoverageOperation, Since: request.Since.UTC(), RunMarker: request.RunMarker,
		SelectedSuccessfulIdentityCount: len(selectedIDs), SelectedSuccessfulIdentitySetSHA256: identitySetSHA256(selectedIDs),
		TerminalSuccessCount: coverage.TerminalSuccessCount,
	}
	return result, nil
}

func routeAttestation(route modeldomain.Route) RouteAttestation {
	return RouteAttestation{
		ID: strconv.FormatUint(route.ID, 10), PublicID: modeldomain.ExternalPublicID(route.Provider, route.PublicID),
		UpstreamModel: route.UpstreamModel, Capability: string(route.Capability), BindingMode: true,
		TopologySHA256: routeTopologySHA256(route),
	}
}

func routeTopologySHA256(route modeldomain.Route) string {
	ids := normalizedIDs(route.BoundAccountIDs)
	parts := []string{
		routeTopologyHashDomain,
		strconv.FormatUint(route.ID, 10),
		modeldomain.ExternalPublicID(route.Provider, route.PublicID),
		route.UpstreamModel,
		string(route.Capability),
		"true",
	}
	for _, id := range ids {
		parts = append(parts, strconv.FormatUint(id, 10))
	}
	return nulSeparatedSHA256(parts)
}

func identitySetSHA256(ids []uint64) string {
	parts := []string{identitySetHashDomain}
	for _, id := range normalizedIDs(ids) {
		parts = append(parts, strconv.FormatUint(id, 10))
	}
	return nulSeparatedSHA256(parts)
}

func nulSeparatedSHA256(parts []string) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = hash.Write([]byte(part))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func normalizedIDs(values []uint64) []uint64 {
	if len(values) == 0 {
		return nil
	}
	result := append([]uint64(nil), values...)
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	write := 0
	for _, value := range result {
		if value == 0 || (write > 0 && result[write-1] == value) {
			continue
		}
		result[write] = value
		write++
	}
	return result[:write]
}

func validRunMarker(value string) bool {
	if len(value) != 24 || value != strings.TrimSpace(value) {
		return false
	}
	for _, character := range []byte(value) {
		if (character < 'a' || character > 'f') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}
