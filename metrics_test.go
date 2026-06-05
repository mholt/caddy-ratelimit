package caddyrl

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/caddyserver/caddy/v2/caddytest"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestMetrics(t *testing.T) {
	// Reset the metrics registry to ensure clean state
	prometheus.DefaultRegisterer = prometheus.NewRegistry()

	// Reset global metrics
	globalMetrics = nil

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
	if globalMetrics == nil {
		t.Fatal("Expected globalMetrics to be initialized")
	}

	// Test that configuration metrics are recorded
	configMetric := testutil.ToFloat64(globalMetrics.config.WithLabelValues("test_zone", strconv.Itoa(maxEvents), fmt.Sprintf("%ds", window), "0", "0"))
	if configMetric == 0 {
		t.Error("Expected configuration metric to be recorded")
	}

	// Make some requests that should be allowed
	for i := 0; i < maxEvents; i++ {
		tester.AssertGetResponse("http://localhost:8080", 200, "")
	}

	// Check request metrics - verify both zone-level and per-key metrics
	zoneLevelRequestsMetric := testutil.ToFloat64(globalMetrics.requestsTotal.WithLabelValues("test_zone", ""))
	perKeyRequestsMetric := testutil.ToFloat64(globalMetrics.requestsTotal.WithLabelValues("test_zone", "static"))

	if zoneLevelRequestsMetric < float64(maxEvents) {
		t.Errorf("Expected at least %d zone-level requests metric, got %f", maxEvents, zoneLevelRequestsMetric)
	}
	if perKeyRequestsMetric < float64(maxEvents) {
		t.Errorf("Expected at least %d per-key requests metric, got %f", maxEvents, perKeyRequestsMetric)
	}

	// Make a request that should be declined
	tester.AssertGetResponse("http://localhost:8080", 429, "")

	// Check declined requests metrics - verify both zone-level and per-key metrics
	zoneLevelDeclinedMetric := testutil.ToFloat64(globalMetrics.declinedTotal.WithLabelValues("test_zone", ""))
	perKeyDeclinedMetric := testutil.ToFloat64(globalMetrics.declinedTotal.WithLabelValues("test_zone", "static"))

	if zoneLevelDeclinedMetric == 0 {
		t.Error("Expected zone-level declined requests metric to be recorded")
	}
	if perKeyDeclinedMetric == 0 {
		t.Error("Expected per-key declined requests metric to be recorded")
	}

	// Check process time histograms - verify both zone-level and per-key metrics
	zoneLevelProcessTimeHistogram := globalMetrics.processTime.WithLabelValues("test_zone", "")
	perKeyProcessTimeHistogram := globalMetrics.processTime.WithLabelValues("test_zone", "static")

	if zoneLevelProcessTimeHistogram == nil {
		t.Error("Expected zone-level process time histogram to be created")
	}
	if perKeyProcessTimeHistogram == nil {
		t.Error("Expected per-key process time histogram to be created")
	}
}

// TestMetricsReRegisterOnReload checks that after a reload (a fresh registry) the
// collectors are still reported by /metrics, and keep their values rather than
// resetting (#100).
func TestMetricsReRegisterOnReload(t *testing.T) {
	globalMetrics = nil

	reg1 := prometheus.NewPedanticRegistry()
	if err := registerMetrics(reg1); err != nil {
		t.Fatalf("initial registration: %v", err)
	}

	mc := newMetricsCollector()
	mc.recordDeclinedRequest("zoneA", "k1") // value 1, registered in reg1

	// Reload: Caddy hands out a brand-new registry and points /metrics at it.
	reg2 := prometheus.NewPedanticRegistry()
	if err := registerMetrics(reg2); err != nil {
		t.Fatalf("reload registration: %v", err)
	}
	mc.recordDeclinedRequest("zoneA", "k1") // value 2, still the same collectors

	// The new registry must report the collectors (before the fix it had none).
	got, err := testutil.GatherAndCount(reg2, "caddy_rate_limit_declined_requests_total")
	if err != nil {
		t.Fatalf("gathering reg2: %v", err)
	}
	if got != 2 { // zone aggregate + per-key series
		t.Fatalf("expected 2 declined series in the post-reload registry, got %d", got)
	}

	// ...and the value carries across the reload instead of resetting.
	if v := testutil.ToFloat64(globalMetrics.declinedTotal.WithLabelValues("zoneA", "k1")); v != 2 {
		t.Errorf("expected per-key declined counter = 2 after reload, got %v", v)
	}
}
