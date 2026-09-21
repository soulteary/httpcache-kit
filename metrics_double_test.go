package httpcache

import "sync"

// countingMetrics records how often the cache reported each event.
//
// It embeds NopMetrics and overrides only what these tests assert on, which is
// the composition pattern the Metrics doc recommends: a recorder that does not
// implement a method added in a future release keeps compiling.
//
// It also stands in for any third-party implementation. Before the split the
// only way to exercise these paths was to build real Prometheus collectors, so
// the cache's own tests pulled in prometheus/client_golang; now the root
// package's tests need nothing outside the standard library.
type countingMetrics struct {
	NopMetrics

	mu        sync.Mutex
	evictions map[string]int
	cleanups  int
	stats     []CacheStats
}

func newCountingMetrics() *countingMetrics {
	return &countingMetrics{evictions: make(map[string]int)}
}

func (c *countingMetrics) RecordCacheEviction(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evictions[reason]++
}

func (c *countingMetrics) RecordCleanupDuration(float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cleanups++
}

func (c *countingMetrics) UpdateCacheStats(stats CacheStats) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats = append(c.stats, stats)
}

func (c *countingMetrics) evictionCount(reason string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.evictions[reason]
}

func (c *countingMetrics) cleanupCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cleanups
}

// Compile-time proof that embedding NopMetrics is enough to satisfy Metrics.
var _ Metrics = (*countingMetrics)(nil)
