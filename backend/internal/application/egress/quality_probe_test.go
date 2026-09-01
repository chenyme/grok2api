package egress

import (
	"context"
	"errors"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type qualityProbeRepository struct{ node domain.Node }

func (r *qualityProbeRepository) ListEgressNodes(context.Context, domain.Scope, repository.SortQuery) ([]domain.Node, error) {
	return []domain.Node{r.node}, nil
}
func (r *qualityProbeRepository) GetEgressNode(_ context.Context, id uint64) (domain.Node, error) {
	if id != r.node.ID {
		return domain.Node{}, repository.ErrNotFound
	}
	return r.node, nil
}
func (r *qualityProbeRepository) CreateEgressNode(_ context.Context, value domain.Node) (domain.Node, error) {
	return value, nil
}
func (r *qualityProbeRepository) UpdateEgressNode(_ context.Context, value domain.Node) (domain.Node, error) {
	return value, nil
}
func (r *qualityProbeRepository) DeleteEgressNode(context.Context, uint64) error { return nil }
func (r *qualityProbeRepository) ListEgressNodePage(context.Context, repository.EgressNodeListQuery) ([]domain.Node, int64, error) {
	return []domain.Node{r.node}, 1, nil
}

type qualityProberStub struct {
	nodeID uint64
	input  QualityProbeInput
}

type qualityLeaseAccountRepository struct {
	credential accountdomain.Credential
	stored     accountdomain.EgressLeaseBlock
}

func (r *qualityLeaseAccountRepository) CountProviderAccountsByIDs(context.Context, accountdomain.Provider, []uint64) (int64, error) {
	return 1, nil
}
func (r *qualityLeaseAccountRepository) UpdateEgressBindings(context.Context, accountdomain.Provider, []uint64, *uint64, accountdomain.EgressAssignmentMode, time.Time) (int64, error) {
	return 1, nil
}
func (r *qualityLeaseAccountRepository) ListEgressAssignments(context.Context, accountdomain.Provider) ([]accountdomain.Credential, error) {
	return []accountdomain.Credential{r.credential}, nil
}
func (r *qualityLeaseAccountRepository) ListEgressBindingProviders(context.Context, uint64) ([]accountdomain.Provider, error) {
	return []accountdomain.Provider{accountdomain.ProviderBuild}, nil
}
func (r *qualityLeaseAccountRepository) ListEgressSourceBindingProviders(context.Context, uint64) ([]accountdomain.Provider, error) {
	return []accountdomain.Provider{}, nil
}
func (r *qualityLeaseAccountRepository) Get(_ context.Context, id uint64) (accountdomain.Credential, error) {
	if id != r.credential.ID {
		return accountdomain.Credential{}, repository.ErrNotFound
	}
	return r.credential, nil
}
func (r *qualityLeaseAccountRepository) ListEgressLeaseBlocks(context.Context, int, *accountdomain.EgressLeaseBlockCursor) ([]accountdomain.EgressLeaseBlock, error) {
	if r.stored.AccountID == 0 {
		return []accountdomain.EgressLeaseBlock{}, nil
	}
	return []accountdomain.EgressLeaseBlock{r.stored}, nil
}
func (r *qualityLeaseAccountRepository) UpsertEgressLeaseBlock(_ context.Context, value accountdomain.EgressLeaseBlock) (accountdomain.EgressLeaseBlock, error) {
	r.stored = value
	return value, nil
}
func (r *qualityLeaseAccountRepository) DeleteEgressLeaseBlock(context.Context, uint64, uint64, string) (bool, error) {
	return true, nil
}
func (r *qualityLeaseAccountRepository) DeleteEgressLeaseBlocksByNodes(context.Context, []uint64) (int64, error) {
	return 0, nil
}
func (r *qualityLeaseAccountRepository) PruneInvalidEgressLeaseBlocks(context.Context, int) (int64, error) {
	return 0, nil
}

func (p *qualityProberStub) ProbeEgressQuality(_ context.Context, nodeID uint64, input QualityProbeInput) (QualityProbeResult, error) {
	p.nodeID = nodeID
	p.input = input
	return QualityProbeResult{NodeID: nodeID, ExpectedMatched: true}, nil
}

func TestProbeQualityNormalizesDefaultsAndAllowsDisabledNode(t *testing.T) {
	repository := &qualityProbeRepository{node: domain.Node{
		ID: 7, Scope: domain.ScopeBuild, Enabled: false, EncryptedProxyURL: "encrypted",
	}}
	prober := &qualityProberStub{}
	service := NewService(repository, nil, "")
	service.SetQualityProber(prober)
	result, err := service.ProbeQuality(context.Background(), 7, QualityProbeInput{ClientKeyID: 3, Model: " grok-test "})
	if err != nil {
		t.Fatal(err)
	}
	if result.NodeID != 7 || prober.nodeID != 7 || prober.input.Prompt != DefaultQualityProbePrompt || prober.input.Expected != DefaultQualityProbeExpected || prober.input.MaxOutputTokens != DefaultQualityProbeMaxOutputTokens {
		t.Fatalf("probe input=%#v result=%#v", prober.input, result)
	}
}

func TestProbeQualityRejectsUnsupportedNodeAndMissingProber(t *testing.T) {
	repository := &qualityProbeRepository{node: domain.Node{ID: 7, Scope: domain.ScopeWeb, EncryptedProxyURL: "encrypted"}}
	service := NewService(repository, nil, "")
	_, err := service.ProbeQuality(context.Background(), 7, QualityProbeInput{ClientKeyID: 3, Model: "grok-test"})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unsupported node error = %v", err)
	}
	repository.node.Scope = domain.ScopeBuild
	_, err = service.ProbeQuality(context.Background(), 7, QualityProbeInput{ClientKeyID: 3, Model: "grok-test"})
	if !errors.Is(err, ErrQualityProbeUnavailable) {
		t.Fatalf("missing prober error = %v", err)
	}
}

func TestProbeQualityScopesThinkingGuardToReasoningBuildModels(t *testing.T) {
	repository := &qualityProbeRepository{node: domain.Node{
		ID: 7, Scope: domain.ScopeBuild, Enabled: true, EncryptedProxyURL: "encrypted",
	}}
	prober := &qualityProberStub{}
	service := NewService(repository, nil, "")
	service.SetQualityProber(prober)

	result, err := service.ProbeQuality(context.Background(), 7, QualityProbeInput{
		ClientKeyID: 3, Model: "grok-4.5", RequireThinking: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !prober.input.RequireThinking || !result.ThinkingRequired {
		t.Fatalf("reasoning model probe=%#v result=%#v", prober.input, result)
	}

	result, err = service.ProbeQuality(context.Background(), 7, QualityProbeInput{
		ClientKeyID: 3, Model: "grok-composer-2.5-fast", RequireThinking: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if prober.input.RequireThinking || result.ThinkingRequired {
		t.Fatalf("non-reasoning model probe=%#v result=%#v", prober.input, result)
	}
}

func TestQualityLeaseUsesObservedNodeForUnboundAccountButHonorsExplicitBinding(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encryptedProxy, err := cipher.Encrypt("socks5h://Default.{account}:token@resin:2260")
	if err != nil {
		t.Fatal(err)
	}
	nodes := &qualityProbeRepository{node: domain.Node{
		ID: 7, Scope: domain.ScopeBuild, Enabled: true, EncryptedProxyURL: encryptedProxy,
	}}
	accounts := &qualityLeaseAccountRepository{credential: accountdomain.Credential{
		ID: 11, Provider: accountdomain.ProviderBuild, Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	}}
	prober := &qualityProberStub{}
	service := NewService(nodes, cipher, "", accounts)
	service.SetQualityProber(prober)

	if _, err := service.ProbeQuality(context.Background(), 7, QualityProbeInput{
		AccountID: 11, ClientKeyID: 3, Model: "grok-test",
	}); err != nil {
		t.Fatalf("unbound account recovery probe rejected the observed node: %v", err)
	}
	lease, err := service.QuarantineQualityLease(context.Background(), QualityLeaseInput{
		AccountID: 11, NodeID: 7, Reason: "hard_tps", QuarantineSeconds: 60,
	})
	if err != nil {
		t.Fatalf("unbound account quarantine rejected the observed node: %v", err)
	}
	if lease.AccountID != 11 || lease.NodeID != 7 || accounts.stored.NodeID != 7 {
		t.Fatalf("stored lease = %#v", lease)
	}

	accounts.credential.EgressNodeID = 9
	if _, err := service.ProbeQuality(context.Background(), 7, QualityProbeInput{
		AccountID: 11, ClientKeyID: 3, Model: "grok-test",
	}); !errors.Is(err, ErrQualityProbeNoAccount) {
		t.Fatalf("probe through a node different from the explicit binding returned %v", err)
	}
	if _, err := service.QuarantineQualityLease(context.Background(), QualityLeaseInput{
		AccountID: 11, NodeID: 7, Reason: "hard_tps", QuarantineSeconds: 60,
	}); !errors.Is(err, ErrQualityLeaseConflict) {
		t.Fatalf("quarantine on a node different from the explicit binding returned %v", err)
	}
}
