package main

// upgrade: replace this native binary with a verified canonical release, or
// with the one-level previous binary.
//
// The order is fixed. The launch classifier decides first whether this copy
// is native at all; an npm copy is told npm's own command and nothing else
// happens. One operation context with selfupdate.OperationTimeout is created
// next and carried through everything that follows — the lifecycle
// exclusion, release discovery, preparation and replacement, or rollback —
// and the candidate's own five-second bound is taken inside it. Success is
// printed only after the transaction returned no error, which is after the
// canonical path validated and .previous was committed. Every Install is
// followed by Prepared.DiscardAfterInstall(err), so a copy an error reports
// as surviving is never removed.
//
// Only then, and never as a condition of success, are the host integrations
// this installation owns rendered again by the binary now installed
// (upgrade_rerender.go, #111).
//
// Failures print one class token — retry, release_invalid, ownership,
// filesystem, lifecycle_busy, refused or manual_intervention — chosen from
// typed errors, never from message text, each with its own exit code where
// the shared exit codes allow and always with its own token.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/twilight-project/dropin-miner/internal/selfupdate"
)

const upgradeUsage = `usage: dropin-miner upgrade [-version X.Y.Z | -rollback] [-home dir]

Replaces this native binary with the latest canonical DropinMiner release, or
with -version exactly that release, after verifying its checksum, inspecting
its archive and running the new binary's version command. The binary it
replaces is kept as <binary>.previous. A copy npm installed is updated with npm.

  -version X.Y.Z  upgrade to exactly this release; never an older one
  -rollback       put back <binary>.previous, the one binary the last upgrade or
                  rollback replaced; no network
  -home dir       the installation whose lifecycle lock is held (default as
                  uninstall resolves it)
`

// upgradeClass is what a participant can do about a failed upgrade.
type upgradeClass string

const (
	upgradeRetry          upgradeClass = "retry"
	upgradeReleaseInvalid upgradeClass = "release_invalid"
	upgradeOwnership      upgradeClass = "ownership"
	upgradeFilesystem     upgradeClass = "filesystem"
	upgradeLifecycleBusy  upgradeClass = "lifecycle_busy"
	upgradeRefused        upgradeClass = "refused"
	upgradeManual         upgradeClass = "manual_intervention"
)

// exit is the class's process exit code.
func (c upgradeClass) exit() int {
	switch c {
	case upgradeRetry, upgradeLifecycleBusy:
		return exitTransport
	case upgradeOwnership, upgradeRefused:
		return exitUsage
	case upgradeReleaseInvalid, upgradeFilesystem:
		return exitClientErr
	default:
		return exitOutcomeUnknown
	}
}

// errUpgradeOwnership marks the launch classifier's refusal.
var errUpgradeOwnership = errors.New("not a native copy")

// classifyUpgrade maps a failure to its class from types alone.
func classifyUpgrade(err error) upgradeClass {
	var active *lifecycleActiveError
	switch {
	case errors.Is(err, errUpgradeOwnership):
		return upgradeOwnership
	case errors.Is(err, errLifecycleBusy), errors.As(err, &active):
		return upgradeLifecycleBusy
	}
	switch selfupdate.KindOf(err) {
	case selfupdate.KindUnavailable, selfupdate.KindReplacementFailed, selfupdate.KindPreviousInUse:
		return upgradeRetry
	case selfupdate.KindReleaseInvalid, selfupdate.KindCandidateInvalid, selfupdate.KindUnsupported:
		return upgradeReleaseInvalid
	case selfupdate.KindNotRelease:
		return upgradeOwnership
	case selfupdate.KindDowngrade:
		return upgradeRefused
	case selfupdate.KindStaging, selfupdate.KindNoPrevious, selfupdate.KindPreviousInvalid:
		return upgradeFilesystem
	case selfupdate.KindIncomplete, selfupdate.KindManualIntervention:
		return upgradeManual
	}
	return upgradeFilesystem
}

// upgradeDeps is everything upgrade reaches outside itself.
type upgradeDeps struct {
	stdout, stderr   io.Writer
	getenv           func(string) string
	userHome         string
	executable       func() (string, error)
	build            func() string
	source           func() selfupdate.ReleaseSource
	runner           selfupdate.CommandRunner
	goos, goarch     string
	operationTimeout time.Duration
	// agents and environ serve the re-render that follows a committed
	// replacement: where the host files are, and what the child that renders
	// them runs with.
	agents  agentOps
	environ func() []string
}

func cmdUpgrade(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	home, _ := os.UserHomeDir()
	return upgradeMain(upgradeDeps{
		stdout:           stdout,
		stderr:           stderr,
		getenv:           getenv,
		userHome:         home,
		executable:       os.Executable,
		build:            buildVersion,
		source:           func() selfupdate.ReleaseSource { return selfupdate.NewHTTPSource(nil) },
		runner:           selfupdate.ExecRunner{},
		goos:             runtime.GOOS,
		goarch:           runtime.GOARCH,
		operationTimeout: selfupdate.OperationTimeout,
		agents:           realAgentOps(),
		environ:          upgradeEnviron,
	}, args)
}

func upgradeMain(d upgradeDeps, args []string) int {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	fs.Usage = func() {}
	versionFlag := fs.String("version", "", "upgrade to exactly this release")
	rollback := fs.Bool("rollback", false, "put back the previous binary")
	homeFlag := fs.String("home", "", "the installation directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(d.stdout, upgradeUsage)
			return exitOK
		}
		fmt.Fprint(d.stderr, upgradeUsage)
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(d.stderr, "dropin-miner upgrade: unexpected argument %q\n%s", fs.Arg(0), upgradeUsage)
		return exitUsage
	}
	if *rollback && *versionFlag != "" {
		fmt.Fprintf(d.stderr, "dropin-miner upgrade: -version and -rollback cannot be combined: -rollback restores the one previous binary, whatever its version\n%s", upgradeUsage)
		return exitUsage
	}
	var requested *selfupdate.Version
	if *versionFlag != "" {
		v, err := selfupdate.ParseVersion(*versionFlag)
		if err != nil {
			fmt.Fprintf(d.stderr, "dropin-miner upgrade: -version: %v\n%s", err, upgradeUsage)
			return exitUsage
		}
		requested = &v
	}
	return upgradeRun(d, *homeFlag, requested, *rollback)
}

func upgradeRun(d upgradeDeps, homeFlag string, requested *selfupdate.Version, rollback bool) int {
	fail := func(err error, paths ...string) int {
		class := classifyUpgrade(err)
		fmt.Fprintf(d.stderr, "dropin-miner upgrade: %s: %v\n", class, err)
		var typed *selfupdate.Error
		if errors.As(err, &typed) {
			paths = append(paths, typed.Paths...)
		}
		for _, p := range paths {
			fmt.Fprintf(d.stderr, "  %s\n", p)
		}
		return class.exit()
	}

	exe, err := d.executable()
	if err != nil {
		return fail(fmt.Errorf("cannot determine my own path: %w", err))
	}
	resolved, err := selfupdate.ResolveExecutable(exe)
	if err != nil {
		return fail(&selfupdate.Error{Kind: selfupdate.KindStaging, Err: err})
	}
	if err := checkUpgradeLaunch(resolved, d.getenv("DROPIN_MINER_LAUNCH")); err != nil {
		return fail(fmt.Errorf("%w: %v", errUpgradeOwnership, err))
	}
	current := d.build()
	if v, err := selfupdate.ParseVersion(current); err != nil || current != v.String() {
		// Before any lock file is created or anything is fetched.
		return fail(&selfupdate.Error{Kind: selfupdate.KindNotRelease,
			Err: fmt.Errorf("this binary reports %q, not a release version; install a release before upgrading", current)})
	}
	home, err := resolveInstallationHome(homeFlag, d.getenv, d.userHome, exe)
	if err != nil {
		return fail(err)
	}

	// The one deadline for the whole operation, set before anything waits,
	// fetches or replaces.
	ctx, cancel := context.WithTimeout(context.Background(), d.operationTimeout)
	defer cancel()

	ex, err := excludeForUpgrade(home, resolved, lifecycleForegroundWait)
	if err != nil {
		return fail(err)
	}
	defer ex.release()
	ensureUpgradeFlushLock(home, d.getenv, d.stderr)

	updater := selfupdate.Updater{Runner: d.runner, GOOS: d.goos, GOARCH: d.goarch}
	if rollback {
		rolled, err := updater.Rollback(ctx, resolved, current)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(d.stdout, "rolled back %s from %s to %s\nthe binary it replaced is now %s; run `dropin-miner upgrade -rollback` again to swap back\n",
			rolled.Executable, rolled.From, rolled.To, selfupdate.PreviousPath(rolled.Executable))
		d.rerenderOwned(ctx, exe, resolved, home)
		return exitOK
	}

	updater.Source = d.source()
	prepared, err := updater.Prepare(ctx, resolved, current, requested)
	if err != nil {
		return fail(err)
	}
	if prepared.NoChange {
		fmt.Fprintf(d.stdout, "dropin-miner %s is already the %s release; nothing to do\n", prepared.From, selectedRelease(requested))
		return exitOK
	}
	err = updater.Install(ctx, prepared)
	prepared.DiscardAfterInstall(err)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(d.stdout, "upgraded %s from %s to %s\nthe binary it replaced is kept as %s; `dropin-miner upgrade -rollback` restores it\n",
		prepared.Executable, prepared.From, prepared.To, selfupdate.PreviousPath(prepared.Executable))
	d.rerenderOwned(ctx, exe, resolved, home)
	return exitOK
}

// ensureUpgradeFlushLock creates the installation's flush lock when it is
// absent, so a flush a sandboxed agent starts after the upgrade can take it
// read-only. It runs inside the upgrade's exclusion and never fails the
// upgrade: a lock it cannot create is said, and the first flush outside a
// sandbox creates it.
func ensureUpgradeFlushLock(home string, getenv func(string) string, stderr io.Writer) {
	_, path, err := operationLockPaths(home, getenv)
	if err == nil {
		_, err = ensureFlushLockFile(path)
	}
	if err != nil {
		fmt.Fprintf(stderr, "dropin-miner upgrade: note: the flush lock could not be created (%v); a flush inside an agent's sandbox will not run until one outside it has\n", err)
	}
}

func selectedRelease(requested *selfupdate.Version) string {
	if requested != nil {
		return "requested"
	}
	return "latest"
}
