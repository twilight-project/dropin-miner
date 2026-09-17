package main

// #86: an aborted destructive run leaves the installation as it found it,
// including the lock files its own exclusion had to create.
//
// Taking an operation lock opens its file with O_CREATE (tryLockFile), so
// probing an installation that has never flushed makes a flush.lock that
// was not there before. The Windows tester's `uninstall -home <disposable>
// -purge-state -yes`, answered with an empty line so the confirmation
// aborted, printed "That does not match. No participant state or
// integrations were changed." over a new empty flush.lock it had just
// made. The sentence was false, and a dry run could not have predicted the
// file.
//
// The cases below are about which files the exclusion may remove, so they
// name the three locks it takes rather than only the one the issue
// reported: a rule scoped to flush.lock would be a second special case, not
// the rule.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// exclusionLocks is the three operation locks excludeLifecycle takes for
// this installation, resolved the way it resolves them rather than
// hand-written, so a case cannot drift from what the exclusion actually
// touches.
func exclusionLocks(t *testing.T, s *setupSandbox) []string {
	t.Helper()
	connectLock, flushLock, err := operationLockPaths(s.home, s.getenv)
	if err != nil {
		t.Fatalf("operationLockPaths: %v", err)
	}
	locks := []string{filepath.Join(s.home, setupLockFile)}
	for _, p := range []string{connectLock, flushLock} {
		if p != "" {
			locks = append(locks, p)
		}
	}
	if len(locks) != 3 {
		t.Fatalf("want setup, connect and flush locks for this installation, got %v", locks)
	}
	return locks
}

func removeAllT(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

// An installation that has never flushed or connected — the disposable one
// a destructive test is run against — has none of these files. An aborted
// purge must leave it that way.
func TestAnAbortedPurgeLeavesNoLockFileItsExclusionCreated(t *testing.T) {
	s := installed(t)
	locks := exclusionLocks(t, s)
	removeAllT(t, locks...)
	before := snapshotTree(t, s.root)

	// An empty line at the typed confirmation: -yes does not satisfy it,
	// and the run aborts having changed nothing.
	code, out, errOut := s.uninstall(t, strings.NewReader("\n"), true, nil, "-purge-state", "-yes")
	if code == exitOK {
		t.Fatalf("the purge proceeded on an empty confirmation\n%s\n%s", out, errOut)
	}
	if !strings.Contains(out, noParticipantChange) {
		t.Fatalf("the refusal did not say nothing was changed:\n%s", out)
	}
	for _, p := range locks {
		if lexists(p) {
			t.Errorf("the exclusion left %s behind, under a line that says nothing was changed", p)
		}
	}
	// The sentence the run printed, checked against the whole tree rather
	// than the three paths above: "nothing was changed" is a claim about
	// the installation, not about the locks.
	assertUnchanged(t, before, snapshotTree(t, s.root))
}

// The other direction, and the one a careless fix breaks: a lock file that
// was already there belongs to the installation, not to this run, and an
// abort must not tidy it away.
func TestAnAbortedPurgeLeavesAPreexistingLockFileByteIdentical(t *testing.T) {
	s := installed(t)
	locks := exclusionLocks(t, s)
	removeAllT(t, locks...)
	// One that existed before, with bytes of its own so "left alone" is a
	// stronger claim than "still exists".
	preexisting := locks[len(locks)-1]
	writeFileT(t, preexisting, "flush lock from an earlier run\n")
	before := snapshotTree(t, s.root)

	code, out, errOut := s.uninstall(t, strings.NewReader("\n"), true, nil, "-purge-state", "-yes")
	if code == exitOK {
		t.Fatalf("the purge proceeded on an empty confirmation\n%s\n%s", out, errOut)
	}
	if !lexists(preexisting) {
		t.Fatalf("an aborted purge removed %s, which it did not create", preexisting)
	}
	if got := string(s.readFile(preexisting)); got != "flush lock from an earlier run\n" {
		t.Fatalf("an aborted purge rewrote a lock file it did not create: %q", got)
	}
	for _, p := range locks[:len(locks)-1] {
		if lexists(p) {
			t.Errorf("the exclusion left %s behind", p)
		}
	}
	assertUnchanged(t, before, snapshotTree(t, s.root))
}

// The same on the plain uninstall, which takes the same exclusion: the
// issue reported -purge-state, but nothing about the defect was particular
// to it.
func TestAnAbortedUninstallLeavesNoLockFileItsExclusionCreated(t *testing.T) {
	s := installed(t)
	locks := exclusionLocks(t, s)
	removeAllT(t, locks...)
	before := snapshotTree(t, s.root)

	code, out, errOut := s.uninstall(t, strings.NewReader("n\n"), true, nil)
	if code != exitOK {
		t.Fatalf("a declined uninstall exited %d\n%s\n%s", code, out, errOut)
	}
	for _, p := range locks {
		if lexists(p) {
			t.Errorf("the exclusion left %s behind on a declined uninstall", p)
		}
	}
	assertUnchanged(t, before, snapshotTree(t, s.root))
}

// The other side of proceeded(): a run that went through keeps the lock
// files it took, because they are now part of the installation it changed
// and not something an abort has to undo. Without this, dropping the
// proceeded() call would cost nothing a test could see.
func TestAnUninstallThatProceedsKeepsTheLocksItTook(t *testing.T) {
	s := installed(t)
	locks := exclusionLocks(t, s)
	removeAllT(t, locks...)
	setupLock := filepath.Join(s.home, setupLockFile)

	code, out, errOut := s.uninstall(t, strings.NewReader("y\n"), true, nil)
	if code != exitOK {
		t.Fatalf("uninstall exited %d\n%s\n%s", code, out, errOut)
	}
	if lexists(s.paths().claudeSkill) {
		t.Fatalf("the uninstall did not proceed:\n%s", out)
	}
	if !lexists(setupLock) {
		t.Error("a completed uninstall removed the setup.lock it took; only an aborted run does that")
	}
}

// A run that proceeds is unchanged: the locks it took are part of the
// installation it is now changing, and the purge removes the state the way
// it always did. The failure this rules out is a cleanup that fires on the
// success path and deletes a lock out from under an operation still using
// it.
func TestAPurgeThatProceedsIsUnchanged(t *testing.T) {
	s := installed(t)
	removeAllT(t, exclusionLocks(t, s)...)

	code, out, errOut := s.uninstall(t, tty(walletFixtureAddress(t)), true, &revokeRecorder{}, "-purge-state")
	if code != exitOK {
		t.Fatalf("the purge exited %d\n%s\n%s", code, out, errOut)
	}
	for _, rel := range []string{"state", "spool", "intake", "wallet", setupConfigFile} {
		if lexists(filepath.Join(s.home, rel)) {
			t.Errorf("-purge-state left %s", rel)
		}
	}
}

// The upgrade exclusion is deliberately outside this rule, and says so in
// its own construction. This pins that, because the removal lives on the
// shared type and would otherwise reach upgrade the first time someone
// copied the excludeLifecycle line.
func TestTheUpgradeExclusionDoesNotRemoveTheLocksItCreated(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, ".tokendrop")
	exe := filepath.Join(home, "bin", "dropin-miner")
	writeFileT(t, exe, "binary")

	ex, err := excludeForUpgrade(home, exe, 0)
	if err != nil {
		t.Fatalf("excludeForUpgrade: %v", err)
	}
	setupLock := filepath.Join(home, setupLockFile)
	updateLock := exe + updateLockSuffix
	for _, p := range []string{setupLock, updateLock} {
		if !lexists(p) {
			t.Fatalf("excludeForUpgrade did not take %s", p)
		}
	}
	// No proceeded() call: the release below is the one an upgrade that
	// refused would reach.
	ex.release()
	for _, p := range []string{setupLock, updateLock} {
		if !lexists(p) {
			t.Errorf("%s was removed; an upgrade's lock files are installation furniture uninstall -binary removes (S19), not this rule's business", p)
		}
	}
}
