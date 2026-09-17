//go:build !windows

package main

// POSIX: owner-only is a mode, and the environment is a profile block.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/twilight-project/dropin-miner/pkg/fsx"
)

// restrictToOwner takes group and other access away. A directory is 0700,
// which is what the auth store and the wallet require; a file keeps its own
// owner bits (a read-only key stays read-only) and loses the rest.
func restrictToOwner(path string, dir bool) error {
	if dir {
		return os.Chmod(path, 0o700) // #nosec G302 G703 -- a directory: owner rwx is the most restrictive mode it can be used with; path is inside the installation or wallet directory setup owns
	}
	info, err := os.Lstat(path) // #nosec G703 -- path is inside the installation or wallet directory setup owns
	if err != nil {
		return err
	}
	return os.Chmod(path, info.Mode().Perm()&0o700) // #nosec G703 -- as above
}

// systemUserEnvironment has no POSIX meaning: the profile block is the
// environment setup leaves.
func systemUserEnvironment() userEnvironment { return nil }

// profileCandidate is setup.sh's choice: the profile of the shell $SHELL
// names, else ~/.bashrc when there is one.
func profileCandidate(userHome string, getenv func(string) string) string {
	shell := getenv("SHELL")
	switch {
	case strings.HasSuffix(shell, "zsh"):
		return filepath.Join(userHome, ".zshrc")
	case strings.HasSuffix(shell, "bash"):
		return filepath.Join(userHome, ".bashrc")
	}
	if lexists(filepath.Join(userHome, ".bashrc")) {
		return filepath.Join(userHome, ".bashrc")
	}
	return ""
}

func (r *setupRun) environmentStep() int {
	binDir := filepath.Dir(r.exe)
	walletDir := ""
	if lexists(filepath.Join(r.home, "wallet", walletKeyFile)) {
		walletDir = filepath.Join(r.home, "wallet")
	}
	lines := profileEnvLines(binDir, r.cfgPath, walletDir)

	r.say("Shell environment")
	if r.leftForOtherInstallation("shell profile", "Your shell profile belongs to that one", "pointing PATH and TOKENDROP_CONFIG here would repoint your real environment at this installation") {
		return exitOK
	}
	r.printf("These lines make the other commands short. Your key is not among them: a\n" +
		"search reads it from the stored credentials file (or TOKENDROP_API_KEY, if a\n" +
		"shell exports one, which then wins):\n\n")
	for _, l := range lines {
		r.printf("    %s\n", l)
	}

	candidate := profileCandidate(r.d.userHome, r.d.getenv)
	if candidate == "" {
		r.printf("No ~/.bashrc or ~/.zshrc to add them to.\n")
		return exitOK
	}
	if r.noProfile {
		r.printf("Left your shell profile alone (-no-profile).\n")
		r.skip("shell profile")
		return exitOK
	}
	target, existing, mode, err := profileTarget(candidate)
	if err != nil {
		r.printf("Not touching %s: %v. Add the lines above by hand.\n", candidate, err)
		return exitOK
	}
	next, err := rewriteProfile(existing, profileBlock(lines))
	if errors.Is(err, errProfileMalformed) {
		r.printf("Not touching %s: %v. Add the lines above to it by hand, between one start and one end marker:\n    %s\n    %s\n",
			candidate, err, profileMarkerStart, profileMarkerEnd)
		return exitOK
	}
	if bytes.Equal(next, existing) {
		r.shortCommands = true
		r.printf("%s already has them.\n", candidate)
		return exitOK
	}
	if r.dry {
		r.printf("(dry run) would write a dropin-miner block to %s\n", candidate)
		return exitOK
	}
	if !r.d.interactive && !r.yes {
		r.printf("Not an interactive shell — not touching %s (pass -yes to add them).\n", candidate)
		r.skip("shell profile")
		return exitOK
	}
	add, err := r.ask("Add them to " + tilde(r.d.userHome, candidate) + "?")
	if err != nil {
		return r.abort("your shell profile was not touched and the coding agents were not set up")
	}
	if !add {
		r.say("Left your shell profile alone")
		return exitOK
	}
	if err := fsx.WriteFileAtomic(filepath.Dir(target), filepath.Base(target), next, mode); err != nil {
		r.printf("Could not write %s: %v. Add the lines above by hand.\n", candidate, err)
		return exitOK
	}
	r.changed = true
	r.shortCommands = true
	r.say("Added a dropin-miner block to " + candidate)
	r.printf("  open a new shell, or: source %s\n", shellQuote(candidate))
	return exitOK
}

// profileEnvLines are the lines setup.sh put in the profile, quoted. The
// PATH test compares the quoted directory literally inside a case pattern,
// so a directory holding a glob character is matched as itself.
func profileEnvLines(binDir, cfgPath, walletDir string) []string {
	q := shellQuote(binDir)
	lines := []string{
		`case ":$PATH:" in *:` + q + `:*) ;; *) export PATH="$PATH:"` + q + ` ;; esac`,
		"export TOKENDROP_CONFIG=" + shellQuote(cfgPath),
	}
	if walletDir != "" {
		lines = append(lines, "export TOKENDROP_WALLET_DIR="+shellQuote(walletDir))
	}
	return lines
}
