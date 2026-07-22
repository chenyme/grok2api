package perfmetrics

import (
	"testing"
	"time"
)

func TestCollectAndResetRetainsGaugeAndClearsIntervalCounters(t *testing.T) {
	registry := NewRegistry()
	labels := Labels{Subsystem: "gateway", Operation: "responses", Provider: "grok_build", Stage: "upstream", Outcome: "success"}
	registry.ObserveDuration("stage_duration_us", labels, 3*time.Millisecond)
	registry.ObserveDuration("stage_duration_us", labels, 5*time.Millisecond)
	registry.SetGauge("queue_depth", Labels{Subsystem: "audit"}, 7)

	first := registry.CollectAndReset()
	if len(first) != 2 {
		t.Fatalf("first sample count = %d", len(first))
	}
	if first[1].Count != 2 || first[1].Total != 8000 || first[1].Maximum != 5000 {
		t.Fatalf("duration sample = %#v", first[1])
	}
	second := registry.CollectAndReset()
	if len(second) != 1 || !second[0].HasGauge || second[0].Gauge != 7 || second[0].Count != 0 {
		t.Fatalf("second samples = %#v", second)
	}
}

func TestCollectAndResetReadsDynamicGaugeAtCollectionTime(t *testing.T) {
	registry := NewRegistry()
	labels := Labels{Subsystem: "http"}
	var value int64 = 7
	registry.RegisterDynamicGauge("active_bytes", labels, func() int64 { return value })

	first := registry.CollectAndReset()
	if len(first) != 1 || !first[0].HasGauge || first[0].Gauge != 7 {
		t.Fatalf("first dynamic gauge sample = %#v", first)
	}
	value = 11
	second := registry.CollectAndReset()
	if len(second) != 1 || !second[0].HasGauge || second[0].Gauge != 11 {
		t.Fatalf("second dynamic gauge sample = %#v", second)
	}
}

func BenchmarkRegistryParallel(b *testing.B) {
	registry := NewRegistry()
	labels := Labels{Subsystem: "gateway", Operation: "responses", Provider: "grok_build", Stage: "upstream", Outcome: "success"}
	b.ReportAllocs()
	b.RunParallel(func(worker *testing.PB) {
		for worker.Next() {
			registry.ObserveDuration("stage_duration_us", labels, time.Millisecond)
		}
	})
}
