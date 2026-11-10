package captain // import "github.com/harbur/captain"

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// terminationGracePeriod is how long a cancelled command tree gets to
// react to the graceful termination signal before the whole process
// group is force-killed. It is a variable so tests can shorten it.
var terminationGracePeriod = 5 * time.Second

func execute(name string, arg ...string) error {
	return executeCtx(context.Background(), name, arg...)
}

// executeCtx runs name with arg..., binding the whole shell process tree
// to ctx. When ctx is cancelled (signal or timeout):
//
//   - the child's own process group is asked to terminate gracefully,
//   - descendants that ignore the request are force-killed together,
//   - cmd.Wait() is always consumed so neither the child nor its
//     grandchildren are left as zombies.
//
// Without cancellation the behavior is identical to execute.
func executeCtx(ctx context.Context, name string, arg ...string) error {
	// Construct command for debug purposes
	var command = name
	for _, i := range arg {
		command += " " + i
	}

	debug("Executing %s", command)

	if err := ctx.Err(); err != nil {
		return ErrCancelled
	}

	cmd := exec.Command(name, arg...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	// Detach the child into its own process group so signals reach the
	// shell *and* every command it spawned.
	configureProcess(cmd)

	if err := cmd.Start(); err != nil {
		return err
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	select {
	case err := <-waitCh:
		// The command finished on its own. If a cancellation landed at
		// the same instant, the caller's Lifecycle.Finish acts as the
		// tie-breaker instead of this error.
		return err
	case <-ctx.Done():
		// Graceful request first (SIGTERM on Unix), then force-kill the
		// entire group if anything (grandchildren included) is stuck.
		requestProcessTreeTermination(cmd)
		select {
		case <-waitCh:
		case <-time.After(terminationGracePeriod):
			forceKillProcessTree(cmd)
			<-waitCh
		}
		return ErrCancelled
	}
}

func oneliner(name string, arg ...string) (string, error) {
	var buff bytes.Buffer
	gitCmd := exec.Command(name, arg...)
	gitCmd.Stdout = &buff
	err := gitCmd.Run()
	return strings.TrimSpace(buff.String()), err
}
