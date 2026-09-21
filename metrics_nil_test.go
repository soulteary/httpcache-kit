package httpcache

import "testing"

// A Handler whose metrics field was never assigned must record into a no-op,
// not panic. Before the split the field was a *CacheMetrics whose nil-receiver
// methods were harmless; an unset interface is not, so metricsRef covers it.
func TestZeroValueHandlerDoesNotPanicOnRecord(t *testing.T) {
	var h Handler
	if h.metrics != nil {
		t.Fatal("premise wrong: the zero value should have a nil metrics field")
	}
	h.metricsRef().RecordCacheHit("GET")
	h.metricsRef().RecordCacheSkip()
	h.metricsRef().RecordStoreOperation(true)
}

// SetMetrics(nil) must install the no-op rather than arming a nil interface.
func TestSetMetricsNilInstallsNop(t *testing.T) {
	h := NewHandler(NewMemoryCache(), nil)
	h.SetMetrics(nil)
	if h.metrics == nil {
		t.Fatal("SetMetrics(nil) left a nil interface behind")
	}
	h.metricsRef().RecordCacheHit("GET")
}
