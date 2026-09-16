//go:build windows

package main

import (
	"context"
	"os/exec"
	"syscall"
)

// cmdShellCommand starts cmd.exe the way Node's child_process does for a
// shell command on Windows — /d /s /c with the whole string in one pair of
// quotes that /s strips — and hands it the raw command line, because Go's
// own argument quoting would escape the rendered string's quotes into
// something cmd never receives from a host.
func cmdShellCommand(ctx context.Context, program, script string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, program) // #nosec G204 -- cmd.exe running a string this test rendered
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `"` + program + `" /d /s /c "` + script + `"`}
	return cmd
}
