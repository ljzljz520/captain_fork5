package captain // import "github.com/harbur/captain"

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// A fresh lifecycle behaves as an ordinary context: every checkpoint and
// every successful step passes through unchanged.
func TestLifecycleCleanRun(t *testing.T) {
	lc := NewLifecycle(context.Background(), 0)

	assert.Nil(t, lc.Checkpoint(), "checkpoint passes while running")
	assert.Nil(t, lc.Finish(nil), "success passes through while running")
	assert.False(t, lc.Cancelled(), "lifecycle is not cancelled")

	ordinary := errors.New("step failed on its own")
	assert.Equal(t, ordinary, lc.Finish(ordinary), "non-cancel errors are preserved")
}

// Cancel is sticky and idempotent: only the first caller performs the
// transition, and afterwards every checkpoint/Finish reports cancellation.
func TestLifecycleCancelIsSticky(t *testing.T) {
	lc := NewLifecycle(context.Background(), 0)

	assert.True(t, lc.Cancel(), "first cancel wins")
	assert.False(t, lc.Cancel(), "further cancels are no-ops")
	assert.True(t, lc.Cancelled())
	assert.Equal(t, ErrCancelled, lc.Checkpoint())

	// A late success must not resurrect a cancelled run.
	assert.Equal(t, ErrCancelled, lc.Finish(nil), "late success is swallowed")
	// A late failure is also normalized to the cancellation cause.
	assert.Equal(t, ErrCancelled, lc.Finish(errors.New("late failure")))

	select {
	case <-lc.Done():
	case <-time.After(time.Second):
		t.Fatal("cancelled context was not closed")
	}
}

// A timeout drives the same cancellation path as a signal, including the
// sticky state, so both triggers are interchangeable for downstream code.
func TestLifecycleTimeoutCancels(t *testing.T) {
	lc := NewLifecycle(context.Background(), 50*time.Millisecond)

	select {
	case <-lc.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("deadline did not cancel the lifecycle")
	}

	deadline := time.Now().Add(2 * time.Second)
	for !lc.Cancelled() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	assert.True(t, lc.Cancelled(), "deadline flips the lifecycle to cancelled")
	assert.Equal(t, ErrCancelled, lc.Checkpoint())
}

// Hammer Cancel and Finish concurrently. Whatever the interleaving, once
// cancellation has landed no subsequent Finish may ever report success:
// this is the "cancel and success arrive together" guarantee.
func TestLifecycleCancelVsSuccessRace(t *testing.T) {
	const rounds = 200
	for i := 0; i < rounds; i++ {
		lc := NewLifecycle(context.Background(), 0)
		var wg sync.WaitGroup

		wg.Add(2)
		go func() {
			defer wg.Done()
			lc.Cancel()
		}()
		go func() {
			defer wg.Done()
			// Simulate a step returning success at the same instant.
			lc.Finish(nil)
		}()
		wg.Wait()

		assert.True(t, lc.Cancelled(), "round %d: cancellation must be visible", i)
		assert.Equal(t, ErrCancelled, lc.Finish(nil),
			"round %d: no success can be observed after the cancel CAS", i)
	}
}

// BuildOptions without an explicit lifecycle keep the historical inert
// behavior; an explicit lifecycle is shared verbatim across stages.
func TestBuildOptionsLifecycleFallback(t *testing.T) {
	assert.Equal(t, backgroundLifecycle, (BuildOptions{}).lifecycle(),
		"missing lifecycle falls back to the inert background one")

	lc := NewLifecycle(context.Background(), 0)
	opts := BuildOptions{Lifecycle: lc}
	assert.Equal(t, lc, opts.lifecycle(), "explicit lifecycle is reused as-is")
}
