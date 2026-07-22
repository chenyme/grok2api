package perfmetrics

import (
	"sort"
	"sync"
	"time"
)

// Labels intentionally exposes only bounded dimensions. Callers must use
// controlled enums and must never include account, request, token, model, or
// proxy identifiers.
type Labels struct {
	Subsystem string
	Operation string
	Provider  string
	Plane     string
	Stage     string
	Ordinal   string
	Outcome   string
}

type Sample struct {
	Name     string
	Labels   Labels
	Count    uint64
	Total    int64
	Maximum  int64
	Gauge    int64
	HasGauge bool
}

type metricKey struct {
	name   string
	labels Labels
}

type metricValue struct {
	count    uint64
	total    int64
	maximum  int64
	gauge    int64
	hasGauge bool
}

const registryShardCount = 32

type registryShard struct {
	mu     sync.Mutex
	values map[metricKey]metricValue
}

type Registry struct {
	shards        [registryShardCount]registryShard
	dynamicMu     sync.RWMutex
	dynamicGauges map[metricKey]func() int64
}

func NewRegistry() *Registry {
	registry := &Registry{}
	for index := range registry.shards {
		registry.shards[index].values = make(map[metricKey]metricValue)
	}
	registry.dynamicGauges = make(map[metricKey]func() int64)
	return registry
}

var Default = NewRegistry()

func (r *Registry) Inc(name string, labels Labels) {
	r.Add(name, labels, 1)
}

func (r *Registry) Add(name string, labels Labels, delta int64) {
	if r == nil || name == "" || delta == 0 {
		return
	}
	key := metricKey{name: name, labels: labels}
	shard := r.shard(key)
	shard.mu.Lock()
	value := shard.values[key]
	value.count++
	value.total += delta
	if delta > value.maximum {
		value.maximum = delta
	}
	shard.values[key] = value
	shard.mu.Unlock()
}

func (r *Registry) ObserveDuration(name string, labels Labels, duration time.Duration) {
	r.Add(name, labels, max(0, duration.Microseconds()))
}

func (r *Registry) SetGauge(name string, labels Labels, value int64) {
	if r == nil || name == "" {
		return
	}
	key := metricKey{name: name, labels: labels}
	shard := r.shard(key)
	shard.mu.Lock()
	current := shard.values[key]
	current.gauge = value
	current.hasGauge = true
	shard.values[key] = current
	shard.mu.Unlock()
}

// RegisterDynamicGauge exposes a bounded gauge whose value is read only when
// metrics are collected. The callback must be non-blocking and side-effect
// free; it is intended for atomic runtime state such as active request bytes.
func (r *Registry) RegisterDynamicGauge(name string, labels Labels, read func() int64) {
	if r == nil || name == "" || read == nil {
		return
	}
	key := metricKey{name: name, labels: labels}
	r.dynamicMu.Lock()
	if r.dynamicGauges == nil {
		r.dynamicGauges = make(map[metricKey]func() int64)
	}
	r.dynamicGauges[key] = read
	r.dynamicMu.Unlock()
}

// CollectAndReset returns interval counters while retaining the latest gauge
// values for the next collection window.
func (r *Registry) CollectAndReset() []Sample {
	if r == nil {
		return nil
	}
	result := make([]Sample, 0, registryShardCount)
	for index := range r.shards {
		shard := &r.shards[index]
		shard.mu.Lock()
		for key, value := range shard.values {
			result = append(result, Sample{
				Name: key.name, Labels: key.labels, Count: value.count,
				Total: value.total, Maximum: value.maximum,
				Gauge: value.gauge, HasGauge: value.hasGauge,
			})
			if value.hasGauge {
				value.count = 0
				value.total = 0
				value.maximum = 0
				shard.values[key] = value
			} else {
				delete(shard.values, key)
			}
		}
		shard.mu.Unlock()
	}
	r.dynamicMu.RLock()
	dynamic := make(map[metricKey]func() int64, len(r.dynamicGauges))
	for key, read := range r.dynamicGauges {
		dynamic[key] = read
	}
	r.dynamicMu.RUnlock()
	for key, read := range dynamic {
		value := Sample{Name: key.name, Labels: key.labels, Gauge: read(), HasGauge: true}
		found := false
		for index := range result {
			if result[index].Name == key.name && result[index].Labels == key.labels {
				result[index].Gauge = value.Gauge
				result[index].HasGauge = true
				found = true
				break
			}
		}
		if !found {
			result = append(result, value)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		return labelsKey(result[i].Labels) < labelsKey(result[j].Labels)
	})
	return result
}

func (r *Registry) shard(key metricKey) *registryShard {
	hash := uint64(1469598103934665603)
	hash = hashMetricString(hash, key.name)
	hash = hashMetricString(hash, key.labels.Subsystem)
	hash = hashMetricString(hash, key.labels.Operation)
	hash = hashMetricString(hash, key.labels.Provider)
	hash = hashMetricString(hash, key.labels.Plane)
	hash = hashMetricString(hash, key.labels.Stage)
	hash = hashMetricString(hash, key.labels.Ordinal)
	hash = hashMetricString(hash, key.labels.Outcome)
	return &r.shards[hash%registryShardCount]
}

func hashMetricString(hash uint64, value string) uint64 {
	for index := 0; index < len(value); index++ {
		hash ^= uint64(value[index])
		hash *= 1099511628211
	}
	hash ^= 0xff
	return hash * 1099511628211
}

func labelsKey(value Labels) string {
	return value.Subsystem + "\x00" + value.Operation + "\x00" + value.Provider + "\x00" + value.Plane + "\x00" + value.Stage + "\x00" + value.Ordinal + "\x00" + value.Outcome
}
