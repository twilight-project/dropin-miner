package main

// The payout command's surface (AS MINIS-VER-014, ESC-029).
//
// The AS makes a declaration inert. What this CLI can still get wrong is
// telling a person it is not — so these tests are about argument handling and
// about words. Neither is fussiness: "payout address set" and "payout address
// proposed" differ by whether the reader keeps watching for the operator.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/twilight-project/dropin-miner/pkg/auth"
)

// A subcommand is required, and an unknown one is a usage error rather than
// something that reaches the network.
func TestPayoutRequiresAKnownSubcommand(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"activate"},
		{"register", "twilight1abc"},
	} {
		if code := cmdPayout(args); code != 2 {
			t.Errorf("payout %v exited %d, want 2 (usage)", args, code)
		}
	}
}

// `payout set` without an address is a usage error, and a flag is not an
// address — otherwise `payout set -config x` would declare "-config".
func TestPayoutSetRequiresAnAddressAndNotAFlag(t *testing.T) {
	for _, args := range [][]string{
		{"set"},
		{"set", "-config", "some.toml"},
	} {
		if code := cmdPayout(args); code != 2 {
			t.Errorf("payout %v exited %d, want 2 (usage)", args, code)
		}
	}
}

// Every command the usage text advertises is actually routed. A command
// implemented and not routed is the "built but unreachable" gap this program
// has paid for before.
//
// It cannot be checked by exit code, and the first version of this test tried:
// dispatch("payout", nil) returns 2 because cmdPayout wants a subcommand, and
// an UNROUTED name returns 2 from the unknown-command branch. Identical
// results, so the assertion could not fail — it passed with the case removed
// from the dispatch table entirely. What distinguishes them is what reaches
// stderr, so that is what this reads.
//
// t.Setenv clears TOKENDROP_CONFIG for the test's duration (coverage gap B,
// WP2 review): dispatch reads it through the real os.Getenv, not an injected
// one (only search/agents/hook/flush/login take that as a parameter), so a
// developer's own TOKENDROP_CONFIG pointing at a real, mining-enabled config
// used to leak straight through config.Load's env fallback and let
// dispatch("enroll", nil) reach a real OAuth device-flow poll — the same
// class of bug independently found and fixed on an unmerged branch off
// main (PR #4); fixed here directly rather than waiting on that to merge.
func TestEveryAdvertisedCommandIsRouted(t *testing.T) {
	t.Setenv("TOKENDROP_CONFIG", "")
	for _, name := range []string{"enroll", "join", "provider", "payout", "status", "doctor", "earnings", "connect", "mining"} {
		t.Run(name, func(t *testing.T) {
			// No -config, so each command refuses early and none of them
			// reaches the network — except connect, which (unlike every
			// other command here) has no [mining].enabled-style gate to
			// default off: with zero config it would happily default to
			// the real platform base URL and attempt a real registration.
			// An explicit, nonexistent -config makes config.Load refuse
			// unconditionally (the explicit-path branch, not the
			// silent-fallback one absent-path takes), which is the same
			// clean "refused before touching state or network" property
			// every other command gets for free from its own gate.
			args := []string(nil)
			if name == "connect" {
				args = []string{"-config", filepath.Join(t.TempDir(), "does-not-exist.toml")}
			}
			errText := captureStderr(t, func() { _ = dispatch(name, args) })
			if strings.Contains(errText, "unknown command") {
				t.Fatalf("%q is advertised in the usage text and is not on the dispatch table: %s", name, errText)
			}
		})
	}

	// And the negative, so the check above is known to be capable of firing.
	errText := captureStderr(t, func() { _ = dispatch("payuot", nil) })
	if !strings.Contains(errText, "unknown command") {
		t.Fatal("a misspelled command was not reported as unknown; this test cannot detect an unrouted one either")
	}
}

// The usage text tells a reader that a declaration is not the end of the
// story, and that `enroll` has a browserless door.
func TestUsageDescribesWhatTheCommandsDoNotDo(t *testing.T) {
	for _, want := range []string{
		"payout set <address>",
		"operator",   // somebody else has to act
		"-assertion", // the browserless enrollment door
	} {
		if !strings.Contains(usageText, want) {
			t.Errorf("usage does not mention %q", want)
		}
	}
	// "proposes", not "sets": a participant who reads the usage as
	// completion is the failure the AS's inertness cannot prevent.
	if !strings.Contains(usageText, "proposes") {
		t.Error(`usage should say a payout address is "proposed", not set`)
	}
}

// What a participant is shown after declaring says, unambiguously, that
// nothing is in force yet.
func TestDeclarationOutputDoesNotReadAsCompletion(t *testing.T) {
	out := captureStdout(t, func() {
		printDeclaration(&declarationFixture)
	})
	for _, want := range []string{"proposed", "in force:   no", "operator"} {
		if !strings.Contains(out, want) {
			t.Errorf("declaration output does not contain %q:\n%s", want, out)
		}
	}
	// Words that would tell a reader they are done.
	for _, forbidden := range []string{"success", "complete", "you will be paid"} {
		if strings.Contains(strings.ToLower(out), forbidden) {
			t.Errorf("declaration output contains %q, which reads as completion:\n%s", forbidden, out)
		}
	}
}

var declarationFixture = auth.PayoutDeclaration{
	Status:           "PENDING",
	Address:          "twilight1abc",
	CanonicalAddress: "twilight1abc",
	Effective:        false,
	DeclaredAt:       "2026-08-26T00:00:00.000Z",
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	return capture(t, &os.Stderr, fn)
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	return capture(t, &os.Stdout, fn)
}

func capture(t *testing.T, target **os.File, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := *target
	*target = w
	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			sb.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- sb.String()
	}()
	fn()
	_ = w.Close()
	*target = saved
	return <-done
}
