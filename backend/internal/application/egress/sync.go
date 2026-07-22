package egress

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

var ErrSubscriptionSync = errors.New("代理订阅同步失败")

func (s *Service) syncSource(ctx context.Context, operations OperationsRepository, source domain.SubscriptionSource) (ImportResult, error) {
	now := time.Now().UTC()
	nextSyncAt := now.Add(time.Duration(source.RefreshIntervalSeconds) * time.Second)
	if source.RefreshIntervalSeconds < 60 {
		nextSyncAt = now.Add(defaultProbeIntervalSeconds * time.Second)
	}
	recordFailure := func() {
		// The source URL and any transport detail are deliberately omitted from
		// persisted status and API errors; they may contain subscription tokens.
		_ = operations.UpdateEgressSourceSync(context.WithoutCancel(ctx), source.ID, now, nextSyncAt, 0, "订阅拉取或解析失败")
	}
	if strings.TrimSpace(source.EncryptedURL) == "" {
		recordFailure()
		return ImportResult{}, ErrSubscriptionSync
	}
	urlValue, err := s.cipher.Decrypt(source.EncryptedURL)
	if err != nil {
		recordFailure()
		return ImportResult{}, ErrSubscriptionSync
	}
	content, err := fetchProxySubscription(ctx, urlValue)
	if err != nil {
		recordFailure()
		return ImportResult{}, ErrSubscriptionSync
	}
	entries, skipped, err := parseProxySubscription(string(content))
	if err != nil {
		recordFailure()
		return ImportResult{}, ErrSubscriptionSync
	}
	userAgent := ""
	if source.Scope != domain.ScopeBuild {
		s.mu.RLock()
		userAgent = s.browserUA
		s.mu.RUnlock()
	}
	nodes := make([]domain.Node, 0, len(entries))
	for index, entry := range entries {
		encryptedProxy, encryptErr := s.cipher.Encrypt(entry.ProxyURL)
		if encryptErr != nil {
			recordFailure()
			return ImportResult{}, fmt.Errorf("%w: 加密导入节点", ErrSubscriptionSync)
		}
		nodes = append(nodes, domain.Node{
			Name: sourceNodeName(source.Name, index), Scope: source.Scope, Enabled: true,
			SourceID: source.ID, SourceKey: entry.Key, AccountCapacity: source.DefaultAccountCapacity,
			EncryptedProxyURL: encryptedProxy, UserAgent: userAgent, Health: 1, ProbeStatus: domain.ProbeStatusUnknown,
		})
	}
	imported, err := operations.UpsertEgressNodesFromSource(ctx, source.ID, nodes)
	if err != nil {
		recordFailure()
		return ImportResult{}, ErrSubscriptionSync
	}
	s.invalidateOperationsConfig()
	if err := operations.UpdateEgressSourceSync(ctx, source.ID, now, nextSyncAt, imported, ""); err != nil {
		return ImportResult{}, err
	}
	return ImportResult{Imported: imported, Skipped: skipped}, nil
}
