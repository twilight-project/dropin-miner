package main

// The client's machine-facing protocol: one JSON object, one newline, and
// a closed vocabulary an agent can branch on without reading prose.
//
// Every machine surface — search, doctor, status, connect — carries the
// same header, so an SDK reads ok/code/retryable/action the same way
// whatever it ran. The header is the contract; the per-command payload
// hangs off it.
//
// Two rules hold everywhere in here. Classification comes from types and
// structured conditions, never from the text of an error — message wording
// that a decision depends on is an API nobody agreed to. And a diagnostic
// message is for a human reading a log: nothing branches on it, it is
// bounded, and it is redacted before it is written.

import (
	"encoding/json"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/twilight-project/dropin-miner/pkg/redact"
)

// machineVersion is the version of the envelope itself, not of any
// command's payload. A payload change that keeps the header's meaning does
// not bump it; a change to what ok/code/retryable/action mean does.
const machineVersion = 1

// The five machine statuses, one per process class. These are the exit
// codes this CLI has always used, given names — not a second, parallel
// meaning for the same numbers.
const (
	statusOK        = "ok"
	statusTransport = "transport_error"
	statusUsage     = "usage_error"
	statusClient    = "client_error"
	statusServer    = "server_error"
)

// The closed action vocabulary. An agent switches on exactly these; there
// is deliberately no "other", because the moment one exists every caller
// starts reading the message to find out what it meant.
//
//	none         nothing to do
//	retry        the same call may succeed later
//	fix_input    the request itself was wrong
//	connect      the registration/setup/claim workflow needs attention
//	login        the search credential needs attention (dropin-miner login)
//	check_access authorization exists but does not cover this
//	report       neither retrying nor editing the request will help
const (
	actionNone        = "none"
	actionRetry       = "retry"
	actionFixInput    = "fix_input"
	actionConnect     = "connect"
	actionLogin       = "login"
	actionCheckAccess = "check_access"
	actionReport      = "report"
)

// statusForExit is the one place the exit-code/status correspondence
// lives, so the two cannot drift apart in separate switch statements.
func statusForExit(code int) string {
	switch code {
	case exitOK:
		return statusOK
	case exitTransport:
		return statusTransport
	case exitUsage:
		return statusUsage
	case exitClientErr:
		return statusClient
	case exitServerErr:
		return statusServer
	default:
		return statusServer
	}
}

// machineHeader is the part of every envelope an agent may rely on. All
// eight fields are always present: ok and retryable carry no omitempty
// precisely because false is the answer that matters most often.
type machineHeader struct {
	Version   int    `json:"version"`
	Command   string `json:"command"`
	OK        bool   `json:"ok"`
	ExitCode  int    `json:"exit_code"`
	Status    string `json:"status"`
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
	Action    string `json:"action"`
}

func newMachineHeader(command string, exitCode int, code string, retryable bool, action string) machineHeader {
	return machineHeader{
		Version:   machineVersion,
		Command:   command,
		OK:        exitCode == exitOK,
		ExitCode:  exitCode,
		Status:    statusForExit(exitCode),
		Code:      code,
		Retryable: retryable,
		Action:    action,
	}
}

// machineMessageCap bounds every diagnostic that reaches an envelope.
const machineMessageCap = 512

// machineError is diagnostic only. No decision logic consumes Message —
// that is the whole point of the header above it.
type machineError struct {
	Message string `json:"message"`
	Source  string `json:"source"` // "client" or "router"
}

func clientMessage(err error) *machineError {
	if err == nil {
		return nil
	}
	return &machineError{Message: boundMessage(err.Error()), Source: "client"}
}

// routerMessage carries the router's own diagnostic string. It is remote,
// untrusted data: bounded and redacted like any other, and never parsed.
func routerMessage(s string) *machineError {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &machineError{Message: boundMessage(s), Source: "router"}
}

// boundMessage makes a diagnostic safe to put in an envelope: redacted of
// anything that looks like a credential or a home path, collapsed to one
// line, and truncated on a rune boundary so the result is always valid
// UTF-8. Control bytes cannot escape the JSON string — encoding/json
// escapes them — but collapsing whitespace keeps a log line a log line.
func boundMessage(s string) string {
	s = redact.String(strings.Join(strings.Fields(s), " "))
	if len(s) <= machineMessageCap {
		return s
	}
	cut := machineMessageCap
	for cut > 0 && !utf8.ValidString(s[:cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// emitMachine writes exactly one JSON object followed by exactly one
// newline, and nothing else. json.Encoder.Encode supplies the newline.
func emitMachine(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

// commandEnvelope is the machine answer for the commands whose payload is
// a report rather than a protocol result: doctor, status, connect.
type commandEnvelope struct {
	machineHeader
	Data  any           `json:"data,omitempty"`
	Error *machineError `json:"error,omitempty"`
}

// jsonRequested reports whether -json appears in args, without consuming
// it. The flag is parsed normally by each command; this exists only so a
// command can know its output mode before flag parsing can fail, and still
// answer a parse failure in the format the caller asked for.
func jsonRequested(args []string) bool {
	return hasBoolFlag(args, "json")
}

func hasBoolFlag(args []string, name string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		switch a {
		case "-" + name, "--" + name,
			"-" + name + "=true", "--" + name + "=true":
			return true
		}
	}
	return false
}
