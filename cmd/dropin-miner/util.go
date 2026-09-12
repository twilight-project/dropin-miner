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
// could not be proven either way — the journal was kept, and
// the participant must NOT retry with a fresh signature: re-running
// wallet send resolves the same journal instead of building a new one.
const exitOutcomeUnknown = 5

// exitHumanDecisionRequired is connectRun's internal signal, in machine
// mode only, that it reached a point mid-run where answering would mean
// inventing a participant's mining decision (B.3 rebuild PR: a rebuilt
// identity turning out to be expired, discovered only after the /v1/agents/me
// call connectNeedsHumanDecision's own pre-check cannot make). It is never
// a real process exit code — cmdConnect's JSON wrapper always translates it
// to exitUsage, with the same code/action the ordinary pre-check gate uses,
// before anything reaches main(). Deliberately out of the exit-code range
// so a bug that let it leak unmapped would be obvious rather than silently
// plausible.
const exitHumanDecisionRequired = -1

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
