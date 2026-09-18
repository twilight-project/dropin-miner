package main

// Lifecycle coordination: which operations may start while another one is
// changing or destroying an installation.
//
// An installation H is guarded by one gate, H.lifecycle.lock, a sibling of H
// rather than a file inside it, so the operation that removes H never removes
// the lock that coordinates it. The gate holds no participant state and is
// never deleted.
//
// The gate plays two roles. For setup, connect and flush it is an admission
// gate: take it, take the operation's own lock, let it go, then run under the
// operation lock alone. For an operation that destroys or replaces the
// installation it is a whole-operation lock, held from before anything is
// inspected until the work is done, together with every operation lock it
// probed. A new setup, connect or flush therefore cannot begin between the
// destructive operation's check and its work: to begin, it would need the
// gate.
//
// Lock order, everywhere, with no exceptions:
//
//	H.lifecycle.lock → setup.lock → connect.lock → flush.lock
//	H.lifecycle.lock → setup.lock → <resolved binary>.update.lock   (upgrade)
//	H.lifecycle.lock → setup.lock → connect.lock → flush.lock → <resolved binary>.update.lock   (uninstall -binary)
//
// The update lock uninstall -binary takes last is the same identity an
// upgrade holds, <ResolveExecutable(binary)>.update.lock, so the two never
// run on one binary at once.
//
// A process never takes an earlier lock while holding a later one. Setup's
// in-process connect runs under setup's admission and does not take the gate
// again: setup already holds setup.lock, and taking the gate after it would
// be the reverse order.
//
// Who is keyed where. connect and flush key the gate on the directory of the
// config file they are about to load — chosen exactly as loadConfig chooses
// it (-config, then TOKENDROP_CONFIG, then ./tokendrop.toml, then the
// installation's own config when that file exists) — and they know that
// path before loading anything. setup keys it on the installation
// directory it writes, whose config is H/tokendrop.toml; on the installer
// layout the two keys are the same path. Every path that names a gate goes
// through lifecycleIdentity, one lexical canonicalization, so two spellings
// of the same directory do not give two gates. Symlinks are not resolved:
// resolving them in one command and not another would split the gate.
//
// Waiting. A person's command — foreground connect, setup, a manual flush —
// waits at most lifecycleForegroundWait for the gate, then refuses and exits
// non-zero. A detached child (a flush search or a hook started, or connect
// -resume) makes one attempt and, when the gate is held, exits 0 quietly and
// records nothing: the health vocabulary is closed, and an uninstall or
// upgrade window is short. Nothing here is on the search path.
//
// A gate that cannot be opened at all — as opposed to one somebody holds —
// admits an ordinary operation without it: the destructive side opens the
// same path as the same user and refuses on the same error, so an ungated
// ordinary operation can never overlap a destructive one on that gate.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const (
	lifecycleGateSuffix = ".lifecycle.lock"
	setupLockFile       = "setup.lock"
	// detachedChildEnv is set by spawnDetached, the single choke point for
	// every detached child, so a child can tell it was not started by a
	// person without a flag a person could also type.
	detachedChildEnv = "TOKENDROP_DETACHED"
	// lifecycleBusyRetryAfter is the retry hint a machine caller gets.
	lifecycleBusyRetryAfter = 5 * time.Second
)

// lifecycleForegroundWait bounds how long a person's command waits for the
// gate. A variable only so tests can shorten it.
var lifecycleForegroundWait = 5 * time.Second

// lifecyclePollInterval is how often a waiting command retries the gate.
const lifecyclePollInterval = 50 * time.Millisecond

// lifecycleTestHook, when set by a test, is called at each named point of
// admission: "gate attempt" before every attempt on the gate, and
// "operation locked" once an ordinary operation holds its own lock. Nil in
// production.
var lifecycleTestHook func(event string)

func lifecycleEvent(event string) {
	if lifecycleTestHook != nil {
		lifecycleTestHook(event)
	}
}

var errLifecycleBusy = errors.New("another setup, uninstall or upgrade is in progress for this installation")

// lifecycleIdentity is the one canonicalization for every lifecycle path: an
// absolute, cleaned spelling, with symlinks deliberately left unresolved.
func lifecycleIdentity(path string) (string, error) {
	return filepath.Abs(path)
}

// lifecycleGatePath is the gate of the installation or config directory dir,
// which must already be canonical.
func lifecycleGatePath(dir string) string {
	return dir + lifecycleGateSuffix
}

// configGatePath is the gate for an ordinary operation that will load the
// config loadConfig would choose for cfgFlag (describeConfigSource, ruling
// D-R1). With no config file at all — resolution exhausted, including the
// installation's own — it is the default installation's gate: nothing such
// an operation touches is under any installation a destructive command can
// target, and one fixed choice keeps two such runs coordinated with each
// other.
func configGatePath(cfgFlag string, getenv func(string) string) (string, error) {
	if src := describeConfigSource(cfgFlag, getenv); src != "" {
		abs, err := lifecycleIdentity(src)
		if err != nil {
			return "", err
		}
		return lifecycleGatePath(filepath.Dir(abs)), nil
	}
	home := defaultTokendropHome(getenv)
	if home == "" {
		return "", nil
	}
	abs, err := lifecycleIdentity(home)
	if err != nil {
		return "", err
	}
	return lifecycleGatePath(abs), nil
}

// resolveInstallationHome is H for the commands that act on an installation
// as a whole (uninstall, upgrade): -home, then TOKENDROP_HOME, then the
// directory of TOKENDROP_CONFIG when that file is named tokendrop.toml, then
// the native installer layout — the executable's directory is named bin and
// its parent holds tokendrop.toml — then ~/.tokendrop. A config with any
// other name says nothing about which directory is an installation: taking
// its parent would point destructive commands at an arbitrary directory.
func resolveInstallationHome(homeFlag string, getenv func(string) string, userHome, executable string) (string, error) {
	candidate := homeFlag
	if candidate == "" {
		candidate = getenv("TOKENDROP_HOME")
	}
	if candidate == "" {
		if cfg := getenv("TOKENDROP_CONFIG"); cfg != "" {
			abs, err := lifecycleIdentity(cfg)
			if err != nil {
				return "", err
			}
			if filepath.Base(abs) == setupConfigFile {
				candidate = filepath.Dir(abs)
			}
		}
	}
	if candidate == "" && executable != "" {
		binDir := filepath.Dir(executable)
		if filepath.Base(binDir) == "bin" && lexists(filepath.Join(filepath.Dir(binDir), setupConfigFile)) {
			candidate = filepath.Dir(binDir)
		}
	}
	if candidate == "" {
		if userHome == "" {
			return "", errors.New("no installation directory: pass -home")
		}
		candidate = filepath.Join(userHome, ".tokendrop")
	}
	return lifecycleIdentity(candidate)
}

// ── the gate ────────────────────────────────────────────────────────────

// lifecycleLock is one held lock file. release is idempotent and safe on a
// nil lock, so an admission that took no gate can be released like one that
// did.
type lifecycleLock struct {
	path string
	f    *os.File
}

func (l *lifecycleLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = unlockFile(l.f)
	l.f = nil
}

// acquireLifecycleGate takes the gate at path, trying for at most wait. A
// held gate is errLifecycleBusy; any other failure is returned as it is.
func acquireLifecycleGate(path string, wait time.Duration) (*lifecycleLock, error) {
	deadline := time.Now().Add(wait)
	for {
		lifecycleEvent("gate attempt")
		f, held, err := tryLockFile(path)
		if err != nil {
			return nil, err
		}
		if held {
			return &lifecycleLock{path: path, f: f}, nil
		}
		if !time.Now().Before(deadline) {
			return nil, errLifecycleBusy
		}
		time.Sleep(lifecyclePollInterval)
	}
}

// admission is how an ordinary operation passes the gate.
type admission int

const (
	admitForeground      admission = iota // a person's command: wait, then refuse
	admitDetached                         // a detached child: one attempt, quiet exit
	admitAlreadyAdmitted                  // the caller holds admission for this run
)

// detachedChild reports whether this process was started by spawnDetached.
func detachedChild(getenv func(string) string) bool {
	return getenv(detachedChildEnv) == "1"
}

// admitOrdinary passes the gate for setup, connect or flush. The returned
// lock, possibly nil, is released by the caller as soon as it holds its own
// operation lock. errLifecycleBusy is the only error: a gate that cannot be
// opened admits without it (see the file comment).
func admitOrdinary(gatePath string, mode admission) (*lifecycleLock, error) {
	if mode == admitAlreadyAdmitted || gatePath == "" {
		return nil, nil
	}
	wait := time.Duration(0)
	if mode == admitForeground {
		wait = lifecycleForegroundWait
	}
	gate, err := acquireLifecycleGate(gatePath, wait)
	if errors.Is(err, errLifecycleBusy) {
		return nil, err
	}
	if err != nil {
		return nil, nil
	}
	return gate, nil
}

// ── destructive exclusion ───────────────────────────────────────────────

// lifecycleActiveError names the operation found running.
type lifecycleActiveError struct {
	Operation string // "setup", "connect" or "flush"
	Lock      string
}

func (e *lifecycleActiveError) Error() string {
	return fmt.Sprintf("%s is running for this installation (%s is held)", e.Operation, e.Lock)
}

// lifecycleConfigError is a config that exists but cannot be loaded: the
// operation locks it names cannot be located, so nothing can be proven idle.
type lifecycleConfigError struct {
	Path string
	Err  error
}

func (e *lifecycleConfigError) Error() string {
	return fmt.Sprintf("cannot load %s to find which operations are running: %v", e.Path, e.Err)
}

func (e *lifecycleConfigError) Unwrap() error { return e.Err }

// lifecycleExclusion is held by an operation that destroys or replaces an
// installation: the gate, and every operation lock it found idle, in lock
// order.
type lifecycleExclusion struct {
	home string
	gate *lifecycleLock
	ops  []*lifecycleLock

	// created is the operation-lock paths hold() brought into existence,
	// in the order it took them. Taking a lock opens its file with
	// O_CREATE (tryLockFile, minerlock_*.go), so probing an installation
	// that has never flushed creates a flush.lock that was not there
	// before — #86, where an uninstall -purge-state aborted at its
	// confirmation printed "No participant state or integrations were
	// changed" over a new empty flush.lock it had just made.
	created []string

	// removeCreated is whether release() removes those files when the
	// operation did not proceed. excludeLifecycle sets it: a destructive
	// run that stops must leave the installation as it found it.
	// excludeForUpgrade does not, and deliberately — an upgrade's lock
	// files are installation furniture that uninstall -binary already
	// knows how to remove (S19), and the flush lock an upgrade wants
	// present is created on purpose one line after the exclusion is taken
	// (ensureUpgradeFlushLock). #86 is about the destructive-abort path;
	// widening it to upgrade needs its own evidence, not this one's.
	removeCreated bool

	// applied is set by proceeded(), once the operation has gone on to
	// change the installation. The default is the safe direction: an
	// exclusion released without it removes what it made, so a refusal
	// added later cleans up without having to remember to.
	applied bool
}

// proceeded records that the operation went past the point of deciding and
// is changing the installation, so the lock files it took are now part of
// that installation rather than something an abort must undo.
func (ex *lifecycleExclusion) proceeded() {
	if ex != nil {
		ex.applied = true
	}
}

// excludeLifecycle takes the gate of home, then setup.lock, then — located
// from home's config, loaded only once the gate is held — connect.lock and
// flush.lock. Any operation found running, a config that exists but will not
// load, or a gate that cannot be opened is a refusal, returned with nothing
// held. With no config file the installer layout's lock paths are probed.
func excludeLifecycle(home string, getenv func(string) string, wait time.Duration) (*lifecycleExclusion, error) {
	gate, err := acquireLifecycleGate(lifecycleGatePath(home), wait)
	if err != nil {
		return nil, err
	}
	ex := &lifecycleExclusion{home: home, gate: gate, removeCreated: true}
	if err := ex.hold("setup", filepath.Join(home, setupLockFile)); err != nil {
		ex.release()
		return nil, err
	}
	connectLock, flushLock, err := operationLockPaths(home, getenv)
	if err != nil {
		ex.release()
		return nil, err
	}
	for _, op := range []struct{ name, path string }{{"connect", connectLock}, {"flush", flushLock}} {
		if op.path == "" {
			continue
		}
		if err := ex.hold(op.name, op.path); err != nil {
			ex.release()
			return nil, err
		}
	}
	return ex, nil
}

// operationLockPaths locates connect.lock and flush.lock for home. An empty
// path is a lock no operation can take under this config.
func operationLockPaths(home string, getenv func(string) string) (connectLock, flushLock string, err error) {
	cfgPath := filepath.Join(home, setupConfigFile)
	if _, statErr := os.Lstat(cfgPath); errors.Is(statErr, fs.ErrNotExist) {
		return connectLockPath(filepath.Join(home, "state")), filepath.Join(home, "flush.lock"), nil
	} else if statErr != nil {
		return "", "", &lifecycleConfigError{Path: cfgPath, Err: statErr}
	}
	cfg, _, loadErr := loadConfig(cfgPath, getenv)
	if loadErr != nil {
		return "", "", &lifecycleConfigError{Path: cfgPath, Err: loadErr}
	}
	if cfg.Mining.StateDir != "" {
		connectLock = connectLockPath(cfg.Mining.StateDir)
	}
	if cfg.Miner.IntakeDir != "" {
		flushLock = flushLockPath(cfg.Miner)
	}
	return connectLock, flushLock, nil
}

// lockableDir is whether path's directory rules out taking a lock there at
// all: only a directory that clearly does not exist does. Every other stat
// outcome — the directory exists, or some other error (permission denied,
// say) — leaves the attempt to tryLockFile itself, so a predictor of this
// same outcome (uninstall.go's predictedFlushLockPath) must not treat an
// error other than "not exist" as "does not exist" either.
func lockableDir(path string) bool {
	_, err := os.Stat(filepath.Dir(path))
	return !errors.Is(err, fs.ErrNotExist)
}

// hold takes one operation lock and keeps it. A lock whose directory does not
// exist cannot be held by anyone and is skipped.
func (ex *lifecycleExclusion) hold(operation, path string) error {
	if !lockableDir(path) {
		return nil
	}
	// Asked before the open, because the open is what creates it. The two
	// outcomes that are not "held" need no record: an open that errored
	// created nothing, and a lock held elsewhere proves the file was
	// already there, since nobody holds one that does not exist.
	existed := lexists(path)
	f, held, err := tryLockFile(path)
	if err != nil {
		return fmt.Errorf("probe %s: %w", path, err)
	}
	if !held {
		return &lifecycleActiveError{Operation: operation, Lock: path}
	}
	if !existed {
		ex.created = append(ex.created, path)
	}
	ex.ops = append(ex.ops, &lifecycleLock{path: path, f: f})
	return nil
}

// releaseOperation lets go of one operation lock while the gate stays held:
// Windows will not delete a file that is open, so a lock is released
// immediately before its own file is removed. Between the two, a binary that
// predates the gate could take it; one that honors the gate cannot.
func (ex *lifecycleExclusion) releaseOperation(path string) {
	for _, l := range ex.ops {
		if l.path == path {
			l.release()
		}
	}
}

// release lets go of everything, operation locks first, then the gate, and
// removes the lock files this exclusion created when the operation never
// proceeded (#86). Every lock is let go above before any file is removed
// below, because Windows will not delete a file that is open. A lock file
// that already existed is never removed: the participant's own flush lock
// is not this operation's to clean up. Nor is the gate, which is a sibling
// of the installation and is documented as left behind and named.
func (ex *lifecycleExclusion) release() {
	if ex == nil {
		return
	}
	for i := len(ex.ops) - 1; i >= 0; i-- {
		ex.ops[i].release()
	}
	if ex.removeCreated && !ex.applied {
		for i := len(ex.created) - 1; i >= 0; i-- {
			// Best-effort: a purge that got as far as removing the file
			// itself, or a directory already gone, is not a failure to
			// report over a run that has just said nothing was changed.
			_ = os.Remove(ex.created[i])
		}
	}
	ex.gate.release()
}

// detachedEnvironment is a detached child's environment: the parent's, with
// detachedChildEnv set. Later duplicates win in os/exec, so an inherited
// value cannot mask it.
func detachedEnvironment(parent []string) []string {
	return append(append([]string(nil), parent...), detachedChildEnv+"=1")
}
