//go:build windows

package main

// The wallet's access on Windows, judged by what Windows enforces.
//
// The second principal. A test cannot create a second user, and this module
// admits unsafe in one file only, so a Go test cannot build a restricted token
// either. probeAsSecondPrincipal therefore runs a separate Windows PowerShell
// process that takes its own token, makes the owner's SID deny-only and drops
// every privilege but traversal (CreateRestrictedToken, reached through a
// Reflection.Emit P/Invoke so no compiler is needed on any runner), and opens
// each path while impersonating that token. What such a principal can open is
// decided only by its groups. The tests grant one of them — the logon session
// SID, which no default access list names — an inheritable read entry on the
// installation directory, the way a coding agent's sandbox setup grants its
// sandbox group one. So a path the probe can open is one that entry reaches,
// and the owner-only wallet must refuse it.
//
// Every fixture proves itself first: the entry is present where the test
// says, and the probe reads the wallet before the repair. A probe that could
// read nothing would make "the wallet is refused" meaningless, so
// credentials.json being readable is asserted alongside every refusal.
//
// Setup restricts the installation directory to its owner on every run, which
// replaces any entry another program added there, and Windows carries that
// change down to everything that inherits. Checking the wallet straight after
// setup would therefore pass without any wallet protection at all. After setup
// runs, these tests add the entry again — as the program that added it does —
// and only then judge the wallet.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// secondPrincipalProbeScript reads DROPIN_PROBE_PATHS and DROPIN_PROBE_KINDS
// ("|"-separated; "d" lists a directory, "f" opens a file for reading) and
// prints "<index> <HRESULT as 8 hex digits>" per path, 00000000 for success.
const secondPrincipalProbeScript = `
$ErrorActionPreference = 'Stop'
$interop = [System.Runtime.InteropServices.Marshal]

$assemblyName = New-Object System.Reflection.AssemblyName 'DropinSecondPrincipalProbe'
$assembly = [AppDomain]::CurrentDomain.DefineDynamicAssembly($assemblyName, [System.Reflection.Emit.AssemblyBuilderAccess]::Run)
$module = $assembly.DefineDynamicModule('DropinSecondPrincipalProbe')
$typeBuilder = $module.DefineType('DropinSecondPrincipalProbe.Native', [System.Reflection.TypeAttributes]'Public, Class')
$parameterTypes = [Type[]]@([IntPtr], [UInt32], [UInt32], [IntPtr], [UInt32], [IntPtr], [UInt32], [IntPtr], [IntPtr].MakeByRefType())
$method = $typeBuilder.DefineMethod('CreateRestrictedToken', [System.Reflection.MethodAttributes]'Public, Static, PinvokeImpl', [bool], $parameterTypes)
[void]$method.DefineParameter(9, [System.Reflection.ParameterAttributes]::Out, 'NewTokenHandle')
$dllImport = [System.Runtime.InteropServices.DllImportAttribute]
$attribute = New-Object System.Reflection.Emit.CustomAttributeBuilder -ArgumentList @(
    $dllImport.GetConstructor([Type[]]@([string])),
    [object[]]@('advapi32.dll'),
    [System.Reflection.FieldInfo[]]@($dllImport.GetField('SetLastError')),
    [object[]]@($true))
$method.SetCustomAttribute($attribute)
$native = $typeBuilder.CreateType()

# SID_AND_ATTRIBUTES for the owner's SID: a pointer, then a DWORD padded to
# pointer alignment. Attributes 0; SidsToDisable makes it deny-only.
$identity = [System.Security.Principal.WindowsIdentity]::GetCurrent()
$ownerSid = $identity.User
$sidBytes = New-Object byte[] ($ownerSid.BinaryLength)
$ownerSid.GetBinaryForm($sidBytes, 0)
$sidMemory = $interop::AllocHGlobal($sidBytes.Length)
$interop::Copy($sidBytes, 0, $sidMemory, $sidBytes.Length)
$disable = $interop::AllocHGlobal([IntPtr]::Size * 2)
$interop::WriteIntPtr($disable, 0, $sidMemory)
$interop::WriteInt32($disable, [IntPtr]::Size, 0)

# Flags 1 is DISABLE_MAX_PRIVILEGE: every privilege but SeChangeNotifyPrivilege
# goes, so no backup or restore privilege can open what the lists refuse.
$restricted = [IntPtr]::Zero
if (-not $native::CreateRestrictedToken($identity.Token, 1, 1, $disable, 0, [IntPtr]::Zero, 0, [IntPtr]::Zero, [ref]$restricted)) {
    throw ('CreateRestrictedToken failed with Win32 error {0}' -f $interop::GetLastWin32Error())
}

$paths = $env:DROPIN_PROBE_PATHS.Split([char]'|')
$kinds = $env:DROPIN_PROBE_KINDS.Split([char]'|')
$results = New-Object 'System.Collections.Generic.List[string]'
$context = [System.Security.Principal.WindowsIdentity]::Impersonate($restricted)
try {
    for ($i = 0; $i -lt $paths.Length; $i++) {
        $hresult = 0
        try {
            if ($kinds[$i] -eq 'd') {
                [void][System.IO.Directory]::GetFileSystemEntries($paths[$i])
            } else {
                $stream = [System.IO.File]::Open($paths[$i], [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]'ReadWrite, Delete')
                $stream.Dispose()
            }
        } catch {
            $cause = $_.Exception
            while ($cause.InnerException -ne $null) { $cause = $cause.InnerException }
            $hresult = $cause.HResult
        }
        $results.Add(('{0} {1:X8}' -f $i, $hresult))
    }
} finally {
    $context.Undo()
}
foreach ($line in $results) { [Console]::Out.WriteLine($line) }
`

const (
	probeOpened       uint32 = 0x00000000
	probeAccessDenied uint32 = 0x80070005 // HRESULT_FROM_WIN32(ERROR_ACCESS_DENIED)
)

type principalProbe struct {
	path string
	dir  bool
}

func probeDir(path string) principalProbe  { return principalProbe{path: path, dir: true} }
func probeFile(path string) principalProbe { return principalProbe{path: path} }

// probeAsSecondPrincipal returns the HRESULT of each open, in order.
func probeAsSecondPrincipal(t *testing.T, targets ...principalProbe) []uint32 {
	t.Helper()
	ps, err := exec.LookPath("powershell.exe")
	if err != nil {
		skipPermissionTest(t, "powershell.exe is not on PATH, so no second principal can be run: "+err.Error())
		return nil
	}
	paths, kinds := make([]string, len(targets)), make([]string, len(targets))
	for i, target := range targets {
		if strings.Contains(target.path, "|") {
			t.Fatalf("probe path %q contains the separator", target.path)
		}
		paths[i], kinds[i] = target.path, "f"
		if target.dir {
			kinds[i] = "d"
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", // #nosec G204 -- Windows PowerShell running this test's own fixed script
		"-EncodedCommand", encodePowerShellCommand(secondPrincipalProbeScript))
	cmd.Env = append(os.Environ(), "DROPIN_PROBE_PATHS="+strings.Join(paths, "|"), "DROPIN_PROBE_KINDS="+strings.Join(kinds, "|"))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("the second-principal probe did not run: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	results := make([]uint32, len(targets))
	seen := make([]bool, len(targets))
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		i, err := strconv.Atoi(fields[0])
		if err != nil || i < 0 || i >= len(targets) {
			continue
		}
		code, err := strconv.ParseUint(fields[1], 16, 32)
		if err != nil {
			t.Fatalf("probe line %q: %v", line, err)
		}
		results[i], seen[i] = uint32(code), true
	}
	for i, ok := range seen {
		if !ok {
			t.Fatalf("the probe reported nothing for %s\nstdout:\n%s\nstderr:\n%s", targets[i].path, stdout.String(), stderr.String())
		}
		t.Logf("second principal opens %s: %08X", targets[i].path, results[i])
	}
	return results
}

// encodePowerShellCommand lives in host_exec_test.go, which carries no build
// tag: PR H's execution harness runs PowerShell through the same
// -EncodedCommand form and needs the helper compiled on every OS. This file
// and that one arrived from different branches with byte-identical copies;
// the merge keeps one.

// logonSessionSID is the SID the tests grant: a group every process of this
// logon session holds, and one no default access list names.
func logonSessionSID(t *testing.T) *windows.SID {
	t.Helper()
	groups, err := windows.GetCurrentProcessToken().GetTokenGroups()
	if err != nil {
		skipPermissionTest(t, "the token's groups cannot be read: "+err.Error())
		return nil
	}
	for _, g := range groups.AllGroups() {
		if g.Attributes&windows.SE_GROUP_LOGON_ID == windows.SE_GROUP_LOGON_ID && g.Attributes&windows.SE_GROUP_ENABLED != 0 {
			sid, err := g.Sid.Copy()
			if err != nil {
				t.Fatal(err)
			}
			return sid
		}
	}
	skipPermissionTest(t, "this token holds no enabled logon session SID")
	return nil
}

// grantRead adds a read-and-execute entry for sid to path's DACL, keeping the
// list's protection as it was. inheritable makes it an (OI)(CI) entry, which
// Windows carries down to everything beneath that inherits.
func grantRead(t *testing.T, path string, sid *windows.SID, inheritable bool) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	old, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	inherit := uint32(windows.NO_INHERITANCE)
	if inheritable {
		inherit = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	const fileGenericReadExecute = 0x001200A9
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: fileGenericReadExecute,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inherit,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}, old)
	if err != nil {
		t.Fatal(err)
	}
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	if control&windows.SE_DACL_PROTECTED != 0 {
		info = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, info, nil, nil, acl, nil); err != nil {
		skipPermissionTest(t, fmt.Sprintf("add a read entry to %s: %v", path, err))
	}
}

// entryFor reports whether path's DACL holds an entry for sid, and whether
// that entry is inherited.
func entryFor(t *testing.T, path string, sid *windows.SID) (found, inherited bool) {
	t.Helper()
	for _, e := range daclEntries(t, path).aces {
		if e.sid.Equals(sid) {
			return true, e.flags&windows.INHERITED_ACE != 0
		}
	}
	return false, false
}

// assertOwnerOnlyProtected: a protected DACL whose every entry is an allow
// entry for the current user — what the repair and creation leave.
func assertOwnerOnlyProtected(t *testing.T, paths ...string) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		view := daclEntries(t, path)
		if !view.protected {
			t.Errorf("%s's DACL is not protected: it still inherits from its parent", path)
		}
		if len(view.aces) == 0 {
			t.Errorf("%s has an empty DACL", path)
		}
		for i, e := range view.aces {
			assertOwnEntry(t, path, i, e, user.User.Sid)
		}
	}
}

// v029Wallet lays out a wallet the way the previous release left one: a
// directory and two files that inherit whatever the installation directory
// holds, with no access list of their own.
func v029Wallet(t *testing.T, home string) (wallet, key, pub string) {
	t.Helper()
	wallet = filepath.Join(home, "wallet")
	key, pub = filepath.Join(wallet, walletKeyFile), filepath.Join(wallet, walletSidecarFile)
	if err := os.MkdirAll(wallet, 0o700); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{key: "sealed-key", pub: `{"address":"twilight1old"}`} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return wallet, key, pub
}

// firstSetup runs setup once in the sandbox so the installation directory, its
// owner-only DACL and credentials.json are exactly what setup leaves.
func firstSetup(t *testing.T, s *setupSandbox) {
	t.Helper()
	s.platform.claim("credits")
	if code, out, errOut := s.run(tty("n"), true, "-yes", "-no-agents", "-no-profile"); code != exitOK {
		t.Fatalf("first setup exited %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if !lexists(filepath.Join(s.home, credentialsFile)) {
		t.Fatal("the first setup stored no credentials.json to compare the wallet against")
	}
}

// The case this exists for: an installation from before this change, whose
// directory then gains an inheritable read entry for a second principal, so
// the wallet directory and wallet.key inherit it (and wallet.pub carries an
// explicit one, which protecting the directory alone would leave in place).
// setup repairs the wallet; once the entry is back, that principal can open
// neither the wallet directory nor any file in it, and still reads
// credentials.json.
func TestSetupRepairsAWalletAnotherPrincipalCouldRead(t *testing.T) {
	s := newSetupSandbox(t)
	firstSetup(t, s)
	wallet, key, pub := v029Wallet(t, s.home)
	creds := filepath.Join(s.home, credentialsFile)
	sid := logonSessionSID(t)
	grantRead(t, s.home, sid, true)
	grantRead(t, pub, sid, false)

	for _, path := range []string{wallet, key, creds} {
		if found, inherited := entryFor(t, path, sid); !found || !inherited {
			t.Fatalf("fixture: %s does not inherit the second principal's entry (found=%v inherited=%v)", path, found, inherited)
		}
	}
	if found, inherited := entryFor(t, pub, sid); !found || inherited {
		t.Fatalf("fixture: %s does not carry its own explicit entry (found=%v inherited=%v)", pub, found, inherited)
	}
	targets := []principalProbe{probeDir(wallet), probeFile(key), probeFile(pub), probeFile(creds)}
	for i, code := range probeAsSecondPrincipal(t, targets...) {
		if code != probeOpened {
			t.Fatalf("fixture: before the repair the second principal could not open %s (%08X), so a refusal afterwards would prove nothing", targets[i].path, code)
		}
	}

	code, out, errOut := s.run(tty(), true, "-yes", "-no-agents", "-no-profile")
	if code != exitOK {
		t.Fatalf("setup exited %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "Made the wallet in "+wallet+" readable only by you") {
		t.Errorf("setup did not say it repaired the wallet:\n%s", out)
	}
	assertOwnerOnlyProtected(t, wallet, key, pub)

	grantRead(t, s.home, sid, true)
	if found, _ := entryFor(t, creds, sid); !found {
		t.Fatalf("fixture: the entry re-added to %s did not reach credentials.json", s.home)
	}
	got := probeAsSecondPrincipal(t, targets...)
	for i, want := range []uint32{probeAccessDenied, probeAccessDenied, probeAccessDenied, probeOpened} {
		if got[i] != want {
			t.Errorf("after setup the second principal opens %s with %08X, want %08X", targets[i].path, got[i], want)
		}
	}
	if b, err := os.ReadFile(key); err != nil || string(b) != "sealed-key" { // #nosec G304 -- this test's own sandbox
		t.Errorf("the owner can no longer read the wallet key: %q %v", b, err)
	}
}

// A wallet created under such a directory is owner-only from the start,
// whether the command creates the wallet directory or finds it already there
// (inheriting the entry).
func TestAWalletCreatedWhereAnotherPrincipalCanReadIsOwnerOnly(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "the command creates the directory"
		if existing {
			name = "the directory is already there"
		}
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "home")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := restrictToOwner(root, true); err != nil {
				t.Fatal(err)
			}
			creds := filepath.Join(root, credentialsFile)
			if err := os.WriteFile(creds, []byte(`{"api_key":"sr-test"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			sid := logonSessionSID(t)
			grantRead(t, root, sid, true)
			wallet := filepath.Join(root, "wallet")
			getenv := func(k string) string {
				if k == walletPassphraseEnv {
					return "a passphrase for this test only"
				}
				return ""
			}

			if existing {
				if err := os.Mkdir(wallet, 0o700); err != nil {
					t.Fatal(err)
				}
				if found, inherited := entryFor(t, wallet, sid); !found || !inherited {
					t.Fatalf("fixture: %s does not inherit the entry", wallet)
				}
			} else {
				// A read-only command is enough to create the directory.
				var out, errOut bytes.Buffer
				_ = walletAddress([]string{"-dir", wallet}, &out, &errOut, getenv)
				if !lexists(wallet) {
					t.Fatalf("wallet address did not create %s: %s", wallet, errOut.String())
				}
				assertOwnerOnlyProtected(t, wallet)
			}

			var out, errOut bytes.Buffer
			if code := walletInit([]string{"-dir", wallet, "-print-anyway"}, strings.NewReader(""), &out, &errOut, getenv); code != exitOK {
				t.Fatalf("wallet init exited %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
			}
			key, pub := filepath.Join(wallet, walletKeyFile), filepath.Join(wallet, walletSidecarFile)
			assertOwnerOnlyProtected(t, wallet, key, pub)
			got := probeAsSecondPrincipal(t, probeDir(wallet), probeFile(key), probeFile(creds))
			for i, want := range []uint32{probeAccessDenied, probeAccessDenied, probeOpened} {
				if got[i] != want {
					t.Errorf("the second principal opens target %d with %08X, want %08X", i, got[i], want)
				}
			}
		})
	}
}

// A reparse point inside the wallet fails the repair and is named; the
// directory it points to is not touched, and what can be secured is.
func TestSetupRefusesAReparsePointInTheWallet(t *testing.T) {
	s := newSetupSandbox(t)
	firstSetup(t, s)
	wallet, key, _ := v029Wallet(t, s.home)
	target := filepath.Join(s.root, "elsewhere")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	targetBefore := daclEntries(t, target)
	link := filepath.Join(wallet, "link")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil { // #nosec G204 -- cmd's mklink on this test's own paths
		skipPermissionTest(t, fmt.Sprintf("create a directory junction: %v: %s", err, out))
	}
	info, err := os.Lstat(link)
	if err != nil || !isReparsePoint(info) {
		t.Fatalf("fixture: %s is not a reparse point (%v)", link, err)
	}
	connectsBefore := s.connectCalls

	code, out, errOut := s.run(tty(), true, "-yes", "-no-agents", "-no-profile")
	if code == exitOK {
		t.Fatalf("setup exited 0 over a reparse point in the wallet\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if !strings.Contains(errOut, link) || !strings.Contains(errOut, "not followed") {
		t.Errorf("the refusal does not name the reparse point:\n%s", errOut)
	}
	if s.connectCalls != connectsBefore || strings.Contains(out, "Setup complete.") {
		t.Error("setup went on past the refused wallet")
	}
	assertOwnerOnlyProtected(t, wallet, key)
	targetAfter := daclEntries(t, target)
	if targetAfter.protected != targetBefore.protected || len(targetAfter.aces) != len(targetBefore.aces) {
		t.Errorf("the junction's target was changed through it: protected %v->%v, %d->%d entries",
			targetBefore.protected, targetAfter.protected, len(targetBefore.aces), len(targetAfter.aces))
	}
	if facts := inspectWalletAccess(wallet); len(facts.Unsecurable) != 1 || facts.Unsecurable[0] != link {
		t.Errorf("doctor does not report the reparse point: %+v", facts)
	}
}

// A wallet file whose DACL cannot be set fails setup, naming it.
func TestSetupFailsWhenAWalletFileCannotBeSecured(t *testing.T) {
	s := newSetupSandbox(t)
	firstSetup(t, s)
	_, key, pub := v029Wallet(t, s.home)
	s.restrict = func(path string, dir bool) error {
		if path == key {
			return windows.ERROR_ACCESS_DENIED
		}
		return restrictToOwner(path, dir)
	}
	connectsBefore := s.connectCalls

	code, out, errOut := s.run(tty(), true, "-yes", "-no-agents", "-no-profile")
	if code == exitOK {
		t.Fatalf("setup exited 0 over a wallet file it could not secure\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	if !strings.Contains(errOut, key) || !strings.Contains(errOut, windows.ERROR_ACCESS_DENIED.Error()) {
		t.Errorf("the failure does not name the file and the cause:\n%s", errOut)
	}
	if s.connectCalls != connectsBefore || strings.Contains(out, "Setup complete.") {
		t.Error("setup went on past the wallet file it could not secure")
	}
	assertOwnerOnlyProtected(t, pub)
}

// doctor names a second reader of the wallet, and the repair; after setup, and
// with the entry back on the installation directory, it reports none.
func TestDoctorReportsAnotherReaderOfTheWalletUntilSetupRepairsIt(t *testing.T) {
	s := newSetupSandbox(t)
	firstSetup(t, s)
	_, key, _ := v029Wallet(t, s.home)
	sid := logonSessionSID(t)
	grantRead(t, s.home, sid, true)
	if found, inherited := entryFor(t, key, sid); !found || !inherited {
		t.Fatalf("fixture: %s does not inherit the entry", key)
	}

	check := doctorWalletCheckFor(t, s)
	if check["verdict"] != string(verdictNo) {
		t.Fatalf("doctor's wallet access verdict with a second reader = %v (%v), want NO", check["verdict"], check["detail"])
	}
	detail, _ := check["detail"].(string)
	if !strings.Contains(detail, key) || !strings.Contains(detail, principalName(sid.String())) {
		t.Errorf("the detail does not name the file and the reader: %q", detail)
	}
	// Against a real DACL too: the entry can come back — this fixture adds it
	// again after setup, and a machine policy does the same on its own
	// schedule — so the detail says so rather than reading as a one-off repair.
	if !strings.Contains(detail, recurringEntryDetail) {
		t.Errorf("the detail does not say the entry can be added again: %q", detail)
	}
	if check["fix"] != "dropin-miner setup" {
		t.Errorf("fix = %v, want dropin-miner setup", check["fix"])
	}

	if code, out, errOut := s.run(tty(), true, "-yes", "-no-agents", "-no-profile"); code != exitOK {
		t.Fatalf("setup exited %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
	grantRead(t, s.home, sid, true)
	if check := doctorWalletCheckFor(t, s); check["verdict"] != string(verdictOK) {
		t.Errorf("after setup doctor's wallet access verdict = %v (%v), want OK", check["verdict"], check["detail"])
	}
}

func doctorWalletCheckFor(t *testing.T, s *setupSandbox) map[string]any {
	t.Helper()
	var out, errOut bytes.Buffer
	_ = cmdDoctor([]string{"-config", s.cfgPath(), "-json"}, &out, &errOut)
	data := dataOf(t, decodeCommandEnvelope(t, out.String(), "doctor"))
	checks, _ := data["checks"].([]any)
	for _, raw := range checks {
		if c, ok := raw.(map[string]any); ok && c["name"] == "wallet access" {
			return c
		}
	}
	t.Fatalf("doctor -json has no wallet access check:\n%s\nstderr:\n%s", out.String(), errOut.String())
	return nil
}

// A set-aside wallet adopted by setup arrives owner-only, whatever entries its
// objects carried where it was.
func TestAdoptionSecuresAWalletAnotherPrincipalCouldRead(t *testing.T) {
	s := newSetupSandbox(t)
	s.platform.claim("credits")
	sibling := s.home + ".bak"
	writeInstallation(t, sibling, "wallet")
	sid := logonSessionSID(t)
	grantRead(t, sibling, sid, true)
	siblingKey := filepath.Join(sibling, "wallet", walletKeyFile)
	grantRead(t, siblingKey, sid, false)
	if found, _ := entryFor(t, siblingKey, sid); !found {
		t.Fatalf("fixture: %s carries no entry for the second principal", siblingKey)
	}

	_, out, errOut := s.run(tty("y", "n"), true, "-no-agents", "-no-profile")
	if !strings.Contains(out, "adopted the wallet") {
		t.Fatalf("the wallet was not adopted\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
	wallet := filepath.Join(s.home, "wallet")
	key, pub := filepath.Join(wallet, walletKeyFile), filepath.Join(wallet, walletSidecarFile)
	assertOwnerOnlyProtected(t, wallet, key, pub)
	for _, path := range []string{wallet, key, pub} {
		if found, _ := entryFor(t, path, sid); found {
			t.Errorf("%s still carries the second principal's entry", path)
		}
	}
}
