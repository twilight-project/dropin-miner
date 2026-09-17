package main

// The platform-independent half of wallet_acl.go, on every OS: the judgment
// of an access list, the walk, the repair decision, where setup, adoption,
// wallet creation and doctor use them. A fake backend stands in for the
// Windows DACL calls and records every object it is asked to protect;
// wallet_acl_windows_test.go proves the same rules against what Windows
// actually enforces.

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// fakeWalletACL is a managed backend over a map. An object it has no entry for
// is what a wallet written before this change is: owner-only by inheritance,
// not protected. protect also applies the real restrictToOwner, so a moved or
// created object keeps the modes the rest of setup expects.
type fakeWalletACL struct {
	state     map[string]walletObjectAccess
	failOn    map[string]error
	protected []string // "dir:<path>" or "file:<path>", in call order
	onProtect func(path string)
	inspected int
}

func useFakeWalletACL(t *testing.T) *fakeWalletACL {
	t.Helper()
	f := &fakeWalletACL{state: map[string]walletObjectAccess{}, failOn: map[string]error{}}
	saved := walletACL
	walletACL = walletACLBackend{managed: true, inspect: f.inspect, protect: f.protect, isLink: saved.isLink}
	t.Cleanup(func() { walletACL = saved })
	return f
}

func (f *fakeWalletACL) inspect(path string) (walletObjectAccess, error) {
	f.inspected++
	if s, ok := f.state[path]; ok {
		return s, nil
	}
	return walletObjectAccess{ownerOnly: true}, nil
}

func (f *fakeWalletACL) protect(path string, dir bool) error {
	kind := "file:"
	if dir {
		kind = "dir:"
	}
	f.protected = append(f.protected, kind+path)
	if f.onProtect != nil {
		f.onProtect(path)
	}
	if err := f.failOn[path]; err != nil {
		return err
	}
	if err := restrictToOwner(path, dir); err != nil {
		return err
	}
	f.state[path] = walletObjectAccess{protected: true, ownerOnly: true}
	return nil
}

func (f *fakeWalletACL) reset() { f.protected = nil }

// An inherited entry grants what an explicit one does. The entry another
// program adds to the installation directory reaches a wallet file as an
// inherited one, so a judgment that skipped inherited entries would report
// that file as private.
func TestWalletACLJudgmentCountsInheritedEntries(t *testing.T) {
	const owner, other, third = "S-1-5-21-1-2-3-1001", "S-1-5-5-0-4242", "S-1-5-32-545"
	const (
		inherited   = 0x10
		inheritOnly = 0x08
		readExecute = 0x001200A9
		traverse    = 0x00000020
	)
	cases := []struct {
		name          string
		aces          []walletACE
		wantOwnerOnly bool
		wantReaders   []string
	}{
		{"owner only", []walletACE{{allow: true, mask: 0x001F01FF, sid: owner}, {allow: true, flags: 0x0B, mask: walletGenericAll, sid: owner}}, true, nil},
		{"an inherited read entry", []walletACE{{allow: true, mask: 0x001F01FF, sid: owner}, {allow: true, flags: inherited, mask: readExecute, sid: other}}, false, []string{other}},
		{"an explicit read entry", []walletACE{{allow: true, mask: 0x001F01FF, sid: owner}, {allow: true, mask: readExecute, sid: other}}, false, []string{other}},
		{"generic read, inherit-only on a directory", []walletACE{{allow: true, flags: inheritOnly | 0x03, mask: walletGenericRead, sid: other}}, false, []string{other}},
		{"generic all", []walletACE{{allow: true, flags: inherited, mask: walletGenericAll, sid: third}}, false, []string{third}},
		{"traverse only reads nothing", []walletACE{{allow: true, mask: 0x001F01FF, sid: owner}, {allow: true, flags: inherited, mask: traverse, sid: other}}, false, nil},
		{"a deny entry is not a reader", []walletACE{{allow: true, mask: 0x001F01FF, sid: owner}, {allow: false, mask: readExecute, sid: other}}, false, nil},
		{"each principal once, sorted", []walletACE{{allow: true, flags: inherited, mask: readExecute, sid: other}, {allow: true, mask: readExecute, sid: third}, {allow: true, flags: inherited, mask: readExecute, sid: other}}, false, []string{third, other}},
		{"an empty list is not owner-only", nil, false, nil},
	}
	for _, c := range cases {
		ownerOnly, readers := judgeWalletACEs(c.aces, owner)
		if ownerOnly != c.wantOwnerOnly || !reflect.DeepEqual(readers, c.wantReaders) {
			t.Errorf("%s: ownerOnly=%v readers=%v, want ownerOnly=%v readers=%v", c.name, ownerOnly, readers, c.wantOwnerOnly, c.wantReaders)
		}
	}
}

// The repair secures every directory and regular file, a directory before its
// contents, skips what is already protected and owner-only, and does nothing
// the second time.
func TestSecureWalletTreeSecuresEveryObjectOnce(t *testing.T) {
	fake := useFakeWalletACL(t)
	dir := filepath.Join(t.TempDir(), "wallet")
	key, pub := filepath.Join(dir, walletKeyFile), filepath.Join(dir, walletSidecarFile)
	sub, nested := filepath.Join(dir, "sub"), filepath.Join(dir, "sub", "nested.json")
	writeFileT(t, key, "sealed")
	writeFileT(t, pub, "{}")
	writeFileT(t, nested, "{}")
	fake.state[pub] = walletObjectAccess{protected: true, ownerOnly: true}
	fake.state[key] = walletObjectAccess{readers: []string{"S-1-5-5-0-4242"}}

	changed, err := secureWalletTree(dir, fake.protect)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	want := []string{"dir:" + dir, "dir:" + sub, "file:" + nested, "file:" + key}
	if !sameMembers(fake.protected, want) {
		t.Fatalf("protected %v, want exactly %v", fake.protected, want)
	}
	if indexOf(fake.protected, "dir:"+dir) != 0 || indexOf(fake.protected, "dir:"+sub) > indexOf(fake.protected, "file:"+nested) {
		t.Errorf("a directory was not secured before its contents: %v", fake.protected)
	}

	fake.reset()
	changed, err = secureWalletTree(dir, fake.protect)
	if err != nil || changed || len(fake.protected) != 0 {
		t.Errorf("second pass: changed=%v err=%v protected=%v, want nothing to do", changed, err, fake.protected)
	}
	if changed, err := secureWalletTree(filepath.Join(t.TempDir(), "absent"), fake.protect); changed || err != nil {
		t.Errorf("an absent wallet: changed=%v err=%v", changed, err)
	}
}

// An object that cannot be secured is a failure naming it, and the walk still
// secures everything else first.
func TestSecureWalletTreeFailsOnWhatItCannotSecureAndSecuresTheRest(t *testing.T) {
	t.Run("restriction refused", func(t *testing.T) {
		fake := useFakeWalletACL(t)
		dir := filepath.Join(t.TempDir(), "wallet")
		key, pub := filepath.Join(dir, walletKeyFile), filepath.Join(dir, walletSidecarFile)
		writeFileT(t, key, "sealed")
		writeFileT(t, pub, "{}")
		fake.failOn[key] = errors.New("injected: access denied")

		_, err := secureWalletTree(dir, fake.protect)
		if err == nil || !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "injected: access denied") {
			t.Fatalf("err = %v, want one naming %s and the cause", err, key)
		}
		if indexOf(fake.protected, "file:"+pub) < 0 {
			t.Errorf("the walk stopped at the failure instead of securing the rest: %v", fake.protected)
		}
	})
	t.Run("a link", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("creating symlinks needs a privilege the Windows runner does not grant; the Windows junction case is TestSetupRefusesAReparsePointInTheWallet")
		}
		fake := useFakeWalletACL(t)
		root := t.TempDir()
		dir := filepath.Join(root, "wallet")
		key := filepath.Join(dir, walletKeyFile)
		writeFileT(t, key, "sealed")
		elsewhere := filepath.Join(root, "elsewhere")
		writeFileT(t, filepath.Join(elsewhere, "secret.txt"), "not the wallet's")
		link := filepath.Join(dir, "link")
		if err := os.Symlink(elsewhere, link); err != nil {
			t.Fatal(err)
		}

		_, err := secureWalletTree(dir, fake.protect)
		for _, p := range fake.protected {
			if strings.Contains(p, elsewhere) || strings.HasPrefix(strings.TrimPrefix(strings.TrimPrefix(p, "dir:"), "file:"), link) {
				t.Errorf("the repair reached through or onto the link: %s", p)
			}
		}
		if err == nil || !strings.Contains(err.Error(), link) || !strings.Contains(err.Error(), "not followed") {
			t.Fatalf("err = %v, want one naming the link %s", err, link)
		}
		if indexOf(fake.protected, "file:"+key) < 0 {
			t.Errorf("the wallet key beside the link was not secured: %v", fake.protected)
		}
		facts := inspectWalletAccess(dir)
		if !reflect.DeepEqual(facts.Unsecurable, []string{link}) {
			t.Errorf("doctor's view of the link: %v", facts.Unsecurable)
		}
	})
}

// Where access lists are not managed, nothing is inspected or protected, and
// doctor has no wallet check.
func TestUnmanagedWalletACLDoesNothing(t *testing.T) {
	fake := useFakeWalletACL(t)
	walletACL.managed = false
	dir := filepath.Join(t.TempDir(), "wallet")
	writeFileT(t, filepath.Join(dir, walletKeyFile), "sealed")
	if changed, err := secureWalletTree(dir, fake.protect); changed || err != nil {
		t.Errorf("changed=%v err=%v", changed, err)
	}
	if _, err := openWalletDir(filepath.Join(t.TempDir(), "new-wallet"), func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}
	if err := writeWalletFile(dir, walletSidecarFile, &sidecar{Address: "twilight1x"}); err != nil {
		t.Fatal(err)
	}
	if f := inspectWalletAccess(dir); f.Checked {
		t.Errorf("an unmanaged platform checked wallet access: %+v", f)
	}
	if len(fake.protected) != 0 || fake.inspected != 0 {
		t.Errorf("protected %v, inspected %d; want nothing", fake.protected, fake.inspected)
	}
}

// A wallet directory openWalletDir creates is protected; one it finds is not
// touched there. Every wallet write protects the directory before the write
// and the file after it.
func TestWalletCreationAndWritesProtectTheWallet(t *testing.T) {
	fake := useFakeWalletACL(t)
	noEnv := func(string) string { return "" }
	dir := filepath.Join(t.TempDir(), "wallet")

	if _, err := openWalletDir(dir, noEnv); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fake.protected, []string{"dir:" + dir}) {
		t.Fatalf("creating the directory protected %v, want exactly the new directory", fake.protected)
	}
	fake.reset()
	if _, err := openWalletDir(dir, noEnv); err != nil {
		t.Fatal(err)
	}
	if len(fake.protected) != 0 {
		t.Errorf("opening an existing directory protected %v", fake.protected)
	}

	pub := filepath.Join(dir, walletSidecarFile)
	fake.onProtect = func(path string) {
		if path == dir && lexists(pub) {
			t.Errorf("the directory was protected only after %s was written", pub)
		}
	}
	if err := writeWalletFile(dir, walletSidecarFile, &sidecar{Address: "twilight1x"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fake.protected, []string{"dir:" + dir, "file:" + pub}) {
		t.Errorf("a write protected %v, want the directory and then the file", fake.protected)
	}
	fake.onProtect = nil

	t.Run("a refused restriction fails the write", func(t *testing.T) {
		fake.failOn[dir] = errors.New("injected: access denied")
		defer delete(fake.failOn, dir)
		err := writeWalletFile(dir, pendingTxFile, &pendingTx{Version: pendingTxVersion, Hash: "ABC"})
		if err == nil || !strings.Contains(err.Error(), "injected") {
			t.Fatalf("err = %v", err)
		}
		if lexists(filepath.Join(dir, pendingTxFile)) {
			t.Error("the file was written into a directory that could not be restricted")
		}
		fresh := filepath.Join(t.TempDir(), "wallet")
		fake.failOn[fresh] = errors.New("injected: access denied")
		if _, err := openWalletDir(fresh, noEnv); err == nil {
			t.Error("a new wallet directory that could not be restricted was opened")
		}
	})
}

// setup secures an existing wallet before connect runs, says so, and finds
// nothing to do the next time.
func TestSetupSecuresAnExistingWalletBeforeConnect(t *testing.T) {
	s := newSetupSandbox(t)
	fake := useFakeWalletACL(t)
	s.restrict = fake.protect
	s.platform.claim("credits")
	if code, out, errOut := s.run(tty("n"), true, "-yes", "-no-agents", "-no-profile"); code != exitOK {
		t.Fatalf("first setup exited %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	wallet := filepath.Join(s.home, "wallet")
	writeInstallation(t, s.home, "wallet")
	fake.reset()
	connectsAtRepair := -1
	fake.onProtect = func(path string) {
		if path == wallet {
			connectsAtRepair = s.connectCalls
		}
	}
	connectsBefore := s.connectCalls

	code, out, errOut := s.run(tty(), true, "-yes", "-no-agents", "-no-profile")
	if code != exitOK {
		t.Fatalf("second setup exited %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	for _, want := range []string{"dir:" + wallet, "file:" + filepath.Join(wallet, walletKeyFile), "file:" + filepath.Join(wallet, walletSidecarFile)} {
		if indexOf(fake.protected, want) < 0 {
			t.Errorf("setup did not secure %s: %v", want, fake.protected)
		}
	}
	if connectsAtRepair != connectsBefore {
		t.Errorf("the wallet was secured after connect ran (connect calls %d at the repair, %d before the run)", connectsAtRepair, connectsBefore)
	}
	if !strings.Contains(out, "Made the wallet in "+wallet+" readable only by you") {
		t.Errorf("setup did not say it secured the wallet:\n%s", out)
	}

	fake.reset()
	fake.onProtect = nil
	code, out, errOut = s.run(tty(), true, "-yes", "-no-agents", "-no-profile")
	if code != exitOK {
		t.Fatalf("third setup exited %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	for _, p := range fake.protected {
		if strings.HasPrefix(strings.TrimPrefix(strings.TrimPrefix(p, "dir:"), "file:"), wallet) {
			t.Errorf("a secured wallet was secured again: %v", fake.protected)
			break
		}
	}
	// Every run here passes -no-profile -no-agents, so the closing line
	// reports them as skipped (#75) rather than "already in place" — that
	// wording is reserved for a run where every step was actually
	// attempted and found nothing to do.
	if strings.Contains(out, "already in place") {
		t.Errorf("closing message claims everything was already in place under -no-profile/-no-agents:\n%s", out)
	}
	if !strings.Contains(out, "setup skipped") {
		t.Errorf("a run with the wallet already secured did not report the flagged-off steps as skipped:\n%s", out)
	}
}

// A wallet object setup cannot secure stops setup before connect, naming it.
func TestSetupStopsWhenAWalletObjectCannotBeSecured(t *testing.T) {
	cases := []struct {
		name string
		// spoil breaks the wallet and returns the path the refusal must name,
		// the words it must use, and a path nothing may be protected under.
		spoil func(t *testing.T, s *setupSandbox, fake *fakeWalletACL, wallet string) (named, words, untouched string)
	}{
		{"a refused restriction", func(t *testing.T, s *setupSandbox, fake *fakeWalletACL, wallet string) (string, string, string) {
			key := filepath.Join(wallet, walletKeyFile)
			fake.failOn[key] = errors.New("injected: access denied")
			return key, "injected: access denied", filepath.Join(s.root, "nothing-here")
		}},
		{"a link", func(t *testing.T, s *setupSandbox, _ *fakeWalletACL, wallet string) (string, string, string) {
			if runtime.GOOS == "windows" {
				t.Skip("creating symlinks needs a privilege the Windows runner does not grant; the Windows junction case is TestSetupRefusesAReparsePointInTheWallet")
			}
			elsewhere := filepath.Join(s.root, "elsewhere")
			writeFileT(t, filepath.Join(elsewhere, "secret.txt"), "not the wallet's")
			link := filepath.Join(wallet, "link")
			if err := os.Symlink(elsewhere, link); err != nil {
				t.Fatal(err)
			}
			return link, "not followed", elsewhere
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newSetupSandbox(t)
			fake := useFakeWalletACL(t)
			s.restrict = fake.protect
			s.platform.claim("credits")
			if code, out, errOut := s.run(tty("n"), true, "-yes", "-no-agents", "-no-profile"); code != exitOK {
				t.Fatalf("first setup exited %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
			}
			wallet := filepath.Join(s.home, "wallet")
			writeInstallation(t, s.home, "wallet")
			named, words, untouched := c.spoil(t, s, fake, wallet)
			fake.reset()
			connectsBefore := s.connectCalls

			code, out, errOut := s.run(tty(), true, "-yes", "-no-agents", "-no-profile")
			for _, p := range fake.protected {
				path := strings.TrimPrefix(strings.TrimPrefix(p, "dir:"), "file:")
				if strings.HasPrefix(path, untouched) || strings.HasPrefix(path, named+string(filepath.Separator)) {
					t.Errorf("setup reached past the wallet to %s", p)
				}
			}
			if code == exitOK {
				t.Fatalf("setup exited 0 over a wallet object it could not secure\nstdout:\n%s\nstderr:\n%s", out, errOut)
			}
			if s.connectCalls != connectsBefore {
				t.Errorf("connect ran after the wallet could not be secured")
			}
			if indexOf(fake.protected, "file:"+filepath.Join(wallet, walletSidecarFile)) < 0 {
				t.Errorf("setup stopped without securing the rest of the wallet: %v", fake.protected)
			}
			for _, want := range []string{named, words, "could not make the wallet in " + wallet + " readable only by you", "connect was not run"} {
				if !strings.Contains(errOut, want) {
					t.Errorf("stderr missing %q:\n%s", want, errOut)
				}
			}
			if strings.Contains(out, "Setup complete.") {
				t.Error("setup narrated completion after the wallet could not be secured")
			}
		})
	}
}

// Adoption secures the wallet it moves as part of the custody transaction: a
// wallet object that cannot be secured is a failed wallet stage, and both the
// identity and the wallet go back where they started.
func TestAdoptionSecuresTheAdoptedWallet(t *testing.T) {
	t.Run("secured", func(t *testing.T) {
		s := newSetupSandbox(t)
		fake := useFakeWalletACL(t)
		s.restrict = fake.protect
		sibling := s.home + ".bak"
		writeInstallation(t, sibling, "wallet")
		_, out, errOut := s.run(tty("y", "n"), true, "-no-agents", "-no-profile")
		if !strings.Contains(out, "adopted the wallet") {
			t.Fatalf("the wallet was not adopted\nstdout:\n%s\nstderr:\n%s", out, errOut)
		}
		wallet := filepath.Join(s.home, "wallet")
		for _, want := range []string{"file:" + filepath.Join(wallet, walletKeyFile), "file:" + filepath.Join(wallet, walletSidecarFile)} {
			if indexOf(fake.protected, want) < 0 {
				t.Errorf("the wallet was not secured: %s missing from %v", want, fake.protected)
			}
		}
		// Adoption secured it, so setup's own step after adoption finds
		// nothing left to do.
		if strings.Contains(out, "Made the wallet in") {
			t.Errorf("the adopted wallet was left for setup's later step to secure:\n%s", out)
		}
	})
	t.Run("refused", func(t *testing.T) {
		s := newSetupSandbox(t)
		fake := useFakeWalletACL(t)
		s.restrict = fake.protect
		sibling := s.home + ".bak"
		writeInstallation(t, sibling, "identity", "wallet")
		fake.failOn[filepath.Join(s.home, "wallet", walletKeyFile)] = errors.New("injected: access denied")
		before := snapshotTree(t, sibling)

		code, out, errOut := s.run(tty("y", "n"), true, "-no-agents", "-no-profile")
		s.assertAdoptionStopped(t, code, out, errOut, "wallet", sibling)
		if !strings.Contains(errOut, "injected: access denied") {
			t.Errorf("the failure report does not carry the cause:\n%s", errOut)
		}
		s.assertCustodyAtSource(t, sibling, before)
	})
}

// doctor's verdicts, from the facts.
func TestDoctorWalletAccessCheck(t *testing.T) {
	dir := filepath.Join("home", "wallet")
	key := filepath.Join(dir, walletKeyFile)
	cases := []struct {
		name        string
		facts       walletAccessFacts
		verdict     doctorVerdict
		detail, fix []string
	}{
		{"no wallet", walletAccessFacts{Checked: true, Dir: dir}, verdictOK, []string{"no wallet in " + dir}, nil},
		{"owner only", walletAccessFacts{Checked: true, Dir: dir, Present: true}, verdictOK, []string{"only you can read " + dir}, nil},
		{"another reader", walletAccessFacts{Checked: true, Dir: dir, Present: true, Readers: []walletReader{{Path: key, Principals: []string{`NT AUTHORITY\LogonSessionId_0_1`}}}},
			verdictNo, []string{key, `NT AUTHORITY\LogonSessionId_0_1`, recurringEntryDetail}, []string{"dropin-miner setup"}},
		{"a link", walletAccessFacts{Checked: true, Dir: dir, Present: true, Unsecurable: []string{filepath.Join(dir, "link")}},
			verdictNo, []string{filepath.Join(dir, "link")}, []string{"move " + filepath.Join(dir, "link"), "dropin-miner setup"}},
		{"unreadable", walletAccessFacts{Checked: true, Dir: dir, Present: true, Err: errors.Join(errors.New("first"), errors.New("second"))},
			verdictUnknown, []string{"could not read", "first; second"}, nil},
		{"a reader outranks what could not be read", walletAccessFacts{Checked: true, Dir: dir, Present: true, Readers: []walletReader{{Path: key, Principals: []string{"S-1-5-5-0-1"}}}, Err: errors.New("x")},
			verdictNo, []string{key, recurringEntryDetail, "could not be checked"}, []string{"dropin-miner setup"}},
	}
	for _, c := range cases {
		got := doctorWalletCheck(c.facts)
		if got.Name != "wallet access" || got.Verdict != c.verdict {
			t.Errorf("%s: %s %s (%s)", c.name, got.Name, got.Verdict, got.Detail)
		}
		for _, want := range c.detail {
			if !strings.Contains(got.Detail, want) {
				t.Errorf("%s: detail %q missing %q", c.name, got.Detail, want)
			}
		}
		if strings.Contains(got.Detail, "\n") {
			t.Errorf("%s: detail spans lines: %q", c.name, got.Detail)
		}
		if len(c.fix) == 0 && got.Fix != "" {
			t.Errorf("%s: offers a fix %q", c.name, got.Fix)
		}
		for _, want := range c.fix {
			if !strings.Contains(got.Fix, want) {
				t.Errorf("%s: fix %q missing %q", c.name, got.Fix, want)
			}
		}
	}

	facts := healthyFacts()
	if checks := assembleDoctor(facts); checks[len(checks)-1].Name == "wallet access" {
		t.Error("a wallet access check appeared where wallet access was not checked")
	}
	facts.Wallet = walletAccessFacts{Checked: true, Dir: dir, Present: true, Readers: []walletReader{{Path: key, Principals: []string{"S-1-5-5-0-1"}}}}
	checks := assembleDoctor(facts)
	last := checks[len(checks)-1]
	if last.Name != "wallet access" || last.Verdict != verdictNo {
		t.Fatalf("last check = %s %s, want wallet access NO", last.Name, last.Verdict)
	}
	var buf strings.Builder
	emitMachine(&buf, doctorEnvelope(facts, checks, doctorExit(checks)))
	data := dataOf(t, decodeCommandEnvelope(t, buf.String(), "doctor"))
	raw, _ := data["checks"].([]any)
	var lastJSON map[string]any
	if len(raw) > 0 {
		lastJSON, _ = raw[len(raw)-1].(map[string]any)
	}
	if lastJSON["name"] != "wallet access" || lastJSON["verdict"] != string(verdictNo) || lastJSON["fix"] != "dropin-miner setup" {
		t.Errorf("doctor -json does not carry the wallet access check and its fix: %s", buf.String())
	}
	// The JSON detail is the text detail: a reader reaching a machine through
	// -json learns the entry can come back exactly as one at a terminal does.
	jsonDetail, _ := lastJSON["detail"].(string)
	if jsonDetail != last.Detail || !strings.Contains(jsonDetail, recurringEntryDetail) {
		t.Errorf("doctor -json detail = %q, want the text detail carrying %q", jsonDetail, recurringEntryDetail)
	}
}

// recurringEntryDetail is what a reader must be told besides who and the fix:
// the entry can be added again, and setup is what re-applies the protection.
const recurringEntryDetail = "another program or a machine policy can add such an entry again, and dropin-miner setup re-applies the protection whenever it does"

// doctor reads the wallet and changes nothing.
func TestInspectWalletAccessOnlyReads(t *testing.T) {
	fake := useFakeWalletACL(t)
	dir := filepath.Join(t.TempDir(), "wallet")
	key := filepath.Join(dir, walletKeyFile)
	writeFileT(t, key, "sealed")
	fake.state[key] = walletObjectAccess{readers: []string{"S-1-5-5-0-4242"}}
	f := inspectWalletAccess(dir)
	if !f.Checked || !f.Present || f.Err != nil {
		t.Fatalf("%+v", f)
	}
	if !reflect.DeepEqual(f.Readers, []walletReader{{Path: key, Principals: []string{"S-1-5-5-0-4242"}}}) {
		t.Errorf("readers = %+v", f.Readers)
	}
	if len(fake.protected) != 0 {
		t.Errorf("inspecting protected %v", fake.protected)
	}
	if absent := inspectWalletAccess(filepath.Join(t.TempDir(), "none")); absent.Present || absent.Err != nil {
		t.Errorf("an absent wallet: %+v", absent)
	}
}

// doctor checks the wallet setup repairs: beside the installation's config,
// else the installation uninstall and upgrade resolve.
func TestDoctorWalletDirIsTheInstallationsWallet(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(root, "install", setupConfigFile)
	if got, want := doctorWalletDir(cfg, func(string) string { return "" }), filepath.Join(root, "install", "wallet"); got != want {
		t.Errorf("from the installation's config: %s, want %s", got, want)
	}
	home := filepath.Join(root, "elsewhere")
	env := func(k string) string {
		if k == "TOKENDROP_HOME" {
			return home
		}
		return ""
	}
	if got, want := doctorWalletDir(filepath.Join(root, "other.toml"), env), filepath.Join(home, "wallet"); got != want {
		t.Errorf("from a config with another name: %s, want %s", got, want)
	}
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

func sameMembers(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, w := range want {
		if indexOf(got, w) < 0 {
			return false
		}
	}
	return true
}
