package egress

import (
	"context"
	"errors"
	"testing"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
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
