package httpcache

import (
	"testing"
)

func TestPackageErrorf(t *testing.T) {
	// Package-level errorf is used by cache/resource; call it to cover logger.errorf
	errorf("test message %s", "value")
}

func TestPackageDebugf(t *testing.T) {
	// Package-level debugf only logs when IsDebugLogging is true
	prev := IsDebugLogging()
	SetDebugLogging(true)
	defer func() { SetDebugLogging(prev) }()
	debugf("test debug %s", "value")
}
