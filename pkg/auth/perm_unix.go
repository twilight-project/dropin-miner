//go:build !windows

package auth

// posixModes: file modes carry the owner/group/world bits this package
// checks. On Windows they do not — Go reports 0777 for every directory —
// so the check would refuse every store; access control there is the
// directory ACL the installer sets.
const posixModes = true
