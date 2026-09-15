//go:build windows

package main

import (
	"sync"
	"testing"

	"golang.org/x/sys/windows"
)

func sandboxEmulationUnavailable() string { return "" }

// Write-type rights only: FILE_WRITE_DATA (FILE_ADD_FILE on a directory),
// FILE_APPEND_DATA (FILE_ADD_SUBDIRECTORY), FILE_WRITE_EA, FILE_DELETE_CHILD
// and FILE_WRITE_ATTRIBUTES. SYNCHRONIZE and READ_CONTROL stay allowed, so a
// read handle can still be opened.
const denyWriteRights windows.ACCESS_MASK = 0x0002 | 0x0004 | 0x0010 | 0x0040 | 0x0100

// denyWritesKeepReads is the Windows emulation of a write-denying sandbox: an
// explicit deny ACE for the current user's write rights on dir and on each
// file, reads untouched. restore puts back each saved DACL and is safe to call
// more than once; it also runs at cleanup. A system that will not take the
// ACE skips the test with the reason.
func denyWritesKeepReads(t *testing.T, dir string, files ...string) (restore func()) {
	t.Helper()
	token, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Skipf("unsupported ACL: the current user cannot be read: %v", err)
	}
	type saved struct {
		path string
		sd   *windows.SECURITY_DESCRIPTOR
		dacl *windows.ACL
	}
	var undo []saved
	var once sync.Once
	restore = func() {
		once.Do(func() {
			for i := len(undo) - 1; i >= 0; i-- {
				_ = windows.SetNamedSecurityInfo(undo[i].path, windows.SE_FILE_OBJECT,
					windows.DACL_SECURITY_INFORMATION, nil, nil, undo[i].dacl, nil)
			}
		})
	}
	t.Cleanup(restore)
	for _, path := range append(append([]string{}, files...), dir) {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Skipf("unsupported ACL: read the DACL of %s: %v", path, err)
		}
		old, _, err := sd.DACL()
		if err != nil {
			t.Skipf("unsupported ACL: %s has no readable DACL: %v", path, err)
		}
		acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
			AccessPermissions: denyWriteRights,
			AccessMode:        windows.DENY_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(token.User.Sid),
			},
		}}, old)
		if err != nil {
			t.Skipf("unsupported ACL: build a deny entry for %s: %v", path, err)
		}
		if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
			t.Skipf("unsupported ACL: set a deny entry on %s: %v", path, err)
		}
		undo = append(undo, saved{path: path, sd: sd, dacl: old})
	}
	return restore
}
