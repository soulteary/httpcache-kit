package httpcache

import (
	"sync/atomic"
)

// Metrics is what the cache records through. It is an interface, and the root
// package deliberately provides no implementation that talks to a metrics
// backend: the Prometheus one lives in the prometheusmetrics subpackage, so a
// service that does not export metrics never links Prometheus.
//
// Implement it yourself to record into something else -- OpenTelemetry, statsd,
// a test double, an expvar map. Embed [NopMetrics] to pick up no-op
// implementations of the methods you do not care about; new methods added in a
// future minor release will then not break your type.
type Metrics interface {
	// RecordCacheHit records a cache hit for an HTTP method.
	RecordCacheHit(method string)
	// RecordCacheMiss records a cache miss for an HTTP method.
	RecordCacheMiss(method string)
	// RecordCacheSkip records a request that was not cacheable at all.
	RecordCacheSkip()
	// RecordUpstreamDuration records how long an upstream request took.
	RecordUpstreamDuration(method string, status int, durationSeconds float64)
	// RecordUpstreamError records an upstream failure by kind.
	RecordUpstreamError(errorType string)
	// RecordStoreOperation records an attempt to write an entry to the cache.
	RecordStoreOperation(success bool)
	// RecordRetrieveOperation records an attempt to read an entry from the cache.
	RecordRetrieveOperation(found bool)
	// SetCacheSize sets the current total size of cached bodies in bytes.
	SetCacheSize(sizeBytes int64)
	// SetCacheItemCount sets the current number of cached items.
	SetCacheItemCount(count int)
	// SetCacheStaleCount sets the current number of stale map entries.
	SetCacheStaleCount(count int)
	// RecordCacheEviction records one eviction, labelled by why it happened.
	RecordCacheEviction(reason string)
	// RecordCleanupDuration records how long a cleanup pass took.
	RecordCleanupDuration(durationSeconds float64)
	// UpdateCacheStats sets every gauge from a stats snapshot.
	UpdateCacheStats(stats CacheStats)
}

// NopMetrics discards everything recorded through it. It is the default, so
// the cache never has to test for a missing recorder before recording -- and
// so no call site can panic on a nil one.
type NopMetrics struct{}

// Compile-time proof that the no-op really does satisfy the interface, which
// is what makes it safe as the default.
var _ Metrics = NopMetrics{}

func (NopMetrics) RecordCacheHit(string)                       {}
func (NopMetrics) RecordCacheMiss(string)                      {}
func (NopMetrics) RecordCacheSkip()                            {}
func (NopMetrics) RecordUpstreamDuration(string, int, float64) {}
func (NopMetrics) RecordUpstreamError(string)                  {}
func (NopMetrics) RecordStoreOperation(bool)                   {}
func (NopMetrics) RecordRetrieveOperation(bool)                {}
func (NopMetrics) SetCacheSize(int64)                          {}
func (NopMetrics) SetCacheItemCount(int)                       {}
func (NopMetrics) SetCacheStaleCount(int)                      {}
func (NopMetrics) RecordCacheEviction(string)                  {}
func (NopMetrics) RecordCleanupDuration(float64)               {}
func (NopMetrics) UpdateCacheStats(CacheStats)                 {}

// defaultMetrics holds the process-wide recorder. Accessed only through
// getDefaultMetrics/SetDefaultMetrics to avoid data races; wrapped in a struct
// because atomic.Value.Store(nil) panics.
var defaultMetrics atomic.Value

type defaultMetricsHolder struct{ m Metrics }

// getDefaultMetrics returns the current recorder, never nil.
//
// Returning [NopMetrics] rather than nil is what lets every call site record
// unconditionally. With an interface, a nil default is not the harmless
// no-op a nil *CacheMetrics used to be -- calling a method on a nil interface
// panics -- so "unset" has to be a real object.
func getDefaultMetrics() Metrics {
	v, ok := defaultMetrics.Load().(defaultMetricsHolder)
	if !ok || v.m == nil {
		return NopMetrics{}
	}
	return v.m
}

// GetDefaultMetrics returns the current recorder. It never returns nil: until
// something calls [SetDefaultMetrics], it is a [NopMetrics].
func GetDefaultMetrics() Metrics {
	return getDefaultMetrics()
}

// SetDefaultMetrics installs the process-wide recorder. Passing nil restores
// [NopMetrics] rather than arming a nil that would panic on first use.
//
// prometheusmetrics.New calls this for you.
func SetDefaultMetrics(m Metrics) {
	if m == nil {
		m = NopMetrics{}
	}
	defaultMetrics.Store(defaultMetricsHolder{m: m})
}
