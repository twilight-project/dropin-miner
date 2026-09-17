//go:build windows

package main

// Windows: owner-only is a protected DACL, and the environment is the User
// environment in the registry, both through golang.org/x/sys/windows — no
// icacls, no PowerShell, no process spawned.

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// restrictToOwner replaces the object's DACL with one entry granting the
// current user full control, and protects it from inheriting anything else —
// what `icacls <dir> /inheritance:r /grant:r <user>:(OI)(CI)F` did. A
// directory's entry is inherited by what is created inside it.
func restrictToOwner(path string, dir bool) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	inherit := uint32(windows.NO_INHERITANCE)
	if dir {
		inherit = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inherit,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}, nil)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil)
}

// registryUserEnvironment is HKCU\<key> — Environment in production, a
// scratch key under Software in the backend's own test.
type registryUserEnvironment struct{ key string }

func systemUserEnvironment() userEnvironment { return registryUserEnvironment{key: "Environment"} }

func (e registryUserEnvironment) Get(name string) (string, bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, e.key, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer k.Close()
	// Unexpanded: a Path entry written as %USERPROFILE%\bin stays that way.
	v, _, err := k.GetStringValue(name)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// Set keeps a value's registry type. Path is REG_EXPAND_SZ by Windows'
// own convention, so it stays expandable even when setup creates it.
func (e registryUserEnvironment) Set(name, value string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, e.key, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	_, typ, gerr := k.GetStringValue(name)
	expand := typ == registry.EXPAND_SZ || (gerr != nil && strings.EqualFold(name, "Path"))
	if expand {
		return k.SetExpandStringValue(name, value)
	}
	return k.SetStringValue(name, value)
}

// Delete removes a value from HKCU\<key>; an absent value or key is fine.
func (e registryUserEnvironment) Delete(name string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, e.key, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

// Broadcast tells running programs — Explorer, and through it every window
// opened from now on — that the environment changed, as
// [Environment]::SetEnvironmentVariable does. x/sys/windows does not wrap
// SendMessageTimeoutW, so it is called through the lazy DLL loader, and its
// "Environment" argument has to reach the call as an address. That is the
// one unsafe.Pointer conversion in this module, and
// TestOnlyTheEnvironmentBroadcastImportsUnsafe keeps it the only one. It is
// made inside the Call argument list, the form the syscall rules support,
// with KeepAlive after the call so the UTF-16 string outlives it whatever
// the callee's annotations. A failed broadcast changes nothing that was stored: new
// sign-ins see the values regardless.
func (registryUserEnvironment) Broadcast() {
	const (
		hwndBroadcast   = 0xffff
		wmSettingChange = 0x001A
		smtoAbortIfHung = 0x0002
	)
	param, err := windows.UTF16PtrFromString("Environment")
	if err != nil {
		return
	}
	proc := windows.NewLazySystemDLL("user32.dll").NewProc("SendMessageTimeoutW")
	if proc.Find() != nil {
		return
	}
	_, _, _ = proc.Call(hwndBroadcast, wmSettingChange, 0,
		uintptr(unsafe.Pointer(param)), // #nosec G103 -- the module's one unsafe use: a *uint16 from UTF16PtrFromString, converted in the call's own argument list and kept alive past the call; the callee only reads it
		smtoAbortIfHung, 5000, 0)
	runtime.KeepAlive(param)
}

func (r *setupRun) environmentStep() int {
	binDir := filepath.Join(r.home, "bin")
	r.say("User environment")
	if r.leftForOtherInstallation("user environment", "Your user PATH and TOKENDROP_CONFIG belong to that one", "setting them here would repoint your real environment at this installation") {
		return exitOK
	}
	r.printf("These make the other commands short: %s on your user PATH, and\n"+
		"TOKENDROP_CONFIG=%s. Your key is not among them: a search reads it from the\n"+
		"stored credentials file. Windows and agents started afterwards see them.\n", binDir, r.cfgPath)
	if r.noProfile {
		r.printf("Left your user environment alone (-no-profile).\n")
		r.skip("user environment")
		return exitOK
	}
	env := r.d.userEnv
	if env == nil {
		r.printf("No user environment to change. Add %s to PATH and set TOKENDROP_CONFIG by hand.\n", binDir)
		return exitOK
	}
	journalPath := filepath.Join(r.home, setupEnvJournalFile)
	prior, err := readEnvJournal(journalPath)
	if err != nil {
		r.printf("Not touching your user environment: %v. Set them by hand.\n", err)
		return exitOK
	}
	change, err := planUserEnvironment(env, prior, binDir, r.cfgPath)
	if err != nil {
		r.printf("Not touching your user environment: %v. Set them by hand.\n", err)
		return exitOK
	}
	if !change.writeJournal && !change.addPath && !change.setConfig {
		r.shortCommands = true
		r.printf("Your user environment already has them.\n")
		return exitOK
	}
	if r.dry {
		r.printf("(dry run) would record what it changes in %s, then set them\n", journalPath)
		return exitOK
	}
	if !r.d.interactive && !r.yes {
		r.printf("Not an interactive shell — not touching your user environment (pass -yes to set them).\n")
		r.skip("user environment")
		return exitOK
	}
	set, err := r.ask("Set them for your user?")
	if err != nil {
		return r.abort("your user environment was not touched and the coding agents were not set up")
	}
	if !set {
		r.say("Left your user environment alone")
		return exitOK
	}
	if err := applyUserEnvironment(env, journalPath, change); err != nil {
		r.printf("Could not finish: %v. Set them by hand.\n", err)
		return exitOK
	}
	r.changed = true
	r.shortCommands = true
	r.say("Set them for your user; open a new window to use them")
	return exitOK
}
