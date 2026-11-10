//go:build !windows
// +build !windows

package captain // import "github.com/harbur/captain"

import (
	"context"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// processGone polls until pid no longer exists (kill -9 style probe with
// signal 0). ESRCH means the kernel has fully removed the process, i.e.
// the grandchild was signalled and reaped rather than orphaned.
func processGone(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d still alive %s after cancellation", pid, timeout)
}

func readGrandchildPID(t *testing.T, marker string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		raw, err := ioutil.ReadFile(marker)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("grandchild did not record its PID in %s", marker)
	return -1
}

// Ordinary commands keep their historical behavior: no context
// cancellation means a clean exit and a nil error.
func TestExecuteCtxNormalCompletion(t *testing.T) {
	assert.Nil(t, executeCtx(context.Background(), "echo", "hello"))

	runErr := executeCtx(context.Background(), "bash", "-c", "exit 7")
	assert.NotNil(t, runErr, "non-zero exits still surface as errors")
}

// Cancelling while a pre/test shell command runs must terminate the whole
// process group, including grandchild processes that the shell spawned.
func TestExecuteCtxCancelReapsGrandchildren(t *testing.T) {
	dir, err := ioutil.TempDir("", "captain-exec-cancel")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(dir)
	marker := filepath.Join(dir, "grandchild.pid")

	lc := NewLifecycle(context.Background(), 0)
	// sleep runs as a grandchild of captain (captain -> bash -> sleep).
	script := "sleep 60 & echo $! > " + marker + "; wait"

	done := make(chan error, 1)
	go func() {
		done <- executeCtx(lc.Context(), "bash", "-c", script)
	}()

	grandchild := readGrandchildPID(t, marker, 2*time.Second)

	lc.Cancel()

	select {
	case runErr := <-done:
		assert.Equal(t, ErrCancelled, runErr, "cancelled command reports ErrCancelled")
	case <-time.After(10 * time.Second):
		t.Fatal("executeCtx did not return after cancellation")
	}

	processGone(t, grandchild, 3*time.Second)
}

// The timeout trigger must reach the process tree in exactly the same way
// a signal does.
func TestExecuteCtxTimeoutReapsGrandchildren(t *testing.T) {
	dir, err := ioutil.TempDir("", "captain-exec-timeout")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(dir)
	marker := filepath.Join(dir, "grandchild.pid")

	lc := NewLifecycle(context.Background(), 150*time.Millisecond)
	script := "sleep 60 & echo $! > " + marker + "; wait"

	runErr := executeCtx(lc.Context(), "bash", "-c", script)
	assert.Equal(t, ErrCancelled, runErr, "deadline expiration cancels the command")

	grandchild := readGrandchildPID(t, marker, 2*time.Second)
	processGone(t, grandchild, 3*time.Second)
}

// A lifecycle cancelled before the command starts must not spawn anything.
func TestExecuteCtxPreCancelledDoesNotStart(t *testing.T) {
	dir, err := ioutil.TempDir("", "captain-exec-precancel")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(dir)
	marker := filepath.Join(dir, "never-created")

	lc := NewLifecycle(context.Background(), 0)
	lc.Cancel()

	runErr := executeCtx(lc.Context(), "bash", "-c", "touch "+marker)
	assert.Equal(t, ErrCancelled, runErr)

	_, statErr := os.Stat(marker)
	assert.True(t, os.IsNotExist(statErr), "no command may run after cancellation")
}

// Processes that ignore SIGTERM are escalated to SIGKILL once the grace
// period elapses; the call still returns and the tree is removed.
func TestExecuteCtxForceKillsAfterGrace(t *testing.T) {
	dir, err := ioutil.TempDir("", "captain-exec-kill")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(dir)
	marker := filepath.Join(dir, "grandchild.pid")

	oldGrace := terminationGracePeriod
	terminationGracePeriod = 200 * time.Millisecond
	defer func() { terminationGracePeriod = oldGrace }()

	lc := NewLifecycle(context.Background(), 0)
	// bash explicitly ignores SIGTERM; only the SIGKILL escalation can stop it.
	script := "trap '' TERM; sleep 60 & echo $! > " + marker + "; wait"

	done := make(chan error, 1)
	go func() {
		done <- executeCtx(lc.Context(), "bash", "-c", script)
	}()

	grandchild := readGrandchildPID(t, marker, 2*time.Second)

	start := time.Now()
	lc.Cancel()

	select {
	case runErr := <-done:
		elapsed := time.Since(start)
		assert.Equal(t, ErrCancelled, runErr)
		assert.True(t, elapsed >= terminationGracePeriod,
			"should have waited the grace period before SIGKILL, took %s", elapsed)
	case <-time.After(10 * time.Second):
		t.Fatal("stuck process tree was not force-killed")
	}

	processGone(t, grandchild, 3*time.Second)
}
