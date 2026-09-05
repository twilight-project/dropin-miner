//go:build windows

package auth

// posixModes is false on Windows: Go reports 0777 for every directory
// and file regardless of ACLs, so a mode check would refuse every store.
// Custody there rests on the owner-only ACL the installer applies to the
// state directory.
const posixModes = false
