//go:build windows

package main

// The Windows half of wallet_acl.go: read an object's DACL, give it a protected
// owner-only one, recognize a reparse point.

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

func systemWalletACL() walletACLBackend {
	return walletACLBackend{
		managed: true,
		inspect: inspectWindowsWalletObject,
		protect: restrictToOwner,
		isLink:  isReparsePoint,
	}
}

// isReparsePoint is the attribute itself rather than the mode Go derives from
// it: a directory junction is neither ModeSymlink nor ModeDir, and a reparse
// point of any kind is what setup refuses to follow.
func isReparsePoint(info fs.FileInfo) bool {
	if data, ok := info.Sys().(*syscall.Win32FileAttributeData); ok {
		return data.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
	}
	return info.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0
}

// inspectWindowsWalletObject reads path's DACL. Each entry's type, flags and
// mask come from the binary ACE; its trustee comes from the same-index entry of
// the descriptor's string form, which names a SID without the pointer
// arithmetic the binary ACE would need (the module admits unsafe in one place
// only). A NULL DACL grants everyone everything.
func inspectWindowsWalletObject(path string) (walletObjectAccess, error) {
	var acc walletObjectAccess
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return acc, fmt.Errorf("read the current user: %w", err)
	}
	owner := user.User.Sid.String()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return acc, err
	}
	control, _, err := sd.Control()
	if err != nil {
		return acc, err
	}
	acc.protected = control&windows.SE_DACL_PROTECTED != 0
	dacl, _, err := sd.DACL()
	if err != nil {
		return acc, err
	}
	if dacl == nil {
		acc.readers = []string{"everyone (the object has no access list)"}
		return acc, nil
	}
	trustees, err := sddlACETrustees(sd.String())
	if err != nil {
		return acc, err
	}
	if len(trustees) != int(dacl.AceCount) {
		return acc, fmt.Errorf("%d access entries but %d in the descriptor's string form", dacl.AceCount, len(trustees))
	}
	aces := make([]walletACE, 0, len(trustees))
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return acc, err
		}
		sid, err := windows.StringToSid(trustees[i])
		if err != nil {
			return acc, fmt.Errorf("entry %d names %q: %w", i, trustees[i], err)
		}
		aces = append(aces, walletACE{
			allow: isAllowACE(ace.Header.AceType),
			flags: ace.Header.AceFlags,
			mask:  uint32(ace.Mask),
			sid:   sid.String(),
		})
	}
	var sids []string
	acc.ownerOnly, sids = judgeWalletACEs(aces, owner)
	for _, s := range sids {
		acc.readers = append(acc.readers, principalName(s))
	}
	return acc, nil
}

// isAllowACE: the four allow entry types a DACL can hold (plain, object,
// callback, callback object). Each keeps its mask right after the header.
func isAllowACE(t uint8) bool {
	switch t {
	case 0x0, 0x5, 0x9, 0xB:
		return true
	}
	return false
}

// principalName is DOMAIN\name where the SID resolves, else the SID itself.
func principalName(s string) string {
	sid, err := windows.StringToSid(s)
	if err != nil {
		return s
	}
	account, domain, _, err := sid.LookupAccount("")
	switch {
	case err != nil || account == "":
		return s
	case domain == "":
		return account
	}
	return domain + `\` + account
}

// sddlACETrustees returns the trustee field of each ACE in a descriptor
// string's DACL, in order: "D:PAI(A;;FA;;;LA)(A;OICIIO;GA;;;LA)" -> [LA LA]. A
// conditional entry's expression carries parentheses of its own, so an entry
// ends at the parenthesis that closes it, not at the first one.
func sddlACETrustees(sddl string) ([]string, error) {
	start := strings.Index(sddl, "D:")
	if start < 0 {
		return nil, errors.New("the descriptor has no DACL")
	}
	var trustees []string
	depth, open := 0, -1
	for i := start + 2; i < len(sddl); i++ {
		switch sddl[i] {
		case '(':
			if depth == 0 {
				open = i
			}
			depth++
		case ')':
			depth--
			if depth < 0 {
				return nil, fmt.Errorf("unbalanced access entry in %q", sddl)
			}
			if depth == 0 {
				fields := strings.SplitN(sddl[open+1:i], ";", 7)
				if len(fields) < 6 {
					return nil, fmt.Errorf("unexpected access entry %q", sddl[open:i+1])
				}
				trustees = append(trustees, fields[5])
			}
		default:
			// Past the DACL: a SACL section follows only when one was asked for.
			if depth == 0 && i+1 < len(sddl) && sddl[i+1] == ':' && open >= 0 {
				return trustees, nil
			}
		}
	}
	if depth != 0 {
		return nil, fmt.Errorf("unterminated access entry in %q", sddl)
	}
	return trustees, nil
}
