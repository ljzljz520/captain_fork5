//go:build windows
// +build windows

package captain // import "github.com/harbur/captain"

import (
	"os/exec"
	"strconv"
)

// Windows has no POSIX process groups/SIGTERM. The process tree is torn
// down through taskkill, which terminates the child together with every
// descendant (/T) unconditionally (/F).
func configureProcess(cmd *exec.Cmd) {
	// cmd.Process.Kill only removes the direct child; tree handling is
	// performed by forceKillProcessTree below.
}

// requestProcessTreeTermination is a no-op: Windows lacks a graceful
// termination signal, so the force path handles cancellation directly.
func requestProcessTreeTermination(cmd *exec.Cmd) {}

// forceKillProcessTree kills the child and its whole descendant tree.
func forceKillProcessTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	if err := kill.Run(); err != nil {
		debug("taskkill failed: %s", err)
	}
}
