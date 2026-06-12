package caddyrl

import (
	"errors"
	"strconv"
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

// globalMetrics is a package-level singleton holding the collectors. The
// collectors are created once but registered with every config's registry:
// Caddy provisions a fresh metrics registry on each reload and points /metrics
// at it, so registerMetrics re-registers the existing collectors with the new
// registry to keep them reported (and their values intact) across reloads.
var globalMetrics *rateLimitMetrics

// newMetrics builds the rate limit collectors (without registering them).
func newMetrics() *rateLimitMetrics {
	const ns, sub = "caddy", "rate_limit"

	return &rateLimitMetrics{
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
}

// registerMetrics registers the rate limit collectors with reg. The collectors
// are created once and re-registered with each config's registry (Caddy makes a
// new one on every reload), so /metrics keeps reporting them after a reload. An
// AlreadyRegisteredError means another rate_limit handler already registered them
// with this registry, which is expected and fine.
func registerMetrics(reg prometheus.Registerer) error {
	if globalMetrics == nil {
		globalMetrics = newMetrics()
	}

	for _, c := range []prometheus.Collector{
		globalMetrics.declinedTotal,
		globalMetrics.requestsTotal,
		globalMetrics.processTime,
		globalMetrics.keysTotal,
		globalMetrics.config,
	} {
		var already prometheus.AlreadyRegisteredError
		if err := reg.Register(c); err != nil && !errors.As(err, &already) {
			return err
		}
	}
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
	if !mc.enabled || globalMetrics == nil {
		return
	}

	hasZoneStr := "false"
	if hasZone {
		hasZoneStr = "true"
	}
	// Record zone-level aggregate metric (key is empty for zone-level aggregation)
	globalMetrics.requestsTotal.WithLabelValues(hasZoneStr, "").Inc()
}

// recordRequestPerKey records a request for a specific zone and key
func (mc *metricsCollector) recordRequestPerKey(zone, key string) {
	if !mc.enabled || globalMetrics == nil {
		return
	}

	// Record both zone-level aggregate and per-key detailed metrics
	globalMetrics.requestsTotal.WithLabelValues(zone, "").Inc()  // Zone-level aggregate
	globalMetrics.requestsTotal.WithLabelValues(zone, key).Inc() // Per-key detailed
}

// recordDeclinedRequest records a request that was declined due to rate limiting
func (mc *metricsCollector) recordDeclinedRequest(zone, key string) {
	if !mc.enabled || globalMetrics == nil {
		return
	}

	// Record both zone-level aggregate and per-key detailed metrics
	globalMetrics.declinedTotal.WithLabelValues(zone, "").Inc()  // Zone-level aggregate
	globalMetrics.declinedTotal.WithLabelValues(zone, key).Inc() // Per-key detailed
}

// recordProcessTime records the time taken to process rate limiting
func (mc *metricsCollector) recordProcessTime(duration time.Duration, hasZone bool) {
	if !mc.enabled || globalMetrics == nil {
		return
	}

	hasZoneStr := "false"
	if hasZone {
		hasZoneStr = "true"
	}
	// Record zone-level aggregate metric (key is empty for zone-level aggregation)
	globalMetrics.processTime.WithLabelValues(hasZoneStr, "").Observe(duration.Seconds())
}

// recordProcessTimePerKey records the time taken to process rate limiting for a specific zone and key
func (mc *metricsCollector) recordProcessTimePerKey(duration time.Duration, zone, key string) {
	if !mc.enabled || globalMetrics == nil {
		return
	}

	// Record both zone-level aggregate and per-key detailed metrics
	globalMetrics.processTime.WithLabelValues(zone, "").Observe(duration.Seconds())  // Zone-level aggregate
	globalMetrics.processTime.WithLabelValues(zone, key).Observe(duration.Seconds()) // Per-key detailed
}

// updateKeysCount updates the count of keys for a specific zone
func (mc *metricsCollector) updateKeysCount(zone string, count int) {
	if !mc.enabled || globalMetrics == nil {
		return
	}

	globalMetrics.keysTotal.WithLabelValues(zone).Set(float64(count))
}

// recordConfig records the configuration of a rate limit zone (called once during provision)
func (mc *metricsCollector) recordConfig(zone string, maxEvents int, window time.Duration, ipv4Prefix, ipv6Prefix int) {
	if !mc.enabled || globalMetrics == nil {
		return
	}

	globalMetrics.config.WithLabelValues(zone,
		strconv.Itoa(maxEvents),
		window.String(),
		strconv.Itoa(ipv4Prefix),
		strconv.Itoa(ipv6Prefix)).Inc()
}
