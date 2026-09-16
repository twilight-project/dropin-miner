package main

// A previous installation, found and — only when the participant says so at
// a terminal — adopted.
//
// Nothing we ship deletes the installation directory: it holds the wallet,
// the registration and the stored key. A participant who removed the miner
// and comes back, or set the directory aside, should get those back rather
// than a second wallet and a second registration.
//
// Adoption moves bundles, never loose files, because some state is only
// meaningful together. The identity bundle is state/ and credentials.json:
// a refresh token without the DPoP key it is bound to fails every request,
// and a platform key beside someone else's agent.json is a registration that
// belongs to nobody. So the identity moves whole or not at all, and a
// destination that already holds an identity is a conflict — neither half
// moves, the source is left exactly as it was, and both paths are named.
// The wallet moves whole or stays. Spool, intake and session files are
// independent records and merge file by file, never overwriting one that is
// already there.
//
// Every source object is checked before it moves: a regular file or a
// directory, never a symlink (a moved link would point the runtime's custody
// checks at whatever it names). Each object is restricted to its owner again
// immediately after the move, so no intermediate state is weaker than what
// the auth store accepts at runtime.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/auth"
	"github.com/twilight-project/dropin-miner/pkg/fsx"
)

// installationMarkers are the files whose presence makes a directory an
// installation. registration_pending.json is one: an interrupted
// registration is durable state, and ignoring it would mint a second
// identity for the same participant.
var installationMarkers = []string{
	filepath.Join("wallet", walletKeyFile),
	filepath.Join("state", "refresh.token"),
	filepath.Join("state", "agent.json"),
	filepath.Join("state", "registration_pending.json"),
	credentialsFile,
}

// identityStateFiles are the state/ files that make a destination's state an
// identity rather than an unfinished enrollment's leftovers.
var identityStateFiles = []string{"refresh.token", "agent.json", "registration_pending.json", "participation.secret"}

func lexists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func hasInstallation(dir string) bool {
	for _, m := range installationMarkers {
		if lexists(filepath.Join(dir, m)) {
			return true
		}
	}
	return false
}

// setAsideInstallation is the newest sibling of home — home.<anything> or
// home-<anything>, a real directory, not a link — that holds an installation.
func setAsideInstallation(home string) string {
	parent, base := filepath.Dir(home), filepath.Base(home)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return ""
	}
	type candidate struct {
		path string
		mod  time.Time
	}
	var found []candidate
	for _, e := range entries {
		name := e.Name()
		if name == base || (!strings.HasPrefix(name, base+".") && !strings.HasPrefix(name, base+"-")) {
			continue
		}
		// The lifecycle gate matches home.* by construction; it coordinates
		// this installation and is never a previous one, whatever it is.
		if name == base+lifecycleGateSuffix {
			continue
		}
		path := filepath.Join(parent, name)
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() {
			continue
		}
		if hasInstallation(path) {
			found = append(found, candidate{path, info.ModTime()})
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].mod.After(found[j].mod) })
	if len(found) == 0 {
		return ""
	}
	return found[0].path
}

// describeInstallation says what a directory holds, in the script's words.
// The wallet address comes from the public sidecar; no process is run and no
// passphrase is needed.
func describeInstallation(dir string) string {
	var parts []string
	if lexists(filepath.Join(dir, "wallet", walletKeyFile)) {
		addr := "?"
		var sc sidecar
		if err := readWalletFile(filepath.Join(dir, "wallet"), walletSidecarFile, &sc); err == nil && sc.Address != "" {
			addr = sc.Address
		}
		parts = append(parts, "wallet "+addr)
	}
	if lexists(filepath.Join(dir, "state", "refresh.token")) || lexists(filepath.Join(dir, "state", "agent.json")) {
		parts = append(parts, "enrolled")
	}
	if lexists(filepath.Join(dir, "state", "registration_pending.json")) {
		parts = append(parts, "an unfinished registration")
	}
	if lexists(filepath.Join(dir, credentialsFile)) {
		parts = append(parts, "stored API key")
	}
	if treeHasFiles(filepath.Join(dir, "spool")) {
		parts = append(parts, "unsent spool")
	}
	return strings.Join(parts, ", ")
}

func treeHasFiles(dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// validateTree refuses anything but regular files and directories, anywhere
// under root (root included).
func validateTree(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error { // #nosec G703 -- root is a bundle inside the installation directory or its set-aside sibling
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink", path)
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file or directory", path)
		}
		return nil
	})
}

// dirEmpty reports whether path is a real, empty directory.
func dirEmpty(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) == 0
}

// adoption is one run of moving bundles from src into dst.
type adoption struct {
	src, dst string
	dry      bool
	now      time.Time
	out      io.Writer
	// restrict re-applies owner-only access to a moved object.
	restrict func(path string, dir bool) error
	// moveFn publishes one object at a new name; fsx.MoveFileDurable unless a
	// test injects a failure.
	moveFn func(from, to string) error

	moved []string // destination paths, for the narration
}

func (a *adoption) say(format string, args ...any) {
	fmt.Fprintf(a.out, "  "+format+"\n", args...)
}

func (a *adoption) doMove(from, to string) error {
	if a.moveFn != nil {
		return a.moveFn(from, to)
	}
	return fsx.MoveFileDurable(from, to)
}

// bundleTxn is a journal of changes, in order, each with the change that
// undoes it. The custody transaction is one bundleTxn kept open across the
// identity and the wallet; the per-file merges use a throwaway one only for
// its move-and-restrict. It lives in memory for one run: nothing persists a
// partial adoption.
type bundleTxn struct {
	a     *adoption
	done  []txnStep
	paths []string // every path a part of a bundle may be at, for a failed rollback's report
}

type txnStep struct {
	what string
	undo func() error
}

func (a *adoption) txn(paths ...string) *bundleTxn { return &bundleTxn{a: a, paths: paths} }

// move publishes one object and restricts it at its new name. A restriction
// that fails is a failed move: the move is recorded first, so the rollback
// takes it back out.
func (t *bundleTxn) move(from, to string) error {
	a := t.a
	if a.dry {
		a.say("would move %s -> %s", from, to)
		return nil
	}
	info, err := os.Lstat(from)
	if err != nil {
		return err
	}
	if err := a.doMove(from, to); err != nil {
		return fmt.Errorf("move %s to %s: %w", from, to, err)
	}
	t.done = append(t.done, txnStep{what: "move " + to + " back to " + from, undo: func() error { return a.doMove(to, from) }})
	a.moved = append(a.moved, to)
	if err := a.restrict(to, info.IsDir()); err != nil {
		return fmt.Errorf("restrict %s to its owner: %w", to, err)
	}
	return nil
}

// makeRoom removes an empty destination directory so a whole bundle can take
// its name. os.Remove refuses a directory that is not empty, so this can
// never delete anything; the rollback recreates it.
func (t *bundleTxn) makeRoom(path string) error {
	if t.a.dry || !lexists(path) {
		return nil
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	t.done = append(t.done, txnStep{what: "recreate " + path, undo: func() error {
		if err := os.Mkdir(path, 0o700); err != nil {
			return err
		}
		return t.a.restrict(path, true)
	}})
	return nil
}

// fail rolls the whole journal back, newest change first — for the custody
// transaction that is everything the identity and the wallet stages moved —
// and returns the failure. A rollback that cannot finish stops at once and
// names every path a part of either bundle is now at; nothing is attempted
// after it.
func (t *bundleTxn) fail(bundle string, cause error) *adoptionFailure {
	f := &adoptionFailure{Bundle: bundle, Reason: cause.Error(), Source: t.a.src, Destination: t.a.dst}
	for i := len(t.done) - 1; i >= 0; i-- {
		if err := t.done[i].undo(); err != nil {
			f.RollbackFailed = fmt.Sprintf("%s: %v", t.done[i].what, err)
			for _, p := range t.paths {
				if lexists(p) {
					f.Surviving = append(f.Surviving, p)
				}
			}
			return f
		}
	}
	return f
}

// identityConflict is the typed result of an adoption that found an identity
// on both sides. It is not narration: setup stops on it, before anything
// else moves and before connect runs, because connect on the destination as
// it stands would register a second identity for the same participant.
type identityConflict struct {
	Destination string // the installation directory
	Evidence    string // the destination file that makes it an identity
	Source      string // the set-aside installation
}

func (c *identityConflict) Error() string {
	return fmt.Sprintf("identity conflict: %s (holds %s) and %s each hold an identity", c.Destination, c.Evidence, c.Source)
}

// adoptionFailure is the typed result of a custody transaction (the identity
// and the wallet) that failed after the participant accepted the adoption:
// Bundle names the stage that failed. Everything the transaction had moved,
// in either stage, has been put back, unless RollbackFailed says otherwise,
// in which case Surviving names every path a part of either bundle is now at.
type adoptionFailure struct {
	Bundle         string // "identity (state and stored key)" or "wallet"
	Reason         string
	Source         string
	Destination    string
	RollbackFailed string // empty when everything was put back
	Surviving      []string
}

func (f *adoptionFailure) Error() string {
	return fmt.Sprintf("could not adopt the %s from %s into %s: %s", f.Bundle, f.Source, f.Destination, f.Reason)
}

// run adopts the accepted installation.
//
// The identity and the wallet are one outer custody transaction: one
// journal, open across both stages and unwound newest-first on any conflict
// after a move, refusal, or failure to move or restrict. The invariant, at
// the point connect could run: either both custody bundles of the accepted
// installation were adopted coherently, or both remain where they started —
// never an identity adopted beside a wallet left behind, which would register
// and pay out against custody the participant did not keep together. Spool,
// intake, sessions and config are independent records with per-file rules,
// and move only after that outer commit.
func (a *adoption) run() error {
	t := a.txn(
		filepath.Join(a.src, "state"), filepath.Join(a.src, credentialsFile), filepath.Join(a.src, "wallet"),
		filepath.Join(a.dst, "state"), filepath.Join(a.dst, credentialsFile), filepath.Join(a.dst, "wallet"),
		a.aside("state.unenrolled"), a.aside("wallet.incomplete"),
	)
	if err := a.identity(t); err != nil {
		return err
	}
	if err := a.custody(t); err != nil {
		return err
	}
	// Outer commit: both custody bundles are in place, or had nothing to move.
	for _, name := range []string{"spool", "intake", "sessions"} {
		a.merge(name)
	}
	a.config()
	return nil
}

const identityBundle = "identity (state and stored key)"

// aside names a set-aside copy in the destination: prefix-<UTC time>.
func (a *adoption) aside(prefix string) string {
	return filepath.Join(a.dst, prefix+"-"+a.now.UTC().Format("20060102150405"))
}

// identity is the custody transaction's first stage: state/ and
// credentials.json together, or neither. A destination that already holds an
// identity is returned as a conflict; it is found before this stage moves
// anything, and this stage is the first.
func (a *adoption) identity(t *bundleTxn) error {
	srcState, srcCreds := filepath.Join(a.src, "state"), filepath.Join(a.src, credentialsFile)
	dstState, dstCreds := filepath.Join(a.dst, "state"), filepath.Join(a.dst, credentialsFile)
	aside := a.aside("state.unenrolled")
	hasState, hasCreds := lexists(srcState), lexists(srcCreds)
	if !hasState && !hasCreds {
		return nil
	}
	for _, p := range []string{srcState, srcCreds} {
		if !lexists(p) {
			continue
		}
		if err := validateTree(p); err != nil {
			return t.fail(identityBundle, err)
		}
	}
	if hasState {
		if info, _ := os.Lstat(srcState); !info.IsDir() {
			return t.fail(identityBundle, fmt.Errorf("%s is not a directory", srcState))
		}
	}

	conflict := func(evidence string) *identityConflict {
		return &identityConflict{Destination: a.dst, Evidence: evidence, Source: a.src}
	}
	if lexists(dstCreds) {
		return conflict(dstCreds)
	}
	setAside := false
	if lexists(dstState) {
		info, err := os.Lstat(dstState)
		if err != nil || !info.IsDir() {
			return conflict(dstState)
		}
		entries, err := os.ReadDir(dstState)
		if err != nil {
			return conflict(dstState)
		}
		for _, name := range identityStateFiles {
			if lexists(filepath.Join(dstState, name)) {
				return conflict(filepath.Join(dstState, name))
			}
		}
		setAside = len(entries) > 0
	}

	if setAside {
		// An enrollment that never finished left a DPoP key (and perhaps a
		// lock or a decision) behind. Its key would shadow the real one, so
		// the whole directory goes aside — renamed, never deleted — and comes
		// back if the adoption does not complete.
		if err := t.move(dstState, aside); err != nil {
			return t.fail(identityBundle, fmt.Errorf("could not set %s aside: %w", dstState, err))
		}
		a.say("set aside %s from an unfinished enrollment as %s", dstState, aside)
	} else if err := t.makeRoom(dstState); err != nil {
		return t.fail(identityBundle, err)
	}

	if hasState {
		if err := t.move(srcState, dstState); err != nil {
			return t.fail(identityBundle, err)
		}
	}
	if hasCreds {
		// Half an identity is worse than none: a failure here puts the state
		// back too.
		if err := t.move(srcCreds, dstCreds); err != nil {
			return t.fail(identityBundle, err)
		}
	}
	a.say("adopted the identity (state and stored key)")
	return nil
}

// custody is the custody transaction's second stage: the wallet, whole or
// not at all. Any failure here unwinds the identity stage too.
//
// A destination wallet/ that is not empty is partial state, never a wallet to
// keep: one holding a wallet key would have made the destination an
// installation, and adoption would not have been offered. So it is set aside
// as wallet.incomplete-<time> under the same rollback rules as an unfinished
// state/, and the participant's chosen source wallet is never discarded in
// its favor.
func (a *adoption) custody(t *bundleTxn) error {
	src, dst := filepath.Join(a.src, "wallet"), filepath.Join(a.dst, "wallet")
	if !lexists(src) {
		return nil
	}
	if err := validateTree(src); err != nil {
		return t.fail("wallet", err)
	}
	if info, _ := os.Lstat(src); !info.IsDir() {
		return t.fail("wallet", fmt.Errorf("%s is not a directory", src))
	}
	if lexists(dst) && !dirEmpty(dst) {
		aside := a.aside("wallet.incomplete")
		if err := t.move(dst, aside); err != nil {
			return t.fail("wallet", fmt.Errorf("could not set %s aside: %w", dst, err))
		}
		a.say("set aside %s, a wallet directory with no wallet key, as %s", dst, aside)
	} else if err := t.makeRoom(dst); err != nil {
		return t.fail("wallet", err)
	}
	if err := t.move(src, dst); err != nil {
		return t.fail("wallet", err)
	}
	// A move keeps each object's access list as it was at the source, entries
	// inherited there included, and restricting the directory does not remove
	// an entry a file already carries: the whole tree is secured, and a wallet
	// object that cannot be is a failed wallet stage (wallet_acl.go).
	if !a.dry {
		if _, err := secureWalletTree(dst, a.restrict); err != nil {
			return t.fail("wallet", err)
		}
	}
	a.say("adopted the wallet")
	return nil
}

// merge moves every record under src/name that dst/name does not already
// have, recursing into directories both sides hold.
func (a *adoption) merge(name string) {
	src, dst := filepath.Join(a.src, name), filepath.Join(a.dst, name)
	info, err := os.Lstat(src)
	if err != nil {
		return
	}
	if !info.IsDir() {
		a.say("not adopting %s: %s is not a directory", name, src)
		return
	}
	var moved, kept, refused int
	a.mergeDir(src, dst, &moved, &kept, &refused)
	if moved+kept+refused == 0 {
		return
	}
	msg := fmt.Sprintf("adopted %s (%d moved", name, moved)
	if kept > 0 {
		msg += fmt.Sprintf(", %d already present kept", kept)
	}
	if refused > 0 {
		msg += fmt.Sprintf(", %d not a regular file or directory and left in place", refused)
	}
	a.say("%s)", msg)
}

func (a *adoption) mergeDir(src, dst string, moved, kept, refused *int) {
	entries, err := os.ReadDir(src)
	if err != nil {
		*refused++
		return
	}
	if !lexists(dst) && !a.dry {
		if err := os.Mkdir(dst, 0o700); err != nil {
			*refused += len(entries)
			return
		}
		_ = a.restrict(dst, true)
	}
	for _, e := range entries {
		from, to := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		info, err := os.Lstat(from)
		if err != nil || (info.Mode()&fs.ModeSymlink != 0) || (!info.IsDir() && !info.Mode().IsRegular()) {
			*refused++
			continue
		}
		if lexists(to) {
			if info.IsDir() {
				if toInfo, err := os.Lstat(to); err == nil && toInfo.IsDir() {
					a.mergeDir(from, to, moved, kept, refused)
					continue
				}
			}
			*kept++
			continue
		}
		if info.IsDir() && validateTree(from) != nil {
			// Something inside cannot move whole; take what can move.
			a.mergeDir(from, to, moved, kept, refused)
			continue
		}
		if err := a.txn().move(from, to); err != nil {
			*refused++
			continue
		}
		*moved++
	}
	if !a.dry {
		_ = os.Remove(src) // only succeeds when everything moved
	}
}

// config adopts tokendrop.toml when the destination has none. The migration
// policy then applies to it exactly as to any existing config.
func (a *adoption) config() {
	src, dst := filepath.Join(a.src, setupConfigFile), filepath.Join(a.dst, setupConfigFile)
	if !lexists(src) {
		return
	}
	if lexists(dst) {
		a.say("keeping the config already at %s (not overwritten by %s)", dst, src)
		return
	}
	if err := validateTree(src); err != nil {
		a.say("not adopting the config: %v", err)
		return
	}
	if err := a.txn().move(src, dst); err != nil {
		a.say("not adopting the config: %v", err)
		return
	}
	a.say("adopted the config")
}

// finish removes the source only when it is proven empty; this is the one
// deletion setup performs, and it cannot delete participant data because an
// empty directory holds none.
func (a *adoption) finish() {
	if a.dry {
		return
	}
	if err := os.Remove(a.src); err == nil {
		a.say("removed the now-empty %s", a.src)
		return
	}
	entries, _ := os.ReadDir(a.src)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	a.say("left the rest of %s in place (%s); delete it when you like", a.src, strings.Join(names, ", "))
}

// storedMiningDecision reads a decision that came with an installation
// through the one mining authority (invariant 10), without creating anything:
// the state directory is opened only if it already exists.
func storedMiningDecision(stateDir string) auth.MiningDecisionState {
	store, err := auth.OpenStoreExisting(stateDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return auth.MiningUndecided
		}
		return auth.MiningDegraded
	}
	return store.ReadMiningDecision().State
}
