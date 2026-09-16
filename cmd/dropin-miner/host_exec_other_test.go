//go:build !windows

package main

import (
	"context"
	"os/exec"
)

// cmdShellCommand exists only on Windows; execShellsFor never names cmd.exe
// elsewhere.
func cmdShellCommand(ctx context.Context, program, script string) *exec.Cmd {
	return exec.CommandContext(ctx, program, "/d", "/s", "/c", script) // #nosec G204 -- unreachable off Windows
}
