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

// exitOutcomeUnknown: a wallet send's broadcast or confirmation-wait
// could not be proven either way (REL-15) — the journal was kept, and
// the participant must NOT retry with a fresh signature: re-running
// wallet send resolves the same journal instead of building a new one.
const exitOutcomeUnknown = 5

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
