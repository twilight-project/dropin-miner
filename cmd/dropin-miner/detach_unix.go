//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// spawnDetached starts exe in its own session with no terminal, so the
// parent may exit (and the agent's tool call return) while the child
// finishes. Output goes nowhere: a flush reports through its exit code
// and the next `status`, never through a stream nobody is reading.
func spawnDetached(exe string, args []string) error {
	cmd := exec.Command(exe, args...) // #nosec G204 -- our own executable, fixed arguments
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Release, do not wait: the child is nobody's responsibility now, and
	// init reaps it because Setsid made it a session leader with no
	// controlling terminal.
	return cmd.Process.Release()
}
