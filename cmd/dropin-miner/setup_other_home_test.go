package main

// #84: `setup -home <dir>` does not reach past the installation it names.
//
// It is the documented way to make a disposable installation for a
// destructive test, and in v0.2.9 it still planned the real user's ~/.claude,
// ~/.codex, ~/.cursor, Pi and Hermes files and the user PATH and
// TOKENDROP_CONFIG — repointing the participant's real agents and environment
// at the scratch config. The Windows tester avoided it only by reading the
// dry run first, which also listed <dir>\bin for PATH while planning no
// binary there.
//
// Every case snapshots the whole sandbox, as installer_test.go does, because
// the claim is about what is NOT touched: the agents' own directories and the
// shell profile are inside the fake home too.

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// defaultInstallationPresent is a sandbox whose own installation is fully set
// up — profile, agents and all — which is the machine a disposable
// installation is made on.
func defaultInstallationPresent(t *testing.T) *setupSandbox {
	t.Helper()
	s := newSetupSandbox(t)
	s.platform.claim("credits")
	s.onPath["claude"] = true
	s.onPath["codex"] = true
	if code, out, errOut := s.run(nil, false, "-yes"); code != exitOK {
		t.Fatalf("setting up the default installation: exit %d\n%s\n%s", code, out, errOut)
	}
	if !lexists(s.paths().claudeSkill) {
		t.Fatal("the default installation has no agents; the cases below would prove nothing")
	}
	return s
}

func userEnvSnapshot(s *setupSandbox) string {
	var b strings.Builder
	for _, k := range []string{"Path", "TOKENDROP_CONFIG"} {
		b.WriteString(k + "=" + s.userEnv.values[k] + "\n")
	}
	return b.String()
}

func TestSetupForAnotherHomeLeavesTheProfileAndTheAgentsAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		tty  bool
	}{
		{"no flags, no terminal", nil, false},
		{"-yes does not override it", []string{"-yes"}, false},
		{"-yes -with does not override it", []string{"-yes", "-with", "claude", "-with", "cursor"}, false},
		{"at a terminal, nothing is even asked", nil, true},
		{"a dry run lists both as skipped too", []string{"-dry-run"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := defaultInstallationPresent(t)
			scratch := filepath.Join(s.root, "dm-scratch")
			before, envBefore := snapshotTree(t, s.root), userEnvSnapshot(s)

			var stdin *ttyReader
			if tc.tty {
				stdin = tty("n") // the mining question; nothing else may be asked
			}
			code, out, errOut := s.run(stdin, tc.tty, append([]string{"-home", scratch}, tc.args...)...)
			if code != exitOK {
				t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
			}

			// Nothing outside the scratch installation changed: no agent
			// file, no profile, and (Windows) no user environment variable.
			assertOwnership(t, before, snapshotTree(t, s.root), scratch)
			if got := userEnvSnapshot(s); got != envBefore {
				t.Errorf("the user environment was changed:\n%s", got)
			}

			// Both steps are named as skipped, with the reason.
			if n := strings.Count(out, "Skipped: -home names "+scratch+", which is not this machine's installation ("+s.home+")"); n != 2 {
				t.Errorf("want both steps named as skipped with the reason, got %d:\n%s", n, out)
			}
			if strings.Contains(out, "[Y/n]") {
				t.Errorf("setup asked about something that is not this installation's:\n%s", out)
			}
			// The dry run used to list <scratch>/bin for PATH while planning
			// no binary there.
			if strings.Contains(out, filepath.Join(scratch, "bin")) {
				t.Errorf("the plan names %s, where nothing will be:\n%s", filepath.Join(scratch, "bin"), out)
			}
			if tc.args != nil && tc.args[0] == "-dry-run" {
				return
			}
			// The closing line names the command that does configure agents
			// for that installation, and not "setup -yes", which would not.
			want := "agents install -config " + filepath.Join(scratch, setupConfigFile)
			if !strings.Contains(out, want) {
				t.Errorf("the closing line did not name %q:\n%s", want, out)
			}
			if strings.Contains(out, "Finish with") {
				t.Errorf("the closing line says to finish with setup -yes, which does not override this:\n%s", out)
			}
			if !lexists(filepath.Join(scratch, setupConfigFile)) {
				t.Errorf("the scratch installation itself was not set up:\n%s", out)
			}
		})
	}
}

// A -home equal to the default is unchanged in every respect: spelled with
// the flag or not, the same files, the same words.
func TestSetupWithTheDefaultHomeNamedIsTheSameAsWithoutIt(t *testing.T) {
	type result struct {
		out   string
		files map[string]fileSig
	}
	run := func(homeArg func(s *setupSandbox) []string) result {
		s := newSetupSandbox(t)
		s.platform.claim("credits")
		s.onPath["claude"] = true
		code, out, errOut := s.run(nil, false, append(homeArg(s), "-yes")...)
		if code != exitOK {
			t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
		}
		// Everything that differs between two sandboxes is their root and
		// their stub servers' ports; with those named, the rest must match.
		// On Windows the root is spelled at several escaping depths: as it
		// is, doubled inside TOML and JSON strings, and doubled again where a
		// quoted command sits inside a JSON string (settings.json). Chasing
		// each depth found two and missed the third, so every run of
		// backslashes is folded to one slash first — in the text and in the
		// root alike — and only then is the root named.
		slashes := regexp.MustCompile(`\\+`)
		fold := func(x string) string { return slashes.ReplaceAllString(x, "/") }
		norm := func(x string) string {
			x = strings.ReplaceAll(fold(x), fold(s.root), "<root>")
			x = strings.ReplaceAll(x, s.platform.srv.URL, "<platform>")
			return strings.ReplaceAll(x, s.as.srv.URL, "<as>")
		}
		files := map[string]fileSig{}
		for p, sig := range snapshotTree(t, s.userHome) {
			rel := norm(p)
			if within(p, filepath.Join(s.home, "state")) && !sig.dir {
				sig = fileSig{} // identity and timestamps: present or absent is what is compared
			}
			if !sig.dir && !within(p, filepath.Join(s.home, "state")) {
				sig.sum = [32]byte{}
				sig.link = norm(string(s.readFile(p)))
			}
			files[rel] = sig
		}
		return result{norm(out), files}
	}

	without := run(func(*setupSandbox) []string { return nil })
	for name, arg := range map[string]func(s *setupSandbox) []string{
		"named exactly":         func(s *setupSandbox) []string { return []string{"-home", s.home} },
		"with a trailing slash": func(s *setupSandbox) []string { return []string{"-home", s.home + string(filepath.Separator)} },
		"through a dot-dot path": func(s *setupSandbox) []string {
			return []string{"-home", filepath.Join(s.home, "..", filepath.Base(s.home))}
		},
	} {
		t.Run(name, func(t *testing.T) {
			with := run(arg)
			if with.out != without.out {
				t.Errorf("the output differs:\n--- without -home ---\n%s\n--- with ---\n%s", without.out, with.out)
			}
			for p, sig := range without.files {
				if got, ok := with.files[p]; !ok {
					t.Errorf("-home <default> did not write %s", p)
				} else if got != sig {
					t.Errorf("-home <default> wrote %s differently", p)
				}
			}
			for p := range with.files {
				if _, ok := without.files[p]; !ok {
					t.Errorf("-home <default> wrote %s, which no -home does not", p)
				}
			}
		})
	}
}

// The default is $TOKENDROP_HOME when that is set: `TOKENDROP_HOME=<dir>
// install.sh` stays the way to put the machine's installation somewhere else,
// and naming that same directory with -home is still the default.
func TestOtherInstallationIsDecidedAgainstTheDefault(t *testing.T) {
	// Absolute, as production passes them: run() makes home absolute before
	// it asks, and on Windows a rooted path with no drive is not absolute —
	// the first version of this case compared `\home\u` with `D:\home\u`.
	abs := func(elem ...string) string {
		p, err := filepath.Abs(filepath.Join(elem...))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	user := abs(string(filepath.Separator)+"home", "u")
	dot := filepath.Join(user, ".tokendrop")
	elsewhere := abs(string(filepath.Separator)+"srv", "td")
	for _, tc := range []struct {
		name                    string
		flag, home, env, userHm string
		wantDefault             string
		wantOther               bool
	}{
		{"no -home: never another installation", "", elsewhere, elsewhere, user, "", false},
		{"-home is ~/.tokendrop", dot, dot, "", user, dot, false},
		{"-home is somewhere else", elsewhere, elsewhere, "", user, dot, true},
		{"-home is what TOKENDROP_HOME names", elsewhere, elsewhere, elsewhere, user, elsewhere, false},
		{"TOKENDROP_HOME names one place and -home another", dot, dot, elsewhere, user, elsewhere, true},
		{"no default can be named: nothing is withheld", elsewhere, elsewhere, "", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def, other := otherInstallation(tc.flag, tc.home, tc.env, tc.userHm)
			if other != tc.wantOther || (tc.wantDefault != "" && !samePath(def, tc.wantDefault)) {
				t.Fatalf("otherInstallation = (%q, %v), want (%q, %v)", def, other, tc.wantDefault, tc.wantOther)
			}
		})
	}
}

// A default reached through a link is still the default.
func TestADefaultHomeReachedThroughALinkIsStillTheDefault(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink needs a privilege the Windows runners do not grant")
	}
	root := t.TempDir()
	real := filepath.Join(root, "real")
	link := filepath.Join(root, "link")
	writeFileT(t, filepath.Join(real, "x"), "")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if !sameInstallationDir(link, real) {
		t.Error("a link to the default installation was taken for another installation")
	}
	if sameInstallationDir(filepath.Join(root, "other"), real) {
		t.Error("a directory that does not exist was taken for the default installation")
	}
}

// Uninstall's restore hint (L6) says "run setup -home <dir>". For a home that
// is not this machine's default that alone no longer restores the profile or
// the agents, so the hint has to finish the sentence — and must not start it
// for the default installation, where setup -home does restore everything.
func TestUninstallsRestoreHintIsCompleteForAnotherHome(t *testing.T) {
	s := defaultInstallationPresent(t)
	scratch := filepath.Join(s.root, "dm-scratch")
	if code, out, errOut := s.run(nil, false, "-home", scratch); code != exitOK {
		t.Fatalf("setup -home: exit %d\n%s\n%s", code, out, errOut)
	}

	code, out, errOut := s.uninstall(t, nil, false, nil, "-yes", "-home", scratch)
	if code != exitOK {
		t.Fatalf("uninstall -home: exit %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{
		"setup -home " + scratch,
		"is not this machine's default installation (" + s.home + ")",
		"agents install -config " + filepath.Join(scratch, setupConfigFile),
		"TOKENDROP_HOME set to it",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the restore hint for another home did not say %q:\n%s", want, out)
		}
	}

	code, out, errOut = s.uninstall(t, nil, false, nil, "-yes")
	if code != exitOK {
		t.Fatalf("uninstall: exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "setup -home "+s.home) || strings.Contains(out, "is not this machine's default installation") {
		t.Errorf("the restore hint for the default installation changed:\n%s", out)
	}
}

// And setup's own closing line says how to name a directory elsewhere as the
// machine's installation, since -home alone now deliberately does not.
func TestSetupsClosingForAnotherHomeNamesTokendropHome(t *testing.T) {
	s := defaultInstallationPresent(t)
	scratch := filepath.Join(s.root, "dm-scratch")
	code, out, errOut := s.run(nil, false, "-yes", "-home", scratch)
	if code != exitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "run\nsetup with TOKENDROP_HOME set to it instead of -home") {
		t.Errorf("the closing line did not say how to name this directory as the machine's installation:\n%s", out)
	}
}
