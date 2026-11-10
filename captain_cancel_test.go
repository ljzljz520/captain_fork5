package captain // import "github.com/harbur/captain"

import (
	"context"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func ymlConfig(t *testing.T, body string) Config {
	t.Helper()
	return unmarshal([]byte(body))
}

// A cancelled lifecycle prevents test commands from running at all: the
// stage checkpoint is hit before any shell is spawned.
func TestRunTestBlockedByCancellation(t *testing.T) {
	dir, err := ioutil.TempDir("", "captain-test-gate")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(dir)
	marker := filepath.Join(dir, "should-not-exist")

	cfg := ymlConfig(t, "app:\n  image: captain_cancel_test\n  test:\n    - touch "+marker+"\n")

	lc := NewLifecycle(context.Background(), 0)
	lc.Cancel()

	runErr := runTest(BuildOptions{Config: cfg, Lifecycle: lc})
	assert.Equal(t, ErrCancelled, runErr)

	_, statErr := os.Stat(marker)
	assert.True(t, os.IsNotExist(statErr), "no test command may run after cancellation")
}

// Cancellation landing while a test command is in flight ends the command
// tree and propagates ErrCancelled out of the stage.
func TestRunTestCancelledMidCommand(t *testing.T) {
	cfg := ymlConfig(t, "app:\n  image: captain_cancel_test\n  test:\n    - sleep 30\n")

	lc := NewLifecycle(context.Background(), 0)
	go func() {
		time.Sleep(150 * time.Millisecond)
		lc.Cancel()
	}()

	start := time.Now()
	runErr := runTest(BuildOptions{Config: cfg, Lifecycle: lc})
	assert.Equal(t, ErrCancelled, runErr)
	assert.True(t, time.Since(start) < 10*time.Second,
		"cancelled test stage must return promptly instead of waiting for sleep")
}

// Cancellation during pre aborts the build before any Docker build, tag or
// post command happens; a late success cannot resume the flow.
func TestRunBuildCancelledDuringPreSkipsBuildTagAndPost(t *testing.T) {
	dir, err := ioutil.TempDir("", "captain-build-gate")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(dir)
	postMarker := filepath.Join(dir, "post-ran")

	body := "app:\n" +
		"  image: captain_cancel_build\n" +
		"  pre:\n    - sleep 30\n" +
		"  post:\n    - touch " + postMarker + "\n"
	cfg := ymlConfig(t, body)

	lc := NewLifecycle(context.Background(), 0)
	go func() {
		time.Sleep(150 * time.Millisecond)
		lc.Cancel()
	}()

	output := captureStdout(t, func() {
		err = runBuild(BuildOptions{Config: cfg, Lifecycle: lc})
	})

	assert.Equal(t, ErrCancelled, err)
	assert.NotContains(t, output, "Building image", "no docker build may start after cancellation")
	assert.NotContains(t, output, "Tagging image", "no tagging may happen after cancellation")
	assert.NotContains(t, output, "Running post command", "ordinary post commands are skipped after cancellation")

	_, statErr := os.Stat(postMarker)
	assert.True(t, os.IsNotExist(statErr), "post command must not execute after cancellation")
}

// Tagging helpers short-circuit on a cancelled lifecycle without touching
// the Docker daemon.
func TestTagHelpersHonourCancellation(t *testing.T) {
	lc := NewLifecycle(context.Background(), 0)
	lc.Cancel()
	app := App{Image: "captain_cancel_tag_does_not_exist"}

	start := time.Now()
	assert.Equal(t, ErrCancelled, tagSwallow(lc, app, "latest", "x"))
	assert.Equal(t, ErrCancelled, tagStrict(lc, app, "latest", "x"))
	assert.True(t, time.Since(start) < 2*time.Second,
		"cancelled tags must return at the checkpoint without any Docker request")
}

// Push must not be initiated after cancellation.
func TestPushOneBlockedByCancellation(t *testing.T) {
	lc := NewLifecycle(context.Background(), 0)
	lc.Cancel()

	start := time.Now()
	runErr := pushOne(lc, "captain_cancel_push_does_not_exist", "latest")
	assert.Equal(t, ErrCancelled, runErr)
	assert.True(t, time.Since(start) < 2*time.Second,
		"cancelled push must short-circuit before invoking docker")
}

// A run with a live, never-cancelled lifecycle preserves the normal happy
// path: the image is really built and tagged.
func TestRunBuildNormalFlowWithLiveLifecycle(t *testing.T) {
	image := "captain_test_lifecycle_happy"
	cfg := scratchConfig(image)
	lc := NewLifecycle(context.Background(), 0)

	runErr := runBuild(BuildOptions{Config: cfg, Lifecycle: lc})
	assert.Nil(t, runErr, "uncancelled build completes normally")
	assert.False(t, lc.Cancelled(), "successful run must not be marked cancelled")
	assert.True(t, imageExist(scratchApp(image), "latest"), "latest tag should exist after a normal build")
}

// Pre/Post public wrappers keep working standalone via the inert
// background lifecycle, matching their historical behavior.
func TestPrePostStandaloneUnchanged(t *testing.T) {
	assert.Nil(t, Pre(App{Pre: []string{"echo pre"}}))
	assert.Nil(t, Post(App{Post: []string{"echo post"}}))
	assert.NotNil(t, Pre(App{Pre: []string{"nonexistingPreCommand"}}))
}
