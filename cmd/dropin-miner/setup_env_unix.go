//go:build !windows

package main

// POSIX: owner-only is a mode, and the environment is a profile block.

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
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
		return os.Chmod(path, 0o700) // #nosec G302 -- a directory: owner rwx is the most restrictive mode it can be used with
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return os.Chmod(path, info.Mode().Perm()&0o700)
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

func (r *setupRun) environmentStep() {
	binDir := filepath.Dir(r.exe)
	walletDir := ""
	if lexists(filepath.Join(r.home, "wallet", walletKeyFile)) {
		walletDir = filepath.Join(r.home, "wallet")
	}
	lines := profileEnvLines(binDir, r.cfgPath, walletDir)

	r.say("Shell environment")
	r.printf("These lines make the other commands short. Your key is not among them: a\n" +
		"search reads it from the stored credentials file (or TOKENDROP_API_KEY, if a\n" +
		"shell exports one, which then wins):\n\n")
	for _, l := range lines {
		r.printf("    %s\n", l)
	}

	candidate := profileCandidate(r.d.userHome, r.d.getenv)
	if candidate == "" {
		r.printf("No ~/.bashrc or ~/.zshrc to add them to.\n")
		return
	}
	if r.noProfile {
		r.printf("Left your shell profile alone (-no-profile).\n")
		return
	}
	target, existing, mode, err := profileTarget(candidate)
	if err != nil {
		r.printf("Not touching %s: %v. Add the lines above by hand.\n", candidate, err)
		return
	}
	next, err := rewriteProfile(existing, profileBlock(lines))
	if errors.Is(err, errProfileMalformed) {
		r.printf("Not touching %s: %v. Add the lines above to it by hand, between one start and one end marker:\n    %s\n    %s\n",
			candidate, err, profileMarkerStart, profileMarkerEnd)
		return
	}
	if bytes.Equal(next, existing) {
		r.shortCommands = true
		r.printf("%s already has them.\n", candidate)
		return
	}
	if r.dry {
		r.printf("(dry run) would write a dropin-miner block to %s\n", candidate)
		return
	}
	if !r.d.interactive && !r.yes {
		r.printf("Not an interactive shell — not touching %s (pass -yes to add them).\n", candidate)
		return
	}
	if !r.ask("Add them to " + tilde(r.d.userHome, candidate) + "?") {
		r.say("Left your shell profile alone")
		return
	}
	if err := fsx.WriteFileAtomic(filepath.Dir(target), filepath.Base(target), next, mode); err != nil {
		r.printf("Could not write %s: %v. Add the lines above by hand.\n", candidate, err)
		return
	}
	r.changed = true
	r.shortCommands = true
	r.say("Added a dropin-miner block to " + candidate)
	r.printf("  open a new shell, or: source %s\n", shellQuote(candidate))
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

// profileTarget resolves the file an edit of path must actually replace.
// A profile that is a symlink is edited through the link: the regular file
// it points at is replaced in its own directory, and the link stays. A
// target that is not a regular file is refused like malformed markers.
func profileTarget(path string) (target string, existing []byte, mode fs.FileMode, err error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return path, nil, 0o644, nil
	}
	if err != nil {
		return "", nil, 0, err
	}
	target = path
	if info.Mode()&fs.ModeSymlink != 0 {
		resolved, rerr := filepath.EvalSymlinks(path)
		if rerr != nil {
			return "", nil, 0, fmt.Errorf("it is a symlink whose target cannot be resolved: %w", rerr)
		}
		target = resolved
		if info, err = os.Lstat(target); err != nil {
			return "", nil, 0, err
		}
	}
	if !info.Mode().IsRegular() {
		return "", nil, 0, fmt.Errorf("%s is not a regular file", target)
	}
	data, err := os.ReadFile(target) // #nosec G304 -- the participant's own shell profile, chosen from $SHELL
	if err != nil {
		return "", nil, 0, err
	}
	return target, data, info.Mode().Perm(), nil
}
