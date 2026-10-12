package httpcache

import (
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"sync"
)

// StageKind identifies a distinct temporary copy. Persistent admitted entries
// remain governed by CacheConfig.MaxSize rather than a staging budget.
type StageKind string

const (
	StageHandlerBody StageKind = "handler-body"
	StageCacheBody   StageKind = "cache-body"
	StageCacheHeader StageKind = "cache-header"
)

// ErrStagingBudgetExceeded identifies refused temporary storage admission.
var ErrStagingBudgetExceeded = errors.New("staging budget exceeded")

// StagingBudget admits temporary storage without blocking. A single instance
// may be injected into both HandlerOptions and CacheConfig to count overlapping
// copies, and implemented by an application that also accounts for its spools.
type StagingBudget interface {
	NewLease(context.Context, StageKind) (StagingLease, error)
}

// StagingLease accounts for one temporary copy. Reserve must be nonblocking,
// thread-safe and all-or-nothing; Release returns unused reserved bytes. Close
// releases all retained bytes and must be idempotent. A named file's lease is
// retained until publication or successful deletion, not merely file closure.
type StagingLease interface {
	Reserve(int64) error
	Release(int64)
	Close()
}

func newStageLease(ctx context.Context, budget StagingBudget, kind StageKind) (StagingLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if budget == nil {
		return nil, nil
	}
	lease, err := budget.NewLease(ctx, kind)
	if err != nil {
		closeStageLease(lease)
		return nil, errors.Join(ErrStagingBudgetExceeded, err)
	}
	if lease == nil {
		return nil, errors.New("staging budget returned a nil lease")
	}
	return lease, nil
}

func closeStageLease(lease StagingLease) {
	if lease != nil {
		lease.Close()
	}
}

type stagingWriter struct {
	ctx   context.Context
	w     io.Writer
	lease StagingLease
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func (w *stagingWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if w.lease != nil {
		if err := w.lease.Reserve(int64(len(p))); err != nil {
			return 0, errors.Join(ErrStagingBudgetExceeded, err)
		}
	}
	n, err := w.w.Write(p)
	if w.lease != nil && n < len(p) {
		w.lease.Release(int64(len(p) - n))
	}
	return n, err
}

// handlerStage keeps one descriptor open from streaming through background
// storage. POSIX unlink avoids crash orphans and reopening a named pathname.
type handlerStage struct {
	*os.File
	lease StagingLease
	path  string // nonempty only on systems that cannot unlink an open file
	once  sync.Once
	err   error
}

func newHandlerStage(ctx context.Context, dir string, budget StagingBudget) (*handlerStage, error) {
	lease, err := newStageLease(ctx, budget, StageHandlerBody)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, "httpcache-*.tmp")
	if err != nil {
		closeStageLease(lease)
		return nil, err
	}
	stage := &handlerStage{File: f, lease: lease, path: f.Name()}
	if runtime.GOOS != "windows" {
		if err := os.Remove(stage.path); err != nil {
			_ = stage.Close()
			return nil, err
		}
		stage.path = ""
	}
	return stage, nil
}

func (s *handlerStage) Close() error {
	s.once.Do(func() {
		s.err = s.File.Close()
		if s.path != "" {
			if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
				s.err = errors.Join(s.err, err)
				return // Unremoved disk bytes remain charged.
			}
		}
		closeStageLease(s.lease)
	})
	return s.err
}

// ContextStore is an optional Cache extension. Handler passes ownership values
// from a miss lease through background Store without inheriting client exit.
// Implementations must independently bound backend I/O when necessary.
type ContextStore interface {
	StoreContext(context.Context, *Resource, ...string) error
}
