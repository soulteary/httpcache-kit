package httpcache

import (
	"testing"
)

func TestPackageErrorf(t *testing.T) {
	// Package-level errorf is used by cache/resource; call it to cover logger.errorf
	errorf("test message %s", "value")
}

func TestPackageDebugf(t *testing.T) {
	// Package-level debugf only logs when DebugLogging is true
	prev := DebugLogging
	DebugLogging = true
	defer func() { DebugLogging = prev }()
	debugf("test debug %s", "value")
}
