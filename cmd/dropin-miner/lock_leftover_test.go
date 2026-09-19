package main

// #103 and #115: a lock file a finished operation created and did not remove
// is a file a person auditing a machine finds and cannot place. The gate
// always named itself as safe to delete; these are the rest of them.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const leftoverHeading = "Left, and safe to delete"

// updateLockFor is what an upgrade leaves beside the installation's binary.
func updateLockFor(s *setupSandbox) string {
	return filepath.Join(s.home, "bin", binaryNameFor()) + updateLockSuffix
}

func TestUninstallNamesEveryLockItLeavesAsSafeToDelete(t *testing.T) {
	for _, purge := range []bool{false, true} {
		name := "plain"
		if purge {
			name = "purge-state"
		}
		t.Run(name, func(t *testing.T) {
			s := installed(t)
			// What an upgrade leaves in bin/ and never removes (#103), and
			// the two an ordinary operation leaves behind (#115). setup.lock
			// and connect.lock are already there from the real setup this
			// sandbox ran; the update lock is written here because no
			// upgrade runs in a test sandbox.
			update := updateLockFor(s)
			writeFileT(t, update, "")

			want := []string{
				lifecycleGatePath(s.home),
				filepath.Join(s.home, setupLockFile),
				connectLockPath(filepath.Join(s.home, "state")),
				update,
			}
			for _, p := range want {
				if !lexists(p) {
					t.Fatalf("this case is meant to find %s already there, and it is not, so naming it would prove nothing", p)
				}
			}

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
			if purge && lexists(want[2]) {
				t.Errorf("-purge-state left %s behind", want[2])
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
			update := updateLockFor(s)
			writeFileT(t, update, "")

			want := []string{
				lifecycleGatePath(s.home),
				filepath.Join(s.home, setupLockFile),
				connectLockPath(filepath.Join(s.home, "state")),
				update,
			}
			for _, p := range want {
				if !lexists(p) {
					t.Fatalf("this case is meant to find %s already there, and it is not, so naming it would prove nothing", p)
				}
			}

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
