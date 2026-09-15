//go:build windows

package main

import (
	"fmt"
	"sync"
	"syscall"
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
// ACE leaves through skipPermissionTest, which fails under CI.
func denyWritesKeepReads(t *testing.T, dir string, files ...string) (restore func()) {
	t.Helper()
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
	unsupported := func(format string, args ...any) func() {
		skipPermissionTest(t, "unsupported ACL: "+fmt.Sprintf(format, args...))
		return restore
	}
	token, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return unsupported("the current user cannot be read: %v", err)
	}
	for _, path := range append(append([]string{}, files...), dir) {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			return unsupported("read the DACL of %s: %v", path, err)
		}
		old, _, err := sd.DACL()
		if err != nil {
			return unsupported("%s has no readable DACL: %v", path, err)
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
			return unsupported("build a deny entry for %s: %v", path, err)
		}
		if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
			return unsupported("set a deny entry on %s: %v", path, err)
		}
		undo = append(undo, saved{path: path, sd: sd, dacl: old})
	}
	return restore
}

// TestWindowsFlushLockOpenErrorDecision enumerates the Windows fallback
// decision: only ERROR_ACCESS_DENIED on the read-write open retries
// read-only; a sharing or lock violation is busy on either open; a missing
// file on the read-only retry is errFlushLockAbsent; everything else fails.
func TestWindowsFlushLockOpenErrorDecision(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		readOnly bool
		want     flushOpenOutcome
	}{
		{"ERROR_ACCESS_DENIED on the read-write open falls back", syscall.ERROR_ACCESS_DENIED, false, flushOpenFallBack},
		{"a wrapped ERROR_ACCESS_DENIED falls back", fmt.Errorf("open flush.lock: %w", syscall.ERROR_ACCESS_DENIED), false, flushOpenFallBack},
		{"ERROR_SHARING_VIOLATION on the read-write open is busy", errSharingViolation, false, flushOpenBusy},
		{"ERROR_SHARING_VIOLATION on the read-only retry is busy", errSharingViolation, true, flushOpenBusy},
		{"ERROR_LOCK_VIOLATION is busy", errLockViolation, false, flushOpenBusy},
		{"ERROR_FILE_NOT_FOUND on the read-only retry is absent", syscall.ERROR_FILE_NOT_FOUND, true, flushOpenAbsent},
		{"ERROR_FILE_NOT_FOUND on the read-write open fails", syscall.ERROR_FILE_NOT_FOUND, false, flushOpenFailed},
		{"ERROR_PATH_NOT_FOUND does not fall back", syscall.ERROR_PATH_NOT_FOUND, false, flushOpenFailed},
		{"ERROR_ACCESS_DENIED on the read-only retry fails", syscall.ERROR_ACCESS_DENIED, true, flushOpenFailed},
		{"a wrapped non-permission error does not fall back", fmt.Errorf("open flush.lock: %w", syscall.Errno(123)), false, flushOpenFailed},
	}
	for _, c := range cases {
		if got := classifyFlushLockOpenError(c.err, c.readOnly); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}
