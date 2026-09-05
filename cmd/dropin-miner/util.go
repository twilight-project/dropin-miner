package main

import (
	"flag"
	"io"
)

// Exit codes, shared by every command: the shell convention an agent or a
// script can branch on without parsing stderr.
const (
	exitOK        = 0
	exitTransport = 1
	exitUsage     = 2
	exitClientErr = 3 // any non-2xx below 500
	exitServerErr = 4 // HTTP 5xx
)

// exitChainRejected: the chain refused a wallet transaction (wallet send).
const exitChainRejected = 3

func orDefaults(cfgSource string) string {
	if cfgSource == "" {
		return "defaults/env, no config file found"
	}
	return cfgSource
}

// newFlagSet builds a FlagSet whose usage output goes to the command's own
// stderr writer rather than the process's.
func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}
