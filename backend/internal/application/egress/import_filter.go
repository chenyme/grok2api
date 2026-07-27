package egress

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

const (
	maxSubscriptionImportLatencyMS = 60000
	maxSubscriptionImportCountries = 50
)

type screenedSubscriptionEntry struct {
	Entry subscriptionEntry
	Probe *domain.ProbeResult
}

func normalizeSubscriptionImportFilter(input SubscriptionImportFilterInput) (domain.SubscriptionImportFilter, error) {
	if input.MaxLatencyMS < 0 || input.MaxLatencyMS > maxSubscriptionImportLatencyMS {
		return domain.SubscriptionImportFilter{}, fmt.Errorf("%w: 最大延迟必须在 0 到 %d 毫秒之间", ErrInvalidInput, maxSubscriptionImportLatencyMS)
	}
	if len(input.Countries) > maxSubscriptionImportCountries {
		return domain.SubscriptionImportFilter{}, fmt.Errorf("%w: 最多选择 %d 个国家或地区", ErrInvalidInput, maxSubscriptionImportCountries)
	}
	countries := make(map[string]struct{}, len(input.Countries))
	for _, raw := range input.Countries {
		country := strings.ToUpper(strings.TrimSpace(raw))
		if country == "" {
			continue
		}
		if len(country) != 2 || !isASCIILetter(country[0]) || !isASCIILetter(country[1]) {
			return domain.SubscriptionImportFilter{}, fmt.Errorf("%w: 国家或地区代码必须为两个字母", ErrInvalidInput)
		}
		countries[country] = struct{}{}
	}
	if len(countries) > maxSubscriptionImportCountries {
		return domain.SubscriptionImportFilter{}, fmt.Errorf("%w: 最多选择 %d 个国家或地区", ErrInvalidInput, maxSubscriptionImportCountries)
	}
	values := make([]string, 0, len(countries))
	for country := range countries {
		values = append(values, country)
	}
	sort.Strings(values)

	return domain.SubscriptionImportFilter{MaxLatencyMS: input.MaxLatencyMS, Countries: values}, nil
}

func isASCIILetter(value byte) bool {
	return (value >= 'A' && value <= 'Z') || (value >= 'a' && value <= 'z')
}

func (s *Service) screenSubscriptionEntries(ctx context.Context, entries []subscriptionEntry, filter domain.SubscriptionImportFilter) ([]screenedSubscriptionEntry, int, error) {
	if !filter.Active() {
		values := make([]screenedSubscriptionEntry, 0, len(entries))
		for _, entry := range entries {
			values = append(values, screenedSubscriptionEntry{Entry: entry})
		}
		return values, 0, nil
	}
	prober, ok := s.nodeProber().(ProxyProber)
	if !ok || prober == nil {
		return nil, 0, ErrOperationsUnavailable
	}
	results := make([]screenedSubscriptionEntry, len(entries))
	accepted := make([]bool, len(entries))
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(maxConcurrentProbes, len(entries)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				probe, err := prober.ProbeEgressProxy(ctx, entries[index].ProxyURL)
				if err != nil || probe.Status != domain.ProbeStatusHealthy || !matchesSubscriptionImportFilter(filter, probe) {
					continue
				}
				if probe.TestedAt.IsZero() {
					probe.TestedAt = time.Now().UTC()
				}
				results[index] = screenedSubscriptionEntry{Entry: entries[index], Probe: &probe}
				accepted[index] = true
			}
		}()
	}
	for index := range entries {
		select {
		case jobs <- index:
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return nil, 0, ctx.Err()
		}
	}
	close(jobs)
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	values := make([]screenedSubscriptionEntry, 0, len(entries))
	for index, accepted := range accepted {
		if accepted {
			values = append(values, results[index])
		}
	}
	return values, len(entries) - len(values), nil
}

func matchesSubscriptionImportFilter(filter domain.SubscriptionImportFilter, probe domain.ProbeResult) bool {
	if probe.Status != domain.ProbeStatusHealthy {
		return false
	}
	if filter.MaxLatencyMS > 0 && probe.LatencyMS > filter.MaxLatencyMS {
		return false
	}
	if len(filter.Countries) > 0 {
		country := strings.ToUpper(strings.TrimSpace(probe.ExitCountry))
		matched := false
		for _, allowed := range filter.Countries {
			if allowed == country {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func applyProbeResult(node *domain.Node, probe domain.ProbeResult) {
	if node == nil {
		return
	}
	node.ProbeStatus = probe.Status
	node.LastProbedAt = &probe.TestedAt
	node.ProbeLatencyMS = probe.LatencyMS
	node.ExitIP = probe.ExitIP
	node.ExitCountry = probe.ExitCountry
	node.ProbeError = probe.Error
}
