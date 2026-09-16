//go:build !windows

package main

import "io/fs"

// systemWalletACL: file modes are the whole story here (wallet_acl.go), so the
// wallet's access lists are not managed. isLink is still the POSIX meaning, for
// a test that substitutes a managed backend.
func systemWalletACL() walletACLBackend {
	return walletACLBackend{
		isLink: func(info fs.FileInfo) bool { return info.Mode()&fs.ModeSymlink != 0 },
	}
}
