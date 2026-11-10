package captain // import "github.com/harbur/captain"

import (
	"context"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsouza/go-dockerclient"
	"github.com/stretchr/testify/assert"
)

// fakeDockerHandler answers the version probe that go-dockerclient
// performs before the first request, then delegates build traffic.
func fakeDockerHandler(t *testing.T, buildHandler http.HandlerFunc) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ApiVersion":"1.30"}`))
			return
		}
		buildHandler(w, r)
	}
}

// newUnixSocketServer exposes an HTTP server over a Unix domain socket,
// which is the exact transport (stream()'s unixProtocol branch) that
// go-dockerclient uses against a real Docker daemon.
func newUnixSocketServer(t *testing.T, handler http.Handler) (string, func()) {
	t.Helper()

	dir, err := ioutil.TempDir("", "captain-docker")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	socket := filepath.Join(dir, "docker.sock")

	listener, err := net.Listen("unix", socket)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("listen unix: %v", err)
	}

	srv := &http.Server{Handler: handler}
	go srv.Serve(listener)

	cleanup := func() {
		srv.Close()
		os.RemoveAll(dir)
	}
	return socket, cleanup
}

// A streaming Docker build that never finishes must be unblocked by
// cancelling the shared lifecycle: go-dockerclient closes the underlying
// socket connection and BuildImage returns context.Canceled instead of
// hanging on the build output stream.
func TestBuildImageCtxCancellationEndsStreamWait(t *testing.T) {
	requestStarted := make(chan struct{})
	socket, cleanup := newUnixSocketServer(t, fakeDockerHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(requestStarted)
		// Simulate a long-running build streaming output that never
		// completes on its own.
		<-r.Context().Done()
	}))
	defer cleanup()

	c, err := docker.NewClient("unix://" + socket)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	c.SkipServerVersionCheck = true

	app := App{Build: basedir + "/test/scratch/Dockerfile", Image: "captain_cancel_stream"}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- buildImageWithClient(ctx, c, app, "latest", false)
	}()

	select {
	case <-requestStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("fake build endpoint was never reached")
	}

	cancel()

	select {
	case buildErr := <-done:
		assert.Equal(t, context.Canceled, buildErr,
			"cancelled streaming build should report context.Canceled")
	case <-time.After(3 * time.Second):
		t.Fatal("streaming build wait was not ended by cancellation")
	}
}

// Ordinary Docker error responses are still surfaced, proving the
// cancellation plumbing did not change the normal failure path.
func TestBuildImageCtxSurfacesServerError(t *testing.T) {
	socket, cleanup := newUnixSocketServer(t, fakeDockerHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer cleanup()

	c, err := docker.NewClient("unix://" + socket)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	c.SkipServerVersionCheck = true

	app := App{Build: basedir + "/test/scratch/Dockerfile", Image: "captain_cancel_error"}

	done := make(chan error, 1)
	go func() {
		done <- buildImageWithClient(context.Background(), c, app, "latest", false)
	}()

	select {
	case buildErr := <-done:
		assert.NotNil(t, buildErr, "server error must be returned to the caller")
		assert.NotEqual(t, context.Canceled, buildErr)
	case <-time.After(3 * time.Second):
		t.Fatal("failing build did not return")
	}
}
