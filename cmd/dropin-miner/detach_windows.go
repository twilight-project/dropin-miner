//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
	createNoWindow        = 0x08000000
)

// spawnDetached on Windows: a new process group with no console, so no
// window flashes on every search and the child outlives the tool call.
func spawnDetached(exe string, args []string) error {
	cmd := exec.Command(exe, args...) // #nosec G204 -- our own executable, fixed arguments
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNewProcessGroup | detachedProcess | createNoWindow,
		HideWindow:    true,
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
