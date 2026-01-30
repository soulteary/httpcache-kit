package httpcache_test

import (
	"io"
	"log"
	"testing"

	"github.com/soulteary/httpcache-kit"
	logger "github.com/soulteary/logger-kit"
)

func TestSetLogger(t *testing.T) {
	l := logger.Default()
	httpcache.SetLogger(l)
	// Call with nil - should not panic and should not replace
	httpcache.SetLogger(nil)
	httpcache.SetLogger(l)
}

// TestDebugLogging triggers package-level debugf by enabling DebugLogging and making a request.
func TestDebugLogging(t *testing.T) {
	prev := httpcache.DebugLogging
	httpcache.DebugLogging = true
	defer func() { httpcache.DebugLogging = prev }()
	log.SetOutput(io.Discard)
	client, _ := testSetup()
	_ = client.get("/")
}
