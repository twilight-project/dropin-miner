package main

// Who owns the binary that is running: the one classifier setup, uninstall
// -binary and upgrade all consult, so no two of them can disagree about
// whether a copy is npm's.
//
// The npm launcher is the first authority. It sets DROPIN_MINER_LAUNCH to
// npm:global, npm:local or npm:unknown for the binary it starts; nothing else
// sets it. A path inside npm's temporary exec cache is npm's whatever the
// variable says, because the path is the proof. With no marker, a binary
// inside node_modules was run around the launcher, and a binary beside npm's
// package files (package.json naming this package, install.js, and the
// launcher dropin-miner.js in its own directory) is the package's copy
// wherever that package lives. A partial set of those files is ambiguous. A
// launcher started with DROPIN_MINER_BINARY still sets the marker, so that
// copy is treated as npm's: conservative, and never destructive.
//
// Each consumer decides what a kind permits. Setup accepts a global npm
// install, since hooks pointing at it survive; uninstall -binary and upgrade
// accept only a native copy, because replacing or removing bytes npm owns is
// npm's job.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// launchKind is what the classifier concluded.
type launchKind int

const (
	launchNative     launchKind = iota // no npm authority or layout
	launchNPMGlobal                    // DROPIN_MINER_LAUNCH=npm:global
	launchNPMLocal                     // DROPIN_MINER_LAUNCH=npm:local
	launchNPMUnknown                   // the launcher could not tell, or an unrecognized marker
	launchNPMCache                     // npm's temporary exec cache
	launchNPMDirect                    // inside node_modules, run around the launcher
	launchNPMLayout                    // beside npm's package files, no marker
	launchAmbiguous                    // some of npm's package files, not all
)

const npmPackageName = "dropin-miner"

// classifyLaunch decides who owns exe, given the launcher's marker.
func classifyLaunch(exe, marker string) launchKind {
	segments := strings.FieldsFunc(exe, func(c rune) bool { return c == '/' || c == '\\' })
	for _, seg := range segments {
		switch strings.ToLower(seg) {
		case "_npx", "_cacache", "npm-cache":
			return launchNPMCache
		}
	}
	switch marker {
	case "npm:global":
		return launchNPMGlobal
	case "npm:local":
		return launchNPMLocal
	case "":
	default:
		return launchNPMUnknown
	}
	for _, seg := range segments {
		if strings.EqualFold(seg, "node_modules") {
			return launchNPMDirect
		}
	}
	return npmLayoutKind(exe)
}

// npmLayoutKind recognizes the files npm/install.js leaves around the binary:
// <package>/bin/dropin-miner beside <package>/bin/dropin-miner.js, with
// <package>/package.json and <package>/install.js.
func npmLayoutKind(exe string) launchKind {
	binDir := filepath.Dir(exe)
	if filepath.Base(binDir) != "bin" {
		return launchNative
	}
	root := filepath.Dir(binDir)
	manifest, manifestErr := os.ReadFile(filepath.Join(root, "package.json")) // #nosec G304 -- a fixed name beside this executable
	_, installErr := os.Lstat(filepath.Join(root, "install.js"))
	_, wrapperErr := os.Lstat(filepath.Join(binDir, "dropin-miner.js"))
	absent := 0
	for _, err := range []error{manifestErr, installErr, wrapperErr} {
		if errors.Is(err, fs.ErrNotExist) {
			absent++
		}
	}
	switch {
	case absent == 3:
		return launchNative
	case manifestErr != nil || installErr != nil || wrapperErr != nil:
		return launchAmbiguous
	}
	var pkg struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(manifest, &pkg) != nil || pkg.Name != npmPackageName {
		return launchAmbiguous
	}
	return launchNPMLayout
}

// npmRemovalGuidance is how a participant removes or updates a copy the
// package manager owns.
func npmRemovalGuidance(kind launchKind, verb string) string {
	switch kind {
	case launchNPMGlobal:
		if verb == "upgrade" {
			return "npm owns this copy: npm install -g dropin-miner@latest"
		}
		return "npm owns this copy: npm uninstall -g dropin-miner"
	case launchNPMLocal:
		return "this copy belongs to a project's node_modules: " + verb + " it through that project's dependencies (package.json and its lockfile)"
	case launchAmbiguous:
		return "this copy sits beside some of npm's package files but not all of them, so who owns it is unclear; " + verb + " it the way it was installed"
	default:
		return "npm owns this copy; " + verb + " it with the package manager that installed it"
	}
}

// checkSetupLaunch decides whether this binary may be the one every hook and
// skill is written to call: a native copy or a global npm install. Anything
// npm or a project will discard is refused. An ambiguous layout is accepted,
// as it always was: setup's question is only whether the path will survive.
func checkSetupLaunch(exe, launch string) error {
	switch classifyLaunch(exe, launch) {
	case launchNative, launchNPMGlobal, launchAmbiguous:
		return nil
	case launchNPMCache:
		return fmt.Errorf("%w (this copy runs from npm's temporary cache, %s)", errNpmLaunch, exe)
	case launchNPMDirect, launchNPMLayout:
		return fmt.Errorf("%w (%s)", errNpmDirect, exe)
	case launchNPMLocal:
		return fmt.Errorf("%w (this copy is a project-local node_modules install, %s)", errNpmLaunch, exe)
	default:
		return fmt.Errorf("%w (npm could not say where this copy is installed: %s)", errNpmLaunch, exe)
	}
}

// checkUpgradeLaunch admits a native self-upgrade only for a native copy:
// every npm kind, and an ambiguous layout, is the package manager's to
// update, and the refusal says how.
func checkUpgradeLaunch(exe, launch string) error {
	if kind := classifyLaunch(exe, launch); kind != launchNative {
		return errors.New(npmRemovalGuidance(kind, "upgrade"))
	}
	return nil
}
