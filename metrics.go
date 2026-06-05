package caddyrl

import (
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// rateLimitMetrics holds all the rate limit metrics
type rateLimitMetrics struct {
	declinedTotal *prometheus.CounterVec
	requestsTotal *prometheus.CounterVec
	processTime   *prometheus.HistogramVec
	keysTotal     *prometheus.GaugeVec
	config        *prometheus.CounterVec
}

// globalMetrics holds the currently-active rate limit collectors.
//
// It is stored atomically because Caddy provisions a brand-new metrics registry
// on every config reload (caddy.Context.GetMetricsRegistry returns a fresh
// prometheus registry per context) and rebinds the admin /metrics endpoint to
// it, while the previous config's in-flight request goroutines may still be
// reading these collectors. A plain pointer set only once would both data-race
// against those readers and keep pointing at collectors registered with the old,
// orphaned registry — so /metrics, which serves the new registry, would report
// no rate-limit activity after any reload. See issue #100.
var globalMetrics atomic.Pointer[rateLimitMetrics]

// register registers c with reg, returning the already-registered collector of
// the same definition when one exists. Caddy hands every rate_limit handler the
// same registry within a single config (so sibling handlers register identical
// collectors) and a fresh registry on each reload; Prometheus reports an existing
// collector as an AlreadyRegisteredError carrying the ExistingCollector. Reusing
// it ensures every handler — and the served /metrics endpoint — increments one
// shared collector instead of an orphaned duplicate. See issue #100.
func register[T prometheus.Collector](reg prometheus.Registerer, c T) (T, error) {
	if err := reg.Register(c); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			if existing, ok := already.ExistingCollector.(T); ok {
				return existing, nil
			}
			return c, fmt.Errorf("already-registered rate limit metric has unexpected type %T", already.ExistingCollector)
		}
		return c, err
	}
	return c, nil
}

// initializeMetrics creates the rate limit collectors and registers them with the
// provided registry, reconciling against any collectors already registered (by a
// sibling handler sharing the registry). The returned struct always references the
// collectors that are actually registered with reg.
func initializeMetrics(reg prometheus.Registerer) (*rateLimitMetrics, error) {
	const ns, sub = "caddy", "rate_limit"

	metrics := &rateLimitMetrics{
		// rate_limit_declined_requests_total - Total number of requests declined with HTTP 429
		declinedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: ns,
				Subsystem: sub,
				Name:      "declined_requests_total",
				Help:      "Total number of requests for which rate limit was applied (Declined with HTTP 429 status code returned).",
			},
			[]string{"zone", "key"},
		),

		// rate_limit_requests_total - Total number of requests that passed through the Rate Limit module
		requestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: ns,
				Subsystem: sub,
				Name:      "requests_total",
				Help:      "Total number of requests that passed through Rate Limit module (both declined & processed).",
			},
			[]string{"zone", "key"},
		),

		// rate_limit_process_time_seconds - Time taken to process rate limiting for each request
		processTime: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: ns,
				Subsystem: sub,
				Name:      "process_time_seconds",
				Help:      "A time taken to process rate limiting for each request.",
				Buckets:   []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1},
			},
			[]string{"zone", "key"},
		),

		// rate_limit_keys_total - Total number of keys that each RL zone contains
		keysTotal: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: ns,
				Subsystem: sub,
				Name:      "keys_total",
				Help:      "Total number of keys that each RL zone contains. (This metric is collected in the background for each zone.)",
			},
			[]string{"zone"},
		),

		// rate_limit_config - Shows configuration of the rate limiter module
		config: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: ns,
				Subsystem: sub,
				Name:      "config",
				Help:      "Shows configuration of the rate limiter module. Reported only once on bootstrap as configuration is not dynamically configurable.",
			},
			[]string{"zone", "max_events", "window", "ipv4_prefix", "ipv6_prefix"},
		),
	}

	var err error
	if metrics.declinedTotal, err = register(reg, metrics.declinedTotal); err != nil {
		return nil, err
	}
	if metrics.requestsTotal, err = register(reg, metrics.requestsTotal); err != nil {
		return nil, err
	}
	if metrics.processTime, err = register(reg, metrics.processTime); err != nil {
		return nil, err
	}
	if metrics.keysTotal, err = register(reg, metrics.keysTotal); err != nil {
		return nil, err
	}
	if metrics.config, err = register(reg, metrics.config); err != nil {
		return nil, err
	}
	return metrics, nil
}

// registerMetrics builds the rate limit collectors against reg — which Caddy
// replaces with a fresh registry on every config reload — and atomically
// publishes them so the record* methods (and thus the /metrics endpoint) observe
// the collectors registered with the currently-served registry. See issue #100.
func registerMetrics(reg prometheus.Registerer) error {
	metrics, err := initializeMetrics(reg)
	if err != nil {
		return err
	}
	globalMetrics.Store(metrics)
	return nil
}

// metricsCollector holds the metrics collection methods
type metricsCollector struct {
	enabled bool
}

// newMetricsCollector creates a new metrics collector
func newMetricsCollector() *metricsCollector {
	return &metricsCollector{enabled: true}
}

// recordRequest records a request that passed through the rate limit module
func (mc *metricsCollector) recordRequest(hasZone bool) {
	m := globalMetrics.Load()
	if !mc.enabled || m == nil {
		return
	}

	hasZoneStr := "false"
	if hasZone {
		hasZoneStr = "true"
	}
	// Record zone-level aggregate metric (key is empty for zone-level aggregation)
	m.requestsTotal.WithLabelValues(hasZoneStr, "").Inc()
}

// recordRequestPerKey records a request for a specific zone and key
func (mc *metricsCollector) recordRequestPerKey(zone, key string) {
	m := globalMetrics.Load()
	if !mc.enabled || m == nil {
		return
	}

	// Record both zone-level aggregate and per-key detailed metrics
	m.requestsTotal.WithLabelValues(zone, "").Inc()  // Zone-level aggregate
	m.requestsTotal.WithLabelValues(zone, key).Inc() // Per-key detailed
}

// recordDeclinedRequest records a request that was declined due to rate limiting
func (mc *metricsCollector) recordDeclinedRequest(zone, key string) {
	m := globalMetrics.Load()
	if !mc.enabled || m == nil {
		return
	}

	// Record both zone-level aggregate and per-key detailed metrics
	m.declinedTotal.WithLabelValues(zone, "").Inc()  // Zone-level aggregate
	m.declinedTotal.WithLabelValues(zone, key).Inc() // Per-key detailed
}

// recordProcessTime records the time taken to process rate limiting
func (mc *metricsCollector) recordProcessTime(duration time.Duration, hasZone bool) {
	m := globalMetrics.Load()
	if !mc.enabled || m == nil {
		return
	}

	hasZoneStr := "false"
	if hasZone {
		hasZoneStr = "true"
	}
	// Record zone-level aggregate metric (key is empty for zone-level aggregation)
	m.processTime.WithLabelValues(hasZoneStr, "").Observe(duration.Seconds())
}

// recordProcessTimePerKey records the time taken to process rate limiting for a specific zone and key
func (mc *metricsCollector) recordProcessTimePerKey(duration time.Duration, zone, key string) {
	m := globalMetrics.Load()
	if !mc.enabled || m == nil {
		return
	}

	// Record both zone-level aggregate and per-key detailed metrics
	m.processTime.WithLabelValues(zone, "").Observe(duration.Seconds())  // Zone-level aggregate
	m.processTime.WithLabelValues(zone, key).Observe(duration.Seconds()) // Per-key detailed
}

// updateKeysCount updates the count of keys for a specific zone
func (mc *metricsCollector) updateKeysCount(zone string, count int) {
	m := globalMetrics.Load()
	if !mc.enabled || m == nil {
		return
	}

	m.keysTotal.WithLabelValues(zone).Set(float64(count))
}

// recordConfig records the configuration of a rate limit zone (called once during provision)
func (mc *metricsCollector) recordConfig(zone string, maxEvents int, window time.Duration, ipv4Prefix, ipv6Prefix int) {
	m := globalMetrics.Load()
	if !mc.enabled || m == nil {
		return
	}

	m.config.WithLabelValues(zone,
		strconv.Itoa(maxEvents),
		window.String(),
		strconv.Itoa(ipv4Prefix),
		strconv.Itoa(ipv6Prefix)).Inc()
}
