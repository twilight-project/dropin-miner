//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// The registry backend, against a scratch key rather than the real
// Environment: values round-trip, a missing value is absent rather than an
// error, and Path keeps (or is created with) REG_EXPAND_SZ.
func TestRegistryUserEnvironmentBackend(t *testing.T) {
	key := fmt.Sprintf(`Software\dropin-miner-test-%d`, time.Now().UnixNano())
	t.Cleanup(func() { _ = registry.DeleteKey(registry.CURRENT_USER, key) })
	env := registryUserEnvironment{key: key}

	if _, present, err := env.Get("TOKENDROP_CONFIG"); err != nil || present {
		t.Fatalf("missing key: present=%v err=%v", present, err)
	}
	if err := env.Set("Path", `%USERPROFILE%\bin;C:\x`); err != nil {
		t.Fatal(err)
	}
	if err := env.Set("TOKENDROP_CONFIG", `C:\a b\tokendrop.toml`); err != nil {
		t.Fatal(err)
	}
	if v, present, err := env.Get("Path"); err != nil || !present || v != `%USERPROFILE%\bin;C:\x` {
		t.Fatalf("Path read back %q present=%v err=%v (must stay unexpanded)", v, present, err)
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, key, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	if _, typ, _ := k.GetStringValue("Path"); typ != registry.EXPAND_SZ {
		t.Errorf("Path type = %d, want REG_EXPAND_SZ", typ)
	}
	if _, typ, _ := k.GetStringValue("TOKENDROP_CONFIG"); typ != registry.SZ {
		t.Errorf("TOKENDROP_CONFIG type = %d, want REG_SZ", typ)
	}
}

// restrictToOwner, judged by what Windows enforces rather than by how SDDL
// happens to print it: the DACL is protected; every entry is an allow entry
// for the current user's SID, explicit rather than inherited; the user has
// full control of the directory itself; and what is created inside the
// directory afterwards inherits exactly that and nothing else.
//
// Windows may store the one inheritable grant as two entries (one effective
// on the directory, one inherit-only for its children) and prints a
// well-known SID as an alias, so neither the entry count nor the SDDL text
// is asserted. Each entry's type, flags and mask come from the binary ACE;
// its trustee is turned into a SID object and compared with the process
// token's user SID.
func TestRestrictToOwnerLeavesOnlyTheCurrentUser(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := restrictToOwner(dir, true); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	me := user.User.Sid

	entries := daclEntries(t, dir)
	if !entries.protected {
		t.Error("the directory's DACL is not protected: it would inherit its parent's entries")
	}
	full, inheritable := false, false
	for i, e := range entries.aces {
		assertOwnEntry(t, dir, i, e, me)
		if e.flags&windows.INHERITED_ACE != 0 {
			t.Errorf("%s entry %d is inherited", dir, i)
		}
		if e.flags&windows.INHERIT_ONLY_ACE == 0 && grantsFullControl(e.mask) {
			full = true
		}
		if e.flags&(windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE) == windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE {
			inheritable = true
		}
	}
	if !full {
		t.Errorf("no entry gives the current user full control of %s itself", dir)
	}
	if !inheritable {
		t.Errorf("no entry of %s is inherited by both files and directories created inside it", dir)
	}

	// Propagation: a file and a directory created afterwards carry only the
	// inherited grant.
	child := filepath.Join(dir, "child.txt")
	if err := os.WriteFile(child, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{child, sub} {
		got := daclEntries(t, path)
		if len(got.aces) == 0 {
			t.Errorf("%s has an empty DACL", path)
			continue
		}
		childFull := false
		for i, e := range got.aces {
			assertOwnEntry(t, path, i, e, me)
			if e.flags&windows.INHERITED_ACE == 0 {
				t.Errorf("%s entry %d is explicit; everything on a child should come from %s", path, i, dir)
			}
			if e.flags&windows.INHERIT_ONLY_ACE == 0 && grantsFullControl(e.mask) {
				childFull = true
			}
		}
		if !childFull {
			t.Errorf("%s did not inherit full control for the current user", path)
		}
	}
}

type aceEntry struct {
	typ   uint8
	flags uint8
	mask  windows.ACCESS_MASK
	sid   *windows.SID
}

type daclView struct {
	protected bool
	aces      []aceEntry
}

// daclEntries reads path's DACL. Types, flags and masks come from the binary
// ACEs; each trustee is read from the same-index entry of the descriptor's
// string form and converted back to a SID object, which reads a SID without
// the pointer arithmetic the binary ACE would need.
func daclEntries(t *testing.T, path string) daclView {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if dacl == nil {
		t.Fatalf("%s has a NULL DACL, which grants everyone everything", path)
	}
	trustees := sddlTrustees(t, sd.String())
	if len(trustees) != int(dacl.AceCount) {
		t.Fatalf("%s: %d ACEs but %d SDDL entries in %s", path, dacl.AceCount, len(trustees), sd.String())
	}
	view := daclView{protected: control&windows.SE_DACL_PROTECTED != 0}
	for i := 0; i < int(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			t.Fatal(err)
		}
		sid, err := windows.StringToSid(trustees[i])
		if err != nil {
			t.Fatalf("%s entry %d: trustee %q: %v", path, i, trustees[i], err)
		}
		view.aces = append(view.aces, aceEntry{typ: ace.Header.AceType, flags: ace.Header.AceFlags, mask: ace.Mask, sid: sid})
	}
	return view
}

// sddlTrustees returns the trustee field of each ACE in a descriptor's DACL
// string, in order: "D:PAI(A;;FA;;;LA)(A;OICIIO;GA;;;LA)" -> [LA LA].
func sddlTrustees(t *testing.T, sddl string) []string {
	t.Helper()
	i := strings.Index(sddl, "D:")
	if i < 0 {
		t.Fatalf("no DACL in %q", sddl)
	}
	var out []string
	rest := sddl[i:]
	for {
		open := strings.IndexByte(rest, '(')
		if open < 0 {
			return out
		}
		end := strings.IndexByte(rest[open:], ')')
		if end < 0 {
			t.Fatalf("unterminated ACE in %q", sddl)
		}
		fields := strings.Split(rest[open+1:open+end], ";")
		if len(fields) < 6 {
			t.Fatalf("unexpected ACE %q in %q", rest[open:open+end+1], sddl)
		}
		out = append(out, fields[5])
		rest = rest[open+end+1:]
	}
}

func assertOwnEntry(t *testing.T, path string, i int, e aceEntry, me *windows.SID) {
	t.Helper()
	if e.typ != windows.ACCESS_ALLOWED_ACE_TYPE {
		t.Errorf("%s entry %d has type %d, want an allow entry (no deny, no audit, no object entries)", path, i, e.typ)
	}
	if !e.sid.Equals(me) {
		t.Errorf("%s entry %d is for %s, not the current user %s", path, i, e.sid, me)
	}
}

// grantsFullControl: the generic right as written, or the file-system full
// control Windows maps it to on the object itself.
func grantsFullControl(mask windows.ACCESS_MASK) bool {
	const fileAllAccess = 0x001F01FF
	return mask&windows.GENERIC_ALL != 0 || mask&fileAllAccess == fileAllAccess
}
