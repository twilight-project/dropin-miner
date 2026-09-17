package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

// One rule for every question this binary asks a participant: an answer
// exists only when the participant typed one. A read that ends without a
// line — an interrupt, a closed stdin, a console read the terminal
// aborted — is not "no", and it is not the visual default either. It is
// the absence of an answer, and the operation stops there having
// recorded, written and sent nothing.
//
// #81 is what a prompt that guesses instead costs. At
// `Enable mining rewards? [y/N]` the read error was discarded, so an
// interrupt produced an empty line, an empty line is not "y", and
// state/mining_decision.json was written {"version":1,"enabled":false} —
// a decision the participant never made, on the one file invariant 10
// calls the only runtime authority on whether mining is on. connect then
// went on to register, and setup's closing line invited a re-run that
// would reuse the saved "no" without asking again. The empty line is not
// the defect; treating the absence of a line as a line is. The same
// shape with the opposite default is worse: `agents install`'s
// `Proceed? [Y/n]` answers an empty line yes, so an interrupt there
// meant "yes, write every agent file".
//
// The signal handler is honored by keying on the read itself rather
// than on a context. An interrupt is what ends the read without a line,
// which is exactly the condition below; a context check here would also
// fire on a run's outer deadline, and a participant still reading the
// question is not an interrupt.
var errPromptAborted = errors.New("the question was not answered")

// promptAbortedReason is the first clause of every abort message, so one
// thing reads as one thing wherever a participant meets it.
const promptAbortedReason = "the question was not answered (stdin ended before a line was typed)"

// promptBufio asks question on w and reads the answer from br — the one
// reader a command must share across all of its prompts, because a pipe
// delivers several answers in a single chunk and a second reader over
// the same stdin silently loses whatever the first buffered past its own
// line.
//
// The line comes back raw: each caller trims what its own answer means,
// since a typed confirmation is not a yes/no and must not be trimmed the
// same way.
func promptBufio(w io.Writer, question string, br *bufio.Reader) (string, error) {
	fmt.Fprint(w, question)
	line, err := br.ReadString('\n')
	return answerOrAbort(line, err)
}

// promptSetup asks the same question over readSetupLine, which reads a
// byte at a time so the next reader of this stdin — connect, after
// setup's first question — still sees everything past the newline.
func promptSetup(w io.Writer, question string, r io.Reader) (string, error) {
	fmt.Fprint(w, question)
	line, err := readSetupLine(r)
	return answerOrAbort(line, err)
}

// answerOrAbort classifies one line read. Bytes that arrived are an
// answer even when no newline followed them: a pipe that ends without a
// trailing newline still delivered what it was asked for, and that is
// the reading the prompts which already handled their read error — the
// enrollment token, the provider key, the sr- key, the keyfile
// passphrase, the purge confirmation — were each written against.
//
// Two of those did not move here and should be named rather than left to
// be rediscovered: enroll.go's enrollment-token and provider-key prompts
// still carry their own copy of this classification, spelled
// `err != nil && strings.TrimSpace(line) == ""`. It agrees with this one
// in substance today — a read that delivered only whitespace is as good
// as one that delivered nothing — and both are correct. But they are two
// answers to the question this file exists to answer once, and they will
// not follow if the rule here ever changes. They read os.Stdin directly
// rather than a reader passed in, so routing them through promptBufio
// needs a seam they do not have; giving them one is a change to how
// `enroll` and `provider` take their input, which is worth doing on its
// own and not as a rider on this.
func answerOrAbort(line string, err error) (string, error) {
	if err != nil && line == "" {
		return "", errPromptAborted
	}
	return line, nil
}
