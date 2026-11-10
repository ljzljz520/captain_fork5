//go:build !windows
// +build !windows

package captain // import "github.com/harbur/captain"

import (
	"os/exec"
	"syscall"
)

// configureProcess starts the child as leader of a fresh process group.
// Signals sent to the negative PID then reach the shell and every
// descendant it spawned (the grandchild processes), instead of only the
// direct child.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalProcessGroup delivers sig to every member of the child's group.
func signalProcessGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, sig)
}

// requestProcessTreeTermination asks the whole group to exit. Unlike a
// bare Process.Kill, this gives well-behaved shells and services a
// chance to clean up.
func requestProcessTreeTermination(cmd *exec.Cmd) {
	if err := signalProcessGroup(cmd, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		debug("SIGTERM to process group failed: %s", err)
	}
}

// forceKillProcessTree escalates to SIGKILL across the group. Processes
// that already exited report ESRCH, which is the desired outcome.
func forceKillProcessTree(cmd *exec.Cmd) {
	if err := signalProcessGroup(cmd, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		debug("SIGKILL to process group failed: %s", err)
	}
}
