package main

import (
	"flag"
	"fmt"
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

// exitLifecycleBusy is connectRun's internal signal, in machine mode only,
// that it never passed the lifecycle gate. cmdConnect's JSON wrapper
// translates it to exitTransport with code lifecycle_busy; like
// exitHumanDecisionRequired it is out of range so a leak would be obvious.
const exitLifecycleBusy = -2

// exitConfigNotFound is connectRun's internal signal, in machine mode only,
// that resolution (describeConfigSource, ruling D-R1) found no config file
// at all. Like the other two sentinels, connectCommand's JSON wrapper
// translates it before anything reaches main(); the text path never
// produces it (machine is false there), so it is out of the exit-code
// range for the same reason as the other two.
const exitConfigNotFound = -3

func orDefaults(cfgSource string) string {
	if cfgSource == "" {
		return "defaults/env, no config file found"
	}
	return cfgSource
}

// printConfigSource names, in every text report that resolves one, exactly
// which config file it read (or that none was found and built-in defaults
// are in use) and which state directory that resolution led to — ruling
// D-R1's naming requirement. status and doctor both call this before
// anything else, so a participant whose terminal resolved a different
// installation than they expected sees why at the top of the report rather
// than having to infer it from what follows.
func printConfigSource(w io.Writer, cfgSource, stateDir string) {
	fmt.Fprintf(w, "%-8s %s\n", "config:", orDefaults(cfgSource))
	fmt.Fprintf(w, "%-8s %s\n", "state:", stateDir)
}

// newFlagSet builds a FlagSet whose usage output goes to the command's own
// stderr writer rather than the process's.
func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}
