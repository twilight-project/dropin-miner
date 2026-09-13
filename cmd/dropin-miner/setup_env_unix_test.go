//go:build !windows

package main

import "testing"

// restoreUserEnvironmentAfter has nothing to restore off Windows; the only
// caller skips there first.
func restoreUserEnvironmentAfter(*testing.T, ...string) {}
