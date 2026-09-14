package main

// The environment setup leaves behind, in the two shapes it takes.
//
// POSIX: one marked block in the participant's shell profile. The block is
// the whole record of what setup put there, so a replacement edits exactly
// one well-formed block and nothing else; a profile whose markers are not
// one well-formed block is not edited at all, because there is no reading
// of a start without its end that makes truncating to the end of the file
// the right thing to do. Every path in the block is single-quoted for the
// shell.
//
// Windows: no profile. Setup owns two values in the User environment, and
// $HOME_DIR/setup-env.json is the journal of what it changed there — a delta,
// not a snapshot. It is written before the environment is touched, and it
// records whether setup itself added the PATH entry and what TOKENDROP_CONFIG
// held before setup set it, so an uninstall can take out exactly what setup
// put in and put back exactly what was there. A rerun reconciles against the
// journal and never mistakes setup's own earlier value for the original.
//
// The logic lives here, platform-neutral, over the userEnvironment seam; the
// registry is only the Windows backend (setup_env_windows.go).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/twilight-project/dropin-miner/pkg/fsx"
)

const (
	profileMarkerStart = "# >>> dropin-miner >>>"
	profileMarkerEnd   = "# <<< dropin-miner <<<"
	profileBlockNote   = "# Written by dropin-miner setup. Delete this block to undo."
)

// shellQuote single-quotes s for a POSIX shell: inside single quotes nothing
// is special, and an embedded single quote is closed, escaped and reopened.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func profileBlock(lines []string) string {
	return profileMarkerStart + "\n" + profileBlockNote + "\n" + strings.Join(lines, "\n") + "\n" + profileMarkerEnd + "\n"
}

// errProfileMalformed is a profile whose markers are not exactly one
// well-formed block.
var errProfileMalformed = errors.New("its dropin-miner markers are not one well-formed block")

// rewriteProfile returns the profile with exactly one dropin-miner block
// holding block. The existing block, if there is one, is replaced in place;
// otherwise the block is appended. Anything but zero markers or exactly one
// start followed by exactly one end is refused.
func rewriteProfile(existing []byte, block string) ([]byte, error) {
	lines := strings.SplitAfter(string(existing), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	start, end := -1, -1
	starts, ends := 0, 0
	for i, l := range lines {
		switch strings.TrimRight(l, "\r\n") {
		case profileMarkerStart:
			starts++
			start = i
		case profileMarkerEnd:
			ends++
			end = i
		}
	}
	switch {
	case starts == 0 && ends == 0:
		var b strings.Builder
		b.Write(existing)
		if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
			b.WriteString("\n")
		}
		b.WriteString(block)
		return []byte(b.String()), nil
	case starts == 1 && ends == 1 && start < end:
		var b strings.Builder
		for _, l := range lines[:start] {
			b.WriteString(l)
		}
		b.WriteString(block)
		for _, l := range lines[end+1:] {
			b.WriteString(l)
		}
		return []byte(b.String()), nil
	default:
		return nil, fmt.Errorf("%w (%d start, %d end)", errProfileMalformed, starts, ends)
	}
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

// removeProfileBlock is rewriteProfile's inverse: the profile without its one
// dropin-miner block, and the block's own lines. Zero markers is found=false
// and the bytes unchanged; anything but exactly one start followed by one
// end is errProfileMalformed, and nothing is guessed.
func removeProfileBlock(existing []byte) (next []byte, block []string, found bool, err error) {
	lines := strings.SplitAfter(string(existing), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	start, end := -1, -1
	starts, ends := 0, 0
	for i, l := range lines {
		switch strings.TrimRight(l, "\r\n") {
		case profileMarkerStart:
			starts++
			start = i
		case profileMarkerEnd:
			ends++
			end = i
		}
	}
	switch {
	case starts == 0 && ends == 0:
		return existing, nil, false, nil
	case starts == 1 && ends == 1 && start < end:
		var b strings.Builder
		for _, l := range lines[:start] {
			b.WriteString(l)
		}
		for _, l := range lines[end+1:] {
			b.WriteString(l)
		}
		for _, l := range lines[start+1 : end] {
			block = append(block, strings.TrimRight(l, "\r\n"))
		}
		return []byte(b.String()), block, true, nil
	default:
		return nil, nil, false, fmt.Errorf("%w (%d start, %d end)", errProfileMalformed, starts, ends)
	}
}

// profileBlockConfig is the TOKENDROP_CONFIG a block exports, as written:
// shell-quoted by setup, bare by the setup.sh that preceded it.
func profileBlockConfig(block []string) (string, bool) {
	const prefix = "export TOKENDROP_CONFIG="
	for _, l := range block {
		if strings.HasPrefix(l, prefix) {
			return strings.TrimPrefix(l, prefix), true
		}
	}
	return "", false
}

// profileBlockNamesConfig reports whether a block exports exactly cfgPath,
// in either form setup has written it.
func profileBlockNamesConfig(block []string, cfgPath string) bool {
	value, ok := profileBlockConfig(block)
	return ok && (value == shellQuote(cfgPath) || value == cfgPath)
}

// ── Windows: the User environment and its journal ─────────────────────────

// userEnvironment is the User-scope environment store. The registry backs it
// on Windows; tests use a map.
type userEnvironment interface {
	Get(name string) (value string, present bool, err error)
	Set(name, value string) error
	// Delete removes the value; an absent one is not an error.
	Delete(name string) error
	Broadcast()
}

const setupEnvJournalFile = "setup-env.json"

type envJournalPath struct {
	Entry        string `json:"entry"`
	AddedBySetup bool   `json:"added_by_setup"`
}

type envJournalConfig struct {
	ValueSet        string `json:"value_set"`
	PreviousPresent bool   `json:"previous_present"`
	PreviousValue   string `json:"previous_value"`
}

type envJournal struct {
	Version         int              `json:"version"`
	Path            envJournalPath   `json:"path"`
	TokendropConfig envJournalConfig `json:"tokendrop_config"`
}

// readEnvJournal returns (nil, nil) when there is no journal. One that exists
// but cannot be trusted is an error: reconciling past it would lose the
// original value it recorded.
func readEnvJournal(path string) (*envJournal, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- $HOME_DIR/setup-env.json
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var j envJournal
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("%s is not readable: %w", path, err)
	}
	if j.Version != 1 {
		return nil, fmt.Errorf("%s has version %d; this setup understands version 1", path, j.Version)
	}
	return &j, nil
}

// pathEntryEquivalent compares PATH entries the way Windows resolves them:
// case-insensitively, after cleaning, with a trailing separator ignored.
func pathEntryEquivalent(a, b string) bool {
	clean := func(s string) string {
		s = strings.TrimSpace(s)
		if s == "" {
			return ""
		}
		return strings.TrimRight(filepath.Clean(s), `\/`)
	}
	ca, cb := clean(a), clean(b)
	return ca != "" && strings.EqualFold(ca, cb)
}

func pathHasEntry(pathValue, entry string) bool {
	for _, e := range strings.Split(pathValue, ";") {
		if pathEntryEquivalent(e, entry) {
			return true
		}
	}
	return false
}

// envChange is what applyUserEnvironment will do, computed before any of
// it is done.
type envChange struct {
	journal      envJournal
	writeJournal bool
	addPath      bool
	newPath      string
	setConfig    bool
}

// planUserEnvironment reconciles the desired state (binDir on the user PATH,
// TOKENDROP_CONFIG = cfgPath) against the store and any earlier journal.
func planUserEnvironment(env userEnvironment, prior *envJournal, binDir, cfgPath string) (envChange, error) {
	pathValue, _, err := env.Get("Path")
	if err != nil {
		return envChange{}, fmt.Errorf("read the user Path: %w", err)
	}
	cfgValue, cfgPresent, err := env.Get("TOKENDROP_CONFIG")
	if err != nil {
		return envChange{}, fmt.Errorf("read the user TOKENDROP_CONFIG: %w", err)
	}

	var c envChange
	c.journal.Version = 1

	present := pathHasEntry(pathValue, binDir)
	addedBefore := prior != nil && prior.Path.AddedBySetup && pathEntryEquivalent(prior.Path.Entry, binDir)
	switch {
	case !present:
		// Absent now: setup adds it, whether or not it did once before and
		// the participant took it out since.
		c.addPath = true
		c.journal.Path = envJournalPath{Entry: binDir, AddedBySetup: true}
		if strings.TrimRight(pathValue, ";") == "" {
			c.newPath = binDir
		} else {
			c.newPath = strings.TrimRight(pathValue, ";") + ";" + binDir
		}
	case addedBefore:
		c.journal.Path = prior.Path
	default:
		// Present, and not because of setup: nothing to undo later.
		c.journal.Path = envJournalPath{Entry: binDir, AddedBySetup: false}
	}

	if prior != nil {
		// The original is whatever the first run found. A rerun sees setup's
		// own value there and must not record that as the thing to restore.
		c.journal.TokendropConfig = prior.TokendropConfig
	} else {
		c.journal.TokendropConfig = envJournalConfig{PreviousPresent: cfgPresent, PreviousValue: cfgValue}
	}
	c.journal.TokendropConfig.ValueSet = cfgPath
	c.setConfig = !cfgPresent || cfgValue != cfgPath

	c.writeJournal = prior == nil || *prior != c.journal
	return c, nil
}

// applyUserEnvironment writes the journal durably, then changes the
// environment, then tells running programs. Journal first: an environment
// changed with no record of what it was before is the one state an uninstall
// could not undo correctly.
func applyUserEnvironment(env userEnvironment, journalPath string, c envChange) error {
	if c.writeJournal {
		data, err := json.MarshalIndent(c.journal, "", "  ")
		if err != nil {
			return err
		}
		if err := fsx.WriteFileAtomic(filepath.Dir(journalPath), filepath.Base(journalPath), append(data, '\n'), 0o600); err != nil {
			return fmt.Errorf("write %s: %w", journalPath, err)
		}
	}
	changed := false
	if c.addPath {
		if err := env.Set("Path", c.newPath); err != nil {
			return fmt.Errorf("add %s to the user Path: %w", c.journal.Path.Entry, err)
		}
		changed = true
	}
	if c.setConfig {
		if err := env.Set("TOKENDROP_CONFIG", c.journal.TokendropConfig.ValueSet); err != nil {
			return fmt.Errorf("set the user TOKENDROP_CONFIG: %w", err)
		}
		changed = true
	}
	if changed {
		env.Broadcast()
	}
	return nil
}

// ── uninstall: compare and revert ──────────────────────────────────────────

// configRevert is what an uninstall does with TOKENDROP_CONFIG.
type configRevert int

const (
	configUntouched configRevert = iota // absent now: nothing of setup's is left
	configRestore                       // still setup's value: put the previous one back
	configDelete                        // still setup's value, and there was none before
	configCeded                         // changed since setup: the participant's now
)

// envRevert is an uninstall's plan against the User environment, computed
// from the journal and the environment as they are now, never from a
// snapshot: a PATH entry the participant added since setup survives it.
type envRevert struct {
	removePath bool
	newPath    string
	config     configRevert
	restore    string
}

func (r envRevert) changes() bool {
	return r.removePath || r.config == configRestore || r.config == configDelete
}

// planUserEnvironmentRevert takes out one entry equivalent to the journal's
// PATH entry only if setup added it, and reverts TOKENDROP_CONFIG only while
// it still holds the value setup set.
func planUserEnvironmentRevert(env userEnvironment, j envJournal) (envRevert, error) {
	var r envRevert
	if j.Path.AddedBySetup {
		pathValue, _, err := env.Get("Path")
		if err != nil {
			return envRevert{}, fmt.Errorf("read the user Path: %w", err)
		}
		entries := strings.Split(pathValue, ";")
		for i, e := range entries {
			if pathEntryEquivalent(e, j.Path.Entry) {
				r.removePath = true
				r.newPath = strings.Join(append(entries[:i:i], entries[i+1:]...), ";")
				break
			}
		}
	}
	cfgValue, present, err := env.Get("TOKENDROP_CONFIG")
	if err != nil {
		return envRevert{}, fmt.Errorf("read the user TOKENDROP_CONFIG: %w", err)
	}
	switch {
	case !present:
		r.config = configUntouched
	case cfgValue != j.TokendropConfig.ValueSet:
		r.config = configCeded
	case j.TokendropConfig.PreviousPresent:
		r.config, r.restore = configRestore, j.TokendropConfig.PreviousValue
	default:
		r.config = configDelete
	}
	return r, nil
}

// applyUserEnvironmentRevert changes the environment and only then removes
// the journal. A failed change keeps the journal, so the next uninstall can
// finish the same revert instead of guessing.
func applyUserEnvironmentRevert(env userEnvironment, journalPath string, r envRevert) error {
	if r.removePath {
		if err := env.Set("Path", r.newPath); err != nil {
			return fmt.Errorf("remove setup's entry from the user Path: %w", err)
		}
	}
	switch r.config {
	case configRestore:
		if err := env.Set("TOKENDROP_CONFIG", r.restore); err != nil {
			return fmt.Errorf("restore the user TOKENDROP_CONFIG: %w", err)
		}
	case configDelete:
		if err := env.Delete("TOKENDROP_CONFIG"); err != nil {
			return fmt.Errorf("remove the user TOKENDROP_CONFIG: %w", err)
		}
	}
	if r.changes() {
		env.Broadcast()
	}
	if err := os.Remove(journalPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", journalPath, err)
	}
	return nil
}
