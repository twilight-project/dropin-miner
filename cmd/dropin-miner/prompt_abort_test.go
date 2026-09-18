package main

// #81, one case per prompt this binary asks.
//
// The defect the Windows tester met was not "an interrupt is mishandled at
// one prompt": it was that a prompt which discards its read error cannot
// tell an answer from the absence of one, and every prompt in the binary
// that discarded its error had that hole. So these cases do not test
// prompt.go's helper — they drive each real command to each real question
// and assert the three things the rule promises there: a non-zero exit,
// nothing recorded, and nothing sent.
//
// The negative half matters as much: an answer that WAS typed still
// decides, and -yes still answers exactly what it answered before, which
// is why the last two cases are here.

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/twilight-project/dropin-miner/pkg/auth"
)

// errConsoleInterrupt is the shape the Windows console read fails with
// when the participant presses Ctrl+C at a prompt: an error, no line, and
// the process still running because a signal handler took the signal. EOF
// — a closed stdin — is the gentler half of the same class, and every
// case below covers both.
var errConsoleInterrupt = errors.New("interrupt signal received")

// interruptReader delivers the lines the participant did type, one per
// Read the way a line-disciplined terminal does, and then ends the way an
// interrupted console read ends. With no lines it is the interrupt at the
// very first question.
type interruptReader struct {
	lines []string
	err   error
}

func interrupted(lines ...string) *interruptReader {
	return &interruptReader{lines: withNewlines(lines), err: errConsoleInterrupt}
}

// closedStdin is the same reader ending in EOF instead: `setup < /dev/null`
// after the terminal check has already been forced, a pipe whose writer
// went away, a harness that forgot to script an answer.
func closedStdin(lines ...string) *interruptReader {
	return &interruptReader{lines: withNewlines(lines), err: io.EOF}
}

func withNewlines(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, l+"\n")
	}
	return out
}

func (r *interruptReader) Read(p []byte) (int, error) {
	if len(r.lines) == 0 {
		return 0, r.err
	}
	n := copy(p, r.lines[0])
	if n < len(r.lines[0]) {
		r.lines[0] = r.lines[0][n:]
	} else {
		r.lines = r.lines[1:]
	}
	return n, nil
}

// endings runs one case against both ways a read can end without a line.
// They are one defect and must not be allowed to drift apart: a fix that
// keyed on io.EOF alone would leave the Windows console interrupt — the
// case actually reported — still writing a decision nobody made.
func endings(t *testing.T, run func(t *testing.T, stdin func(...string) *interruptReader)) {
	t.Helper()
	t.Run("interrupt", func(t *testing.T) { run(t, interrupted) })
	t.Run("closed stdin", func(t *testing.T) { run(t, closedStdin) })
}

// assertUnchanged names every path that moved. reflect.DeepEqual on the
// two snapshots is the assertion the uninstall cases already use; what
// this adds is saying which path broke it, because "the trees differ" is
// not a finding anyone can act on.
func assertUnchanged(t *testing.T, before, after map[string]fileSig) {
	t.Helper()
	for path, sig := range after {
		if prev, existed := before[path]; !existed {
			t.Errorf("created %s", path)
		} else if prev != sig {
			t.Errorf("changed %s", path)
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			t.Errorf("removed %s", path)
		}
	}
}

// ── the mining question (#81 as filed) ──────────────────────────────────

// The decision file is invariant 10's only runtime authority on whether
// mining is on. An unanswered question must not write it.
func TestAnInterruptAtTheMiningQuestionRecordsNoDecision(t *testing.T) {
	endings(t, func(t *testing.T, stdin func(...string) *interruptReader) {
		platform := newStubPlatform(t)
		cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")
		cfg, _, err := loadConfig(cfgPath, noEnv)
		if err != nil {
			t.Fatal(err)
		}
		store, err := auth.OpenStore(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		in := stdin()
		var errOut bytes.Buffer
		outcome, code := askMiningQuestion(in, bufio.NewReader(in), &bytes.Buffer{}, &errOut, noEnv, cfg, store, true, false)
		if code == exitOK {
			t.Fatalf("an unanswered mining question exited %d; want non-zero (got %+v)", code, outcome)
		}
		if outcome.enabled {
			t.Fatal("an unanswered mining question came back enabled")
		}
		if got := store.ReadMiningDecision().State; got != auth.MiningUndecided {
			t.Fatalf("mining decision is %q after an unanswered question; want undecided", got)
		}
		if lexists(filepath.Join(stateDir, "mining_decision.json")) {
			t.Fatal("mining_decision.json was written for a question nobody answered")
		}
		if !strings.Contains(errOut.String(), promptAbortedReason) {
			t.Fatalf("no abort message on stderr:\n%s", errOut.String())
		}
	})
}

// The whole of #81: the decision, and the registration that followed it.
// Counting the stub platform's register calls is the assertion — connect
// reaching Register at all is the half that left an agent the tester never
// claimed.
func TestAnInterruptAtTheMiningQuestionRegistersNothing(t *testing.T) {
	endings(t, func(t *testing.T, stdin func(...string) *interruptReader) {
		s := newSetupSandbox(t)
		s.platform.claim("credits")
		code, out, errOut := s.run(stdin(), true)
		if code == exitOK {
			t.Fatalf("setup exited 0 with the mining question unanswered\n%s\n%s", out, errOut)
		}
		if reg, _, _ := s.platform.counts(); reg != 0 {
			t.Fatalf("the platform saw %d register call(s) after an unanswered mining question", reg)
		}
		if lexists(filepath.Join(s.home, "state", "mining_decision.json")) {
			t.Fatal("mining_decision.json was written for a question nobody answered")
		}
		if lexists(filepath.Join(s.home, "state", "agent.json")) {
			t.Fatal("an agent registration was published after an unanswered mining question")
		}
	})
}

// ── the payout-address question ─────────────────────────────────────────

// The enable half is already on disk here, from a "y" the participant did
// type, so this cannot claim nothing was recorded. What it claims is that
// the empty-answer branch — create a wallet — is not reached by an answer
// that was never given.
func TestAnInterruptAtThePayoutAddressCreatesNoWallet(t *testing.T) {
	endings(t, func(t *testing.T, stdin func(...string) *interruptReader) {
		platform := newStubPlatform(t)
		cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")
		cfg, _, err := loadConfig(cfgPath, noEnv)
		if err != nil {
			t.Fatal(err)
		}
		store, err := auth.OpenStore(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		walletDir := filepath.Join(t.TempDir(), "wallet")
		t.Setenv("TOKENDROP_WALLET_DIR", walletDir)
		t.Setenv(walletPassphraseEnv, "correct horse battery staple")

		in := stdin("y")
		var errOut bytes.Buffer
		outcome, code := askMiningQuestion(in, bufio.NewReader(in), &bytes.Buffer{}, &errOut, os.Getenv, cfg, store, true, false)
		if code == exitOK {
			t.Fatalf("an unanswered address question exited %d; want non-zero (got %+v)", code, outcome)
		}
		if lexists(filepath.Join(walletDir, walletKeyFile)) {
			t.Fatal("a wallet was created for an address question nobody answered")
		}
		if _, ok, _ := store.LoadPayoutAddress(); ok {
			t.Fatal("a payout address was persisted for a question nobody answered")
		}
		// The typed "y" above still decided its own half, and the state it
		// leaves is exactly the one decideRegistrationOutcome resumes from.
		if got := store.ReadMiningDecision().State; got != auth.MiningEnabled {
			t.Fatalf("the typed y was lost: mining decision is %q", got)
		}
	})
}

// ── agents install's Proceed? [Y/n] ─────────────────────────────────────

// The opposite default, and so the worse half: an empty line here means
// yes. Counting the fake machine's writes is the assertion.
func TestAnInterruptAtTheAgentsProceedQuestionWritesNothing(t *testing.T) {
	endings(t, func(t *testing.T, stdin func(...string) *interruptReader) {
		m, ops := newFakeMachine("claude", "cursor")
		m.terminal = true
		before := len(m.files)
		var out, errOut bytes.Buffer
		code := agentsMain(ops, []string{"install", "-config", testCfg}, stdin(), &out, &errOut, envOf(nil))
		if code == exitOK {
			t.Fatalf("agents install exited 0 with Proceed? unanswered\n%s\n%s", out.String(), errOut.String())
		}
		if len(m.files) != before {
			t.Fatalf("agents install wrote %d file(s) for a question nobody answered", len(m.files)-before)
		}
		if !strings.Contains(errOut.String(), promptAbortedReason) {
			t.Fatalf("no abort message on stderr:\n%s", errOut.String())
		}
	})
}

// A typed "n" is still a decline, and still exits 0: an answered question
// and an unanswered one must not collapse into one outcome.
func TestATypedNoAtTheAgentsProceedQuestionStillDeclines(t *testing.T) {
	m, ops := newFakeMachine("claude", "cursor")
	m.terminal = true
	before := len(m.files)
	var out, errOut bytes.Buffer
	code := agentsMain(ops, []string{"install", "-config", testCfg}, strings.NewReader("n\n"), &out, &errOut, envOf(nil))
	if code != exitOK {
		t.Fatalf("a typed decline exited %d; want 0\n%s\n%s", code, out.String(), errOut.String())
	}
	if len(m.files) != before {
		t.Fatalf("a declined agents install wrote %d file(s)", len(m.files)-before)
	}
	if !strings.Contains(out.String(), "left everything as it was") {
		t.Fatalf("a typed decline lost its own wording:\n%s", out.String())
	}
}

// ── setup's adoption question ───────────────────────────────────────────

// Nothing has been created in home when this is asked, so an abort here
// really does leave both installations as they were found — and connect is
// never reached, which is what stops a fresh registration being minted
// beside an identity the participant was about to adopt.
func TestAnInterruptAtTheAdoptionQuestionAdoptsNothingAndConnectsNothing(t *testing.T) {
	endings(t, func(t *testing.T, stdin func(...string) *interruptReader) {
		s := newSetupSandbox(t)
		s.platform.claim("credits")
		aside := s.home + ".bak-20260101"
		writeFileT(t, filepath.Join(aside, credentialsFile), "{}")
		before := snapshotTree(t, s.root)

		code, out, errOut := s.run(stdin(), true)
		if code == exitOK {
			t.Fatalf("setup exited 0 with the adoption question unanswered\n%s\n%s", out, errOut)
		}
		if !strings.Contains(out, "Use it?") {
			t.Fatalf("the adoption question was never asked:\n%s", out)
		}
		if s.connectCalls != 0 {
			t.Fatalf("connect ran %d time(s) after an unanswered adoption question", s.connectCalls)
		}
		if reg, _, _ := s.platform.counts(); reg != 0 {
			t.Fatalf("the platform saw %d register call(s)", reg)
		}
		assertOwnership(t, before, snapshotTree(t, s.root), s.home)
		if !lexists(filepath.Join(aside, credentialsFile)) {
			t.Fatal("the set-aside installation was moved")
		}
	})
}

// ── setup's profile and agents questions ────────────────────────────────

// These two come after connect, so "nothing was recorded" cannot be the
// claim — the config and the registration are already on disk. The claim
// is that the step that asked did not act, and that the steps after it did
// not run either: an unanswered question ends the run, it does not get
// skipped past.
func TestAnInterruptAtSetupsProfileQuestionLeavesTheProfileAndTheAgentsAlone(t *testing.T) {
	endings(t, func(t *testing.T, stdin func(...string) *interruptReader) {
		s := newSetupSandbox(t)
		s.platform.claim("credits")
		s.onPath["claude"] = true
		profile := filepath.Join(s.userHome, ".zshrc")
		writeFileT(t, profile, "# mine\n")

		code, out, errOut := s.run(stdin("n"), true) // "n" answers mining; the profile question is next
		if code == exitOK {
			t.Fatalf("setup exited 0 with the profile question unanswered\n%s\n%s", out, errOut)
		}
		if got := string(s.readFile(profile)); got != "# mine\n" {
			t.Fatalf("the shell profile was changed:\n%s", got)
		}
		if lexists(s.paths().claudeSkill) {
			t.Fatal("the agents step ran after the profile question was left unanswered")
		}
		if !strings.Contains(errOut, promptAbortedReason) {
			t.Fatalf("no abort message on stderr:\n%s", errOut)
		}
	})
}

func TestAnInterruptAtSetupsAgentsQuestionSetsUpNoAgent(t *testing.T) {
	endings(t, func(t *testing.T, stdin func(...string) *interruptReader) {
		s := newSetupSandbox(t)
		s.platform.claim("credits")
		s.onPath["claude"] = true

		// mining, then the profile question, then the agents question.
		code, out, errOut := s.run(stdin("n", "n"), true)
		if code == exitOK {
			t.Fatalf("setup exited 0 with the agents question unanswered\n%s\n%s", out, errOut)
		}
		if !strings.Contains(out, "Set up the coding agents found on this machine now?") {
			t.Fatalf("the agents question was never asked:\n%s", out)
		}
		if lexists(s.paths().claudeSkill) {
			t.Fatal("an agent was set up for a question nobody answered")
		}
	})
}

// ── uninstall's two confirmations ───────────────────────────────────────

func TestAnInterruptAtUninstallsConfirmationRemovesNothing(t *testing.T) {
	endings(t, func(t *testing.T, stdin func(...string) *interruptReader) {
		s := installed(t)
		before := snapshotTree(t, s.root)
		code, out, errOut := s.uninstall(t, stdin(), true, nil)
		if code == exitOK {
			t.Fatalf("uninstall exited 0 with its confirmation unanswered\n%s\n%s", out, errOut)
		}
		if !strings.Contains(errOut, promptAbortedReason) {
			t.Fatalf("no abort message on stderr:\n%s", errOut)
		}
		assertUnchanged(t, before, snapshotTree(t, s.root))
	})
}

// The purge confirmation already had this guard before #81 — it is the one
// prompt in the binary that did. It is pinned here so the shared rule
// cannot quietly regress the prompt it was generalized from.
func TestAnInterruptAtThePurgeConfirmationPurgesNothing(t *testing.T) {
	endings(t, func(t *testing.T, stdin func(...string) *interruptReader) {
		s := installed(t)
		before := snapshotTree(t, s.root)
		rr := &revokeRecorder{}
		code, out, errOut := s.uninstall(t, stdin(), true, rr, "-purge-state", "-yes")
		if code == exitOK {
			t.Fatalf("purge exited 0 with its confirmation unanswered\n%s\n%s", out, errOut)
		}
		if rr.calls != 0 {
			t.Fatalf("a purge nobody confirmed revoked the authorization (%d call(s))", rr.calls)
		}
		if !strings.Contains(out, noParticipantChange) {
			t.Fatalf("the refusal did not say nothing was changed:\n%s", out)
		}
		assertUnchanged(t, before, snapshotTree(t, s.root))
	})
}

// ── wallet send's typed confirmation ────────────────────────────────────

// Nothing is signed either way here; the exit code is the whole
// difference, and a script driving a payout must be able to tell "the
// operator said no" from "the operator was never asked".
func TestAnInterruptAtTheWalletSendConfirmationSignsNothing(t *testing.T) {
	endings(t, func(t *testing.T, stdin func(...string) *interruptReader) {
		node := newFakeNode(t, nodeConfig{
			chainID: "twilight-devnet-2", accountNumber: 5, sequence: 9,
			balance: "1000000", appearAfter: 1,
		})
		dir := walletScratchDir(t)
		var out, errOut bytes.Buffer
		if code := cmdWallet([]string{"init", "-dir", dir, "-print-anyway"},
			strings.NewReader(""), &out, &errOut, envOf(map[string]string{walletPassphraseEnv: "p-test-1"})); code != 0 {
			t.Fatalf("init: %s", errOut.String())
		}
		out.Reset()
		errOut.Reset()
		code := cmdWallet([]string{"send", "-dir", dir, "-node", node.srv.URL, "-chain-id", "twilight-devnet-2",
			"-to", "twilight1kl0dn0rtwk46h9zcmazyyrruta290crh93rnlh", "-amount", "1000"},
			stdin(), &out, &errOut, envOf(map[string]string{walletPassphraseEnv: "p-test-1"}))
		if code == exitOK {
			t.Fatalf("wallet send exited 0 with its confirmation unanswered\n%s\n%s", out.String(), errOut.String())
		}
		if node.broadcastCount != 0 {
			t.Fatalf("a send nobody confirmed broadcast %d transaction(s)", node.broadcastCount)
		}
		if lexists(filepath.Join(dir, pendingTxFile)) {
			t.Fatal("a send nobody confirmed journaled a pending transaction")
		}
	})
}

// ── a line without a newline is still a line ────────────────────────────

// answerOrAbort aborts only when NO byte of an answer arrived. Bytes that
// did arrive are an answer even with no newline behind them, because a
// pipe — `printf y | dropin-miner agents install` — ends exactly that
// way, and the alternative reading would turn every such caller's answer
// into an abort. The clause is invisible in the interrupt cases above
// (they deliver no bytes at all), so it is asserted here, on both
// readers, in both directions.
func TestAPipedAnswerWithoutATrailingNewlineIsStillAnAnswer(t *testing.T) {
	t.Run("agents install proceeds on a bare y", func(t *testing.T) {
		m, ops := newFakeMachine("claude")
		m.terminal = true
		before := len(m.files)
		var out, errOut bytes.Buffer
		code := agentsMain(ops, []string{"install", "-config", testCfg}, strings.NewReader("y"), &out, &errOut, envOf(nil))
		if code != exitOK {
			t.Fatalf("a bare y exited %d\n%s\n%s", code, out.String(), errOut.String())
		}
		if len(m.files) == before {
			t.Fatalf("a bare y wrote nothing:\n%s", out.String())
		}
	})

	t.Run("agents install declines on a bare n", func(t *testing.T) {
		m, ops := newFakeMachine("claude")
		m.terminal = true
		before := len(m.files)
		var out, errOut bytes.Buffer
		code := agentsMain(ops, []string{"install", "-config", testCfg}, strings.NewReader("n"), &out, &errOut, envOf(nil))
		if code != exitOK {
			t.Fatalf("a bare n exited %d; a declined answer is not an aborted one\n%s\n%s", code, out.String(), errOut.String())
		}
		if len(m.files) != before {
			t.Fatalf("a bare n wrote %d file(s)", len(m.files)-before)
		}
		if strings.Contains(errOut.String(), promptAbortedReason) {
			t.Fatalf("a bare n was read as an unanswered question:\n%s", errOut.String())
		}
	})

	t.Run("uninstall removes on a bare y", func(t *testing.T) {
		s := installed(t)
		code, out, errOut := s.uninstall(t, strings.NewReader("y"), true, nil)
		if code != exitOK {
			t.Fatalf("a bare y exited %d\n%s\n%s", code, out, errOut)
		}
		if lexists(s.paths().claudeSkill) {
			t.Fatalf("a bare y removed nothing:\n%s", out)
		}
	})

	t.Run("uninstall declines on a bare n", func(t *testing.T) {
		s := installed(t)
		before := snapshotTree(t, s.root)
		code, out, errOut := s.uninstall(t, strings.NewReader("n"), true, nil)
		if code != exitOK {
			t.Fatalf("a bare n exited %d; a declined uninstall is not an aborted one\n%s\n%s", code, out, errOut)
		}
		if strings.Contains(errOut, promptAbortedReason) {
			t.Fatalf("a bare n was read as an unanswered question:\n%s", errOut)
		}
		assertUnchanged(t, before, snapshotTree(t, s.root))
	})
}

// ── -yes answers what it always answered, and nothing more ──────────────

// -yes answers setup's own questions without reading stdin at all, so a
// stdin that fails does not stop them. The one line this feeds is the
// mining question, which -yes deliberately does not answer (S6) — that
// asymmetry is the point of the case.
func TestYesStillAnswersSetupsOwnQuestionsOverAFailingStdin(t *testing.T) {
	endings(t, func(t *testing.T, stdin func(...string) *interruptReader) {
		s := newSetupSandbox(t)
		s.platform.claim("credits")
		s.onPath["claude"] = true
		code, out, errOut := s.run(stdin("n"), true, "-yes")
		if code != exitOK {
			t.Fatalf("-yes setup exited %d\n%s\n%s", code, out, errOut)
		}
		if !lexists(s.paths().claudeSkill) {
			t.Fatalf("-yes did not set up the agent it found:\n%s", out)
		}
		if !strings.Contains(out, "yes (-yes)") {
			t.Fatalf("-yes did not answer setup's own questions:\n%s", out)
		}
	})
}

// ...and it does not answer the mining question. Without a typed line,
// -yes leaves it unanswered, which now aborts rather than recording a
// decision nobody made.
func TestYesDoesNotAnswerTheMiningQuestion(t *testing.T) {
	endings(t, func(t *testing.T, stdin func(...string) *interruptReader) {
		s := newSetupSandbox(t)
		s.platform.claim("credits")
		code, out, errOut := s.run(stdin(), true, "-yes")
		if code == exitOK {
			t.Fatalf("-yes answered the mining question\n%s\n%s", out, errOut)
		}
		if reg, _, _ := s.platform.counts(); reg != 0 {
			t.Fatalf("the platform saw %d register call(s)", reg)
		}
		if lexists(filepath.Join(s.home, "state", "mining_decision.json")) {
			t.Fatal("-yes recorded a mining decision")
		}
	})
}
