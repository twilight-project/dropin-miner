//go:build windows

package main

// posixModes mirrors pkg/auth: Go reports 0777 for everything on Windows,
// so mode checks would refuse every wallet; the directory ACL guards it.
const posixModes = false
