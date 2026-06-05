package caddyrl

import (
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddytest"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestMetrics(t *testing.T) {
	// Reset the metrics registry to ensure clean state
	prometheus.DefaultRegisterer = prometheus.NewRegistry()

	// Reset global metrics
	globalMetrics.Store(nil)

	window := 10
	maxEvents := 2

	// Admin API must be exposed on port 2999 to match what caddytest.Tester does
	config := fmt.Sprintf(`{
	"admin": {"listen": "localhost:2999"},
	"apps": {
		"http": {
			"servers": {
				"demo": {
					"listen": [":8080"],
					"metrics": {},
					"routes": [{
						"handle": [
							{
								"handler": "rate_limit",
								"rate_limits": {
									"test_zone": {
										"match": [{"method": ["GET"]}],
										"key": "static",
										"window": "%ds",
										"max_events": %d
									}
								}
							},
							{
								"handler": "static_response",
								"status_code": 200
							}
						]
					}]
				}
			}
		}
	}
}`, window, maxEvents)

	initTime()

	tester := caddytest.NewTester(t)
	tester.InitServer(config, "json")

	// Ensure metrics are initialized
	m := globalMetrics.Load()
	if m == nil {
		t.Fatal("Expected globalMetrics to be initialized")
	}

	// Test that configuration metrics are recorded
	configMetric := testutil.ToFloat64(m.config.WithLabelValues("test_zone", strconv.Itoa(maxEvents), fmt.Sprintf("%ds", window), "0", "0"))
	if configMetric == 0 {
		t.Error("Expected configuration metric to be recorded")
	}

	// Make some requests that should be allowed
	for i := 0; i < maxEvents; i++ {
		tester.AssertGetResponse("http://localhost:8080", 200, "")
	}

	// Check request metrics - verify both zone-level and per-key metrics
	zoneLevelRequestsMetric := testutil.ToFloat64(m.requestsTotal.WithLabelValues("test_zone", ""))
	perKeyRequestsMetric := testutil.ToFloat64(m.requestsTotal.WithLabelValues("test_zone", "static"))

	if zoneLevelRequestsMetric < float64(maxEvents) {
		t.Errorf("Expected at least %d zone-level requests metric, got %f", maxEvents, zoneLevelRequestsMetric)
	}
	if perKeyRequestsMetric < float64(maxEvents) {
		t.Errorf("Expected at least %d per-key requests metric, got %f", maxEvents, perKeyRequestsMetric)
	}

	// Make a request that should be declined
	tester.AssertGetResponse("http://localhost:8080", 429, "")

	// Check declined requests metrics - verify both zone-level and per-key metrics
	zoneLevelDeclinedMetric := testutil.ToFloat64(m.declinedTotal.WithLabelValues("test_zone", ""))
	perKeyDeclinedMetric := testutil.ToFloat64(m.declinedTotal.WithLabelValues("test_zone", "static"))

	if zoneLevelDeclinedMetric == 0 {
		t.Error("Expected zone-level declined requests metric to be recorded")
	}
	if perKeyDeclinedMetric == 0 {
		t.Error("Expected per-key declined requests metric to be recorded")
	}

	// Check process time histograms - verify both zone-level and per-key metrics
	zoneLevelProcessTimeHistogram := m.processTime.WithLabelValues("test_zone", "")
	perKeyProcessTimeHistogram := m.processTime.WithLabelValues("test_zone", "static")

	if zoneLevelProcessTimeHistogram == nil {
		t.Error("Expected zone-level process time histogram to be created")
	}
	if perKeyProcessTimeHistogram == nil {
		t.Error("Expected per-key process time histogram to be created")
	}
}

// TestMetricsReRegisterOnReload is a regression test for issue #100: on a Caddy
// config reload the handler is re-provisioned with a brand-new metrics registry
// (caddy.Context.GetMetricsRegistry returns a fresh prometheus registry per
// context). The rate limit collectors must be re-registered against that new
// registry and globalMetrics re-pointed at them, otherwise increments land in
// the old, orphaned registry while /metrics serves the new one — making
// caddy_rate_limit_* metrics appear stuck/absent after any reload.
func TestMetricsReRegisterOnReload(t *testing.T) {
	globalMetrics.Store(nil)

	// First config load: register against the first per-context registry.
	reg1 := prometheus.NewPedanticRegistry()
	if err := registerMetrics(reg1); err != nil {
		t.Fatalf("initial registration: %v", err)
	}

	mc := newMetricsCollector()
	mc.recordDeclinedRequest("zoneA", "k1") // recorded into reg1's collectors

	// Caddy reload: a brand-new registry replaces reg1 and is what /metrics now
	// serves. The same handler code path runs registerMetrics again.
	reg2 := prometheus.NewPedanticRegistry()
	if err := registerMetrics(reg2); err != nil {
		t.Fatalf("reload registration: %v", err)
	}

	// After the reload, increments must land in the registry that /metrics now
	// serves (reg2), not the orphaned reg1.
	mc.recordDeclinedRequest("zoneA", "k1")

	// The post-reload registry must actually contain the rate limit series.
	// Before the fix, reg2 had zero caddy_rate_limit_* samples after reload.
	got, err := testutil.GatherAndCount(reg2, "caddy_rate_limit_declined_requests_total")
	if err != nil {
		t.Fatalf("gathering reg2: %v", err)
	}
	if got != 2 { // recordDeclinedRequest writes the zone aggregate + the per-key series
		t.Fatalf("expected 2 declined series in the post-reload registry, got %d", got)
	}

	value := testutil.ToFloat64(globalMetrics.Load().declinedTotal.WithLabelValues("zoneA", "k1"))
	if value != 1 {
		t.Errorf("expected post-reload per-key declined counter = 1, got %v", value)
	}
}

// TestMetricsConcurrentReloadNoRace exercises the data race the previous
// singleton design had (issue #100): config reloads write globalMetrics from a
// separate goroutine while in-flight requests read it on the hot path. Run with
// `go test -race` to assert the atomic.Pointer makes that access safe.
func TestMetricsConcurrentReloadNoRace(t *testing.T) {
	globalMetrics.Store(nil)
	if err := registerMetrics(prometheus.NewPedanticRegistry()); err != nil {
		t.Fatalf("initial registration: %v", err)
	}

	mc := newMetricsCollector()

	const readers = 8
	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					mc.recordRequest(true)
					mc.recordDeclinedRequest("zoneA", "k1")
					mc.recordProcessTimePerKey(time.Millisecond, "zoneA", "k1")
					mc.updateKeysCount("zoneA", 1)
				}
			}
		}()
	}

	// Simulate repeated reloads (each with a fresh registry) concurrent with the
	// in-flight requests above.
	for i := 0; i < 100; i++ {
		if err := registerMetrics(prometheus.NewPedanticRegistry()); err != nil {
			t.Errorf("reload %d: %v", i, err)
			break
		}
	}

	close(stop)
	wg.Wait()
}
