package captain // import "github.com/harbur/captain"

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

// ErrCancelled is returned by every stage once the shared lifecycle has
// been terminated by a signal or a timeout. It deliberately replaces any
// "late" result produced by work that was still finishing when the
// cancellation landed.
var ErrCancelled = errors.New("captain: run cancelled")

// Lifecycle states. Only the atomic transition running -> cancelled is
// allowed, which makes cancellation sticky and gives signal-vs-success
// races a single deterministic winner.
const (
	stateRunning   int32 = 0
	stateCancelled int32 = 1
)

// Lifecycle is the single cancellation scope of one captain invocation.
// Build, test and push stages share the same instance so that a
// termination signal (or a timeout) propagates through every stage,
// every Docker request and every shell process tree.
type Lifecycle struct {
	ctx    context.Context
	cancel context.CancelFunc

	// state is manipulated atomically (stateRunning/stateCancelled).
	state int32

	sigCh chan os.Signal
}

// NewLifecycle creates a lifecycle derived from parent. When timeout is
// greater than zero, reaching the deadline cancels the lifecycle exactly
// like a termination signal would.
func NewLifecycle(parent context.Context, timeout time.Duration) *Lifecycle {
	if parent == nil {
		parent = context.Background()
	}

	ctx, cancel := context.WithCancel(parent)
	lc := &Lifecycle{ctx: ctx}

	var cancelTimeout context.CancelFunc
	if timeout > 0 {
		ctx, cancelTimeout = context.WithTimeout(ctx, timeout)
		lc.ctx = ctx
	}
	lc.cancel = func() {
		if cancelTimeout != nil {
			cancelTimeout()
		}
		cancel()
	}

	if timeout > 0 {
		go lc.watchTimeout()
	}

	return lc
}

// watchTimeout flips the lifecycle into the cancelled state when the
// configured deadline is the reason the context ended.
func (l *Lifecycle) watchTimeout() {
	<-l.ctx.Done()
	if l.ctx.Err() == context.DeadlineExceeded {
		l.Cancel()
	}
}

// CatchSignals translates SIGINT/SIGTERM into lifecycle cancellation.
// A second signal while the run is already terminating restores the
// default OS behavior so users can force-quit a stuck shutdown.
func (l *Lifecycle) CatchSignals() *Lifecycle {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	l.sigCh = ch

	go func() {
		select {
		case <-ch:
			info("Termination signal received, cancelling ongoing work...")
			l.Cancel()
		case <-l.ctx.Done():
			return
		}

		select {
		case <-ch:
			signal.Reset(syscall.SIGINT, syscall.SIGTERM)
			if proc, err := os.FindProcess(os.Getpid()); err == nil {
				_ = proc.Signal(syscall.SIGTERM)
			}
		case <-l.ctx.Done():
		}
	}()

	return l
}

// Stop detaches the signal handler and releases the context. It does not
// flip the cancellation state, so a cleanly finished run is not reported
// as cancelled.
func (l *Lifecycle) Stop() {
	if l.sigCh != nil {
		signal.Stop(l.sigCh)
	}
	l.cancel()
}

// Cancel requests termination. It is safe to call concurrently and any
// number of times; only the first call performs the transition and
// cancels the shared context.
func (l *Lifecycle) Cancel() bool {
	if atomic.CompareAndSwapInt32(&l.state, stateRunning, stateCancelled) {
		l.cancel()
		return true
	}
	return false
}

// Cancelled reports whether termination was requested.
func (l *Lifecycle) Cancelled() bool {
	return atomic.LoadInt32(&l.state) == stateCancelled
}

// Context exposes the shared context, to be threaded into Docker
// requests and shell commands.
func (l *Lifecycle) Context() context.Context {
	return l.ctx
}

// Done mirrors the underlying context's Done channel.
func (l *Lifecycle) Done() <-chan struct{} {
	return l.ctx.Done()
}

// Checkpoint is consulted before a new stage or step starts. A cancelled
// run never enters another stage, so tagging, post commands and pushes
// cannot sneak in after termination.
func (l *Lifecycle) Checkpoint() error {
	if l.Cancelled() {
		return ErrCancelled
	}
	return nil
}

// Finish is consulted as soon as an in-flight step returns. Because the
// cancelled state is sticky and checked atomically, a success that
// arrives after cancellation cannot resume the pipeline.
func (l *Lifecycle) Finish(err error) error {
	if l.Cancelled() {
		return ErrCancelled
	}
	return err
}

// backgroundLifecycle is used by callers (and legacy entry points) that
// do not supply their own lifecycle. It is never cancelled and carries
// no signal handler, preserving the previous standalone behavior.
var backgroundLifecycle = NewLifecycle(context.Background(), 0)
