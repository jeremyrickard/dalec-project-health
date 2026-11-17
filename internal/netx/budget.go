package netx

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrUpstreamBudget = fmt.Errorf("upstream working budget exhausted: %w", context.DeadlineExceeded)
	ErrRunDeadline    = fmt.Errorf("generation/run deadline exhausted: %w", context.DeadlineExceeded)
)

type budgetKey struct{}

type workBudget struct {
	mu        sync.Mutex
	remaining time.Duration
	started   time.Time
	paused    int
	stopped   bool
	timer     *time.Timer
	cancel    context.CancelCauseFunc
}

// WithWorkingBudget excludes coordinated rate/queue waits, not request attempts
// or transient retries. The parent run deadline always remains authoritative.
func WithWorkingBudget(parent context.Context, duration time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	b := &workBudget{remaining: duration, started: time.Now(), cancel: cancel}
	b.timer = time.AfterFunc(duration, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if !b.stopped && b.paused == 0 && time.Since(b.started) >= b.remaining {
			b.cancel(ErrUpstreamBudget)
		}
	})
	return context.WithValue(ctx, budgetKey{}, b), func() {
		b.mu.Lock()
		b.stopped = true
		b.timer.Stop()
		b.mu.Unlock()
		cancel(nil)
	}
}

func pauseWork(ctx context.Context) func() {
	b, _ := ctx.Value(budgetKey{}).(*workBudget)
	if b == nil {
		return func() {}
	}
	b.mu.Lock()
	if !b.stopped && b.paused == 0 {
		b.remaining -= time.Since(b.started)
		b.timer.Stop()
		if b.remaining <= 0 {
			b.cancel(ErrUpstreamBudget)
		}
	}
	b.paused++
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.paused--
		if !b.stopped && b.paused == 0 && ctx.Err() == nil {
			b.started = time.Now()
			b.timer.Reset(max(b.remaining, 0))
		}
	}
}

func ContextError(ctx context.Context) error {
	if errors.Is(context.Cause(ctx), ErrUpstreamBudget) {
		return ErrUpstreamBudget
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(context.Cause(ctx), ErrRunDeadline) {
		return ErrRunDeadline
	}
	return context.Canceled
}
