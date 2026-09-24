package main

// #103 and #115: a lock file a finished operation created and did not remove
// is a file a person auditing a machine finds and cannot place. The gate
// always named itself as safe to delete; these are the rest of them.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const leftoverHeading = "Left, and safe to delete"

// updateLockFor is what an upgrade leaves beside the installation's binary.
func updateLockFor(s *setupSandbox) string {
	return filepath.Join(s.home, "bin", binaryNameFor()) + updateLockSuffix
}

// allSevenLocks puts every lock file a DropinMiner command leaves on disk and
// returns them in the order the summary names them. The gate, setup.lock,
// connect.lock and flush.lock are there from the real setup this sandbox ran;
// the update lock is written because no upgrade runs in a test sandbox (#103,
// #115); the wallet's creation lock is taken and released through
// lockWalletDir itself; and the refresh-token lock, which only an OAuth
// refresh takes, is written in the state directory operationLockPaths puts
// connect.lock in, under the name TestTheRefreshLockNameIsPkgAuths pins (#136).
func allSevenLocks(t *testing.T, s *setupSandbox) []string {
	t.Helper()
	update := updateLockFor(s)
	writeFileT(t, update, "")
	connectLock, flushLock, err := operationLockPaths(s.home, s.getenv)
	if err != nil {
		t.Fatal(err)
	}
	refresh := filepath.Join(filepath.Dir(connectLock), refreshTokenLockFile)
	writeFileT(t, refresh, "")
	walletDir := filepath.Join(s.home, "wallet")
	release, err := lockWalletDir(walletDir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	release()

	want := []string{
		lifecycleGatePath(s.home),
		filepath.Join(s.home, setupLockFile),
		connectLock,
		flushLock,
		update,
		refresh,
		filepath.Join(walletDir, walletLockFile),
	}
	for _, p := range want {
		if !lexists(p) {
			t.Fatalf("this case is meant to find %s already there, and it is not, so naming it would prove nothing", p)
		}
	}
	return want
}

func TestUninstallNamesEveryLockItLeavesAsSafeToDelete(t *testing.T) {
	for _, purge := range []bool{false, true} {
		name := "plain"
		if purge {
			name = "purge-state"
		}
		t.Run(name, func(t *testing.T) {
			s := installed(t)
			want := allSevenLocks(t, s)

			var code int
			var out, errOut string
			if purge {
				code, out, errOut = s.uninstall(t, tty(walletFixtureAddress(t)), true, nil, "-purge-state", "-home", s.home)
			} else {
				code, out, errOut = s.uninstall(t, nil, false, nil, "-yes", "-home", s.home)
			}
			if code != exitOK {
				t.Fatalf("uninstall: exit %d\n%s\n%s", code, out, errOut)
			}
			// Scoped to the section, not the whole output: -purge-state's
			// own plan prints the full path of everything it removes, so a
			// search over all of it would match the removal line and call
			// that "named as left behind".
			idx := strings.Index(out, leftoverHeading)
			if idx < 0 {
				t.Fatalf("the summary does not name what it left:\n%s", out)
			}
			section := out[idx:]
			// The invariant, either way round: a lock still on disk is
			// named, and one that is gone is not. -purge-state destroys
			// state/, so connect.lock is legitimately absent there and
			// must not be listed.
			for _, p := range want {
				switch named := strings.Contains(section, p); {
				case lexists(p) && !named:
					t.Errorf("the summary does not name %s, which is still there:\n%s", p, out)
				case !lexists(p) && named:
					t.Errorf("the summary names %s, which the run removed:\n%s", p, out)
				}
			}
			// -purge-state destroys state/ and wallet/, and the three locks
			// inside them go with those directories.
			if purge {
				for _, p := range []string{want[2], want[5], want[6]} {
					if lexists(p) {
						t.Errorf("-purge-state left %s behind", p)
					}
				}
			}
		})
	}
}

// #129: the dry run is the form a participant reads before deciding, so it
// says what the real run says about what will still be there. It listed
// nothing, in both modes, because sayLeftoverLocks was reached only from
// closing(), which a dry run does not reach.
func TestTheDryRunNamesTheSameLocksTheRealRunWould(t *testing.T) {
	for _, purge := range []bool{false, true} {
		name := "plain"
		if purge {
			name = "purge-state"
		}
		t.Run(name, func(t *testing.T) {
			s := installed(t)
			want := allSevenLocks(t, s)

			args := []string{"-dry-run", "-home", s.home}
			if purge {
				args = append([]string{"-purge-state"}, args...)
			}
			code, out, errOut := s.uninstall(t, nil, false, nil, args...)
			if code != exitOK {
				t.Fatalf("dry run: exit %d\n%s\n%s", code, out, errOut)
			}
			// Scoped to the section, as the real run's case is and for the
			// same reason: -purge-state's plan prints the full path of
			// everything it would remove, so a search over the whole output
			// would match a removal line and call that "named as left".
			idx := strings.Index(out, leftoverHeading)
			if idx < 0 {
				t.Fatalf("the dry run does not say what it would leave:\n%s", out)
			}
			section := out[idx:]
			for _, p := range want {
				if !strings.Contains(section, p) {
					t.Errorf("the dry run does not name %s, which is there now:\n%s", p, out)
				}
			}
			// A dry run changes nothing, the locks included.
			for _, p := range want {
				if !lexists(p) {
					t.Errorf("the dry run removed %s", p)
				}
			}
		})
	}
}

// And it says nothing where there is nothing to say: the section is computed
// from the files present, not printed as a fixed paragraph.
func TestTheDryRunNamesNoLocksWhenThereAreNone(t *testing.T) {
	s := installed(t)
	for _, p := range []string{
		lifecycleGatePath(s.home),
		filepath.Join(s.home, setupLockFile),
		connectLockPath(filepath.Join(s.home, "state")),
	} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if connectLock, flushLock, err := operationLockPaths(s.home, s.getenv); err == nil {
		for _, p := range []string{connectLock, flushLock} {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
		}
	}
	code, out, errOut := s.uninstall(t, nil, false, nil, "-dry-run", "-home", s.home)
	if code != exitOK {
		t.Fatalf("dry run: exit %d\n%s\n%s", code, out, errOut)
	}
	if strings.Contains(out, leftoverHeading) {
		t.Errorf("the dry run printed the section with no lock file on disk:\n%s", out)
	}
}

// A lock that is gone is not named: uninstall -binary takes the update lock
// with the binary, and a summary that still listed it would send a
// participant looking for a file that is not there.
func TestTheSummaryNamesNoLockThatIsAlreadyGone(t *testing.T) {
	s := installed(t)
	update := updateLockFor(s)
	if lexists(update) {
		t.Fatalf("no upgrade ran in this sandbox and %s exists; the case below would prove nothing", update)
	}
	code, out, errOut := s.uninstall(t, nil, false, nil, "-yes", "-home", s.home)
	if code != exitOK {
		t.Fatalf("uninstall: exit %d\n%s\n%s", code, out, errOut)
	}
	idx := strings.Index(out, leftoverHeading)
	if idx < 0 {
		t.Fatalf("the summary does not name what it left:\n%s", out)
	}
	section := out[idx:]
	if strings.Contains(section, update) {
		t.Errorf("the summary named an update lock that was never there:\n%s", out)
	}
	if !strings.Contains(section, lifecycleGatePath(s.home)) {
		t.Errorf("the gate is still left and must still be named:\n%s", out)
	}
}

// listSection is out from the first line starting with heading up to the
// next blank line: one list of the output, so a path is counted in the list
// it belongs to and not in the plan or in the other list.
func listSection(t *testing.T, out, heading string) string {
	t.Helper()
	i := strings.Index(out, heading)
	if i < 0 {
		t.Fatalf("no %q in the output:\n%s", heading, out)
	}
	rest := out[i:]
	if j := strings.Index(rest, "\n\n"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// timesListed is how many lines of a section name exactly p.
func timesListed(sec, p string) int {
	n := 0
	for _, line := range strings.Split(sec, "\n") {
		if line == "  "+p {
			n++
		}
	}
	return n
}

// #134: a default uninstall prints the purge set as what REMAINS, and named
// flush.lock there whenever its path could be predicted -- a prediction that
// is -purge-state's, whose exclusion is the only one that creates the file.
// The "safe to delete" summary in the same output only ever named it when it
// was there, so the two lists disagreed about one file. Both now apply the
// same test, and the dry run's summary says what the real run's does.
func TestTheRemainsListNamesFlushLockOnlyWhenItIsThere(t *testing.T) {
	for _, present := range []bool{false, true} {
		name := "absent"
		if present {
			name = "present"
		}
		t.Run(name, func(t *testing.T) {
			s := installed(t)
			_, flushLock, err := operationLockPaths(s.home, s.getenv)
			if err != nil || flushLock == "" {
				t.Fatalf("this installation must have a flush lock to predict: %q, %v", flushLock, err)
			}
			if !present {
				if err := os.Remove(flushLock); err != nil {
					t.Fatal(err)
				}
			}
			if lexists(flushLock) != present {
				t.Fatalf("the case is meant to start with %s present=%v", flushLock, present)
			}
			want := 0
			if present {
				want = 1
			}

			code, dry, errOut := s.uninstall(t, nil, false, nil, "-dry-run", "-home", s.home)
			if code != exitOK {
				t.Fatalf("dry run: exit %d\n%s\n%s", code, dry, errOut)
			}
			dryNamed := 0
			if strings.Contains(dry, leftoverHeading) {
				dryNamed = timesListed(listSection(t, dry, leftoverHeading), flushLock)
			}

			code, out, errOut := s.uninstall(t, nil, false, nil, "-yes", "-home", s.home)
			if code != exitOK {
				t.Fatalf("uninstall: exit %d\n%s\n%s", code, out, errOut)
			}
			if lexists(flushLock) != present {
				t.Fatalf("a default uninstall changed whether %s exists (present before: %v)", flushLock, present)
			}
			remains := timesListed(listSection(t, out, "Your participant state remains at"), flushLock)
			summary := timesListed(listSection(t, out, leftoverHeading), flushLock)
			if remains != want || summary != want || dryNamed != want {
				t.Errorf("%s (present=%v) is named %d time(s) in the remains list, %d in the summary and %d in the dry run's summary; want %d in each:\n%s\n--- dry run ---\n%s",
					flushLock, present, remains, summary, dryNamed, want, out, dry)
			}
		})
	}
}

// -purge-state's prediction is unchanged: its exclusion creates an absent
// flush lock and its purge removes it, so the plan still names it.
func TestThePurgePlanStillNamesAnAbsentFlushLock(t *testing.T) {
	s := installed(t)
	_, flushLock, err := operationLockPaths(s.home, s.getenv)
	if err != nil || flushLock == "" {
		t.Fatalf("this installation must have a flush lock to predict: %q, %v", flushLock, err)
	}
	if err := os.Remove(flushLock); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := s.uninstall(t, nil, false, nil, "-purge-state", "-dry-run", "-home", s.home)
	if code != exitOK {
		t.Fatalf("dry run: exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "\n  remove "+flushLock+"\n") {
		t.Errorf("the -purge-state plan no longer names the flush lock its own exclusion would create and remove:\n%s", out)
	}
}

// #136: pkg/auth does not export the refresh-token lock's name, so
// uninstall.go spells it; this holds that spelling to the one the lock is
// actually taken under. A source read, because the constant is unexported.
func TestTheRefreshLockNameIsPkgAuths(t *testing.T) {
	path := filepath.Join("..", "..", "pkg", "auth", "refreshlock.go")
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := ""
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, id := range vs.Names {
			if id.Name == "refreshLockFile" && i < len(vs.Values) {
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok {
					found, _ = strconv.Unquote(lit.Value)
				}
			}
		}
		return false
	})
	if found == "" {
		t.Fatalf("no string constant refreshLockFile in %s", path)
	}
	if found != refreshTokenLockFile {
		t.Errorf("pkg/auth takes the refresh-token lock as %q; uninstall names %q", found, refreshTokenLockFile)
	}
}
