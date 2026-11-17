package netx

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWorkingBudgetPausesAndResumes(t *testing.T) {
	ctx, cancel := WithWorkingBudget(context.Background(), 100*time.Millisecond)
	defer cancel()
	resume := pauseWork(ctx)
	// Wall time in a rate wait must not consume the upstream working budget.
	time.Sleep(150 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatal("paused budget expired")
	}
	resume()
	select {
	case <-ctx.Done():
		if !errors.Is(ContextError(ctx), ErrUpstreamBudget) || !errors.Is(ContextError(ctx), context.DeadlineExceeded) {
			t.Fatalf("upstream deadline misidentified: %v", ContextError(ctx))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resumed budget never expired")
	}
}

func TestWorkingBudgetCannotPauseRunCancellation(t *testing.T) {
	parent, cancel := context.WithTimeoutCause(context.Background(), 20*time.Millisecond, ErrRunDeadline)
	defer cancel()
	ctx, stop := WithWorkingBudget(parent, time.Second)
	defer stop()
	resume := pauseWork(ctx)
	defer resume()
	<-ctx.Done()
	if !errors.Is(ContextError(ctx), ErrRunDeadline) {
		t.Fatalf("run deadline misidentified: %v", ContextError(ctx))
	}
	parent, cancel = context.WithCancel(context.Background())
	ctx, stop = WithWorkingBudget(parent, time.Second)
	defer stop()
	resume2 := pauseWork(ctx)
	defer resume2()
	cancel()
	<-ctx.Done()
	if !errors.Is(ContextError(ctx), context.Canceled) {
		t.Fatalf("explicit cancellation misidentified: %v", ContextError(ctx))
	}
}
