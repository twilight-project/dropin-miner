package main

// uninstall: the reverse of setup for one installation, and nothing more
// unless asked.
//
// By default it takes out what DropinMiner put outside the participant's
// state: every registered target's skills, hooks and plugins that run this
// installation's binary, the shell-profile block that names this
// installation's config (Windows: the user PATH entry and TOKENDROP_CONFIG,
// reverted against setup's journal, compare-and-revert). The wallet, the
// identity, the stored key, recorded searches and the config all stay, and
// nothing remote is revoked.
//
// -binary also removes the installation's own copy of the binary, only when
// the running executable is that copy and no package manager owns it.
// -purge-state also destroys the participant state under the installation
// directory, only after the participant types the wallet address (or the
// installation path) at a terminal; -yes never answers it. Neither implies
// the other.
//
// Everything is planned first, printed, confirmed, and only then done, in
// one order: integrations, environment, authorization revocation (purge
// only, bounded, best effort), state, binary. A destructive run holds the
// lifecycle exclusion (lifecycle.go) from before it inspects anything until
// it is done, so no setup, connect or flush that honors the gate can start
// underneath it; a default run excludes only setup.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/twilight-project/dropin-miner/internal/selfupdate"
	"github.com/twilight-project/dropin-miner/pkg/auth"
	"github.com/twilight-project/dropin-miner/pkg/config"
	"github.com/twilight-project/dropin-miner/pkg/fsx"
)

const uninstallUsage = `usage: dropin-miner uninstall [-home dir] [-binary] [-purge-state] [-dry-run] [-yes]

Takes out what setup put on this machine for one installation: the coding
agents' skills, hooks and plugins that run its binary, and the shell-profile
block that names its config (on Windows, the user PATH entry and
TOKENDROP_CONFIG setup set). The wallet, registration, stored key, recorded
searches and config stay, and nothing is revoked.

  -home dir      the installation (default $TOKENDROP_HOME, else the directory of a
                 TOKENDROP_CONFIG named tokendrop.toml, else the installation this
                 binary runs from, else ~/.tokendrop)
  -binary        also remove this installation's own copy of the binary; a copy npm
                 installed is npm's to remove
  -purge-state   also destroy this installation's participant state: the wallet,
                 registration, stored key, recorded searches and config. Needs a
                 terminal: you type the wallet address (or the installation path)
                 to confirm, and -yes never answers it
  -dry-run       print what would be removed; change, ask and contact nothing
  -yes           answer the yes/no question, terminal or not; never -purge-state
`

// noParticipantChange is how a refusal reads once the lifecycle exclusion
// may have run: taking a lock can create its empty file, so "nothing" would
// be untrue, but no participant state and no integration has been touched.
const noParticipantChange = "No participant state or integrations were changed."

// purgeRevokeTimeout bounds the one network call a purge makes.
const purgeRevokeTimeout = 8 * time.Second

// uninstallProbeCommand is a binary no installation has: planning a target's
// uninstall for it yields exactly the parts that do not depend on which
// binary they run.
const uninstallProbeCommand = "\x00dropin-miner-uninstall-probe"

// uninstallDeps is everything uninstall reaches outside itself.
type uninstallDeps struct {
	stdin       io.Reader
	stdout      io.Writer
	stderr      io.Writer
	getenv      func(string) string
	userHome    string
	executable  func() (string, error)
	interactive bool
	agents      agentOps
	targets     []installTarget
	userEnv     userEnvironment
	windows     bool
	// revoke revokes the AS token family of m; the decision whether to try
	// is uninstall's own.
	revoke func(ctx context.Context, m config.Mining) error
}

func cmdUninstall(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	return uninstallMain(systemUninstallDeps(stdin, stdout, stderr, getenv), args)
}

func systemUninstallDeps(stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) uninstallDeps {
	home, _ := os.UserHomeDir()
	interactive := false
	if f, ok := stdin.(*os.File); ok {
		interactive = term.IsTerminal(int(f.Fd()))
	}
	return uninstallDeps{
		stdin:       stdin,
		stdout:      stdout,
		stderr:      stderr,
		getenv:      getenv,
		userHome:    home,
		executable:  os.Executable,
		interactive: interactive,
		agents:      realAgentOps(),
		targets:     installTargets,
		userEnv:     systemUserEnvironment(),
		windows:     runtime.GOOS == "windows",
		revoke:      revokeMiningFamily,
	}
}

// revokeMiningFamily is mining disable's revocation and nothing else: the AS
// token family through /oauth/revoke. No revoke-pending marker is written;
// a purge is about to remove the directory it would live in.
func revokeMiningFamily(ctx context.Context, m config.Mining) error {
	oauthClient, _, err := buildMiningClient(ctx, m)
	if err != nil {
		return err
	}
	return oauthClient.Revoke(ctx)
}

type uninstallRun struct {
	d uninstallDeps

	binary, purge, dry, yes bool

	home       string
	cfgPath    string
	exe        string
	candidates []string
	cfg        *config.Config
	ex         *lifecycleExclusion

	// envKept: the user-environment journal must survive this run, because
	// its revert did not finish or it could not be trusted.
	envKept  bool
	failures int
	// residual: binary material -binary could not remove and says where it is.
	residual []string
	// updateLock is the binary's own update lock, held by -binary's exclusion.
	updateLock string
}

func (r *uninstallRun) printf(format string, args ...any) { fmt.Fprintf(r.d.stdout, format, args...) }
func (r *uninstallRun) say(msg string)                    { fmt.Fprintf(r.d.stdout, "\n%s\n", msg) }
func (r *uninstallRun) fail(format string, args ...any) {
	r.failures++
	fmt.Fprintf(r.d.stderr, "dropin-miner uninstall: "+format+"\n", args...)
}

func uninstallMain(d uninstallDeps, args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	fs.Usage = func() {}
	homeFlag := fs.String("home", "", "the installation directory")
	r := &uninstallRun{d: d}
	fs.BoolVar(&r.binary, "binary", false, "also remove this installation's own binary")
	fs.BoolVar(&r.purge, "purge-state", false, "also destroy the participant state")
	fs.BoolVar(&r.dry, "dry-run", false, "print what would be removed, change nothing")
	fs.BoolVar(&r.yes, "yes", false, "answer the yes/no question")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(d.stdout, uninstallUsage)
			return exitOK
		}
		fmt.Fprint(d.stderr, uninstallUsage)
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(d.stderr, "dropin-miner uninstall: unexpected argument %q\n%s", fs.Arg(0), uninstallUsage)
		return exitUsage
	}
	return r.run(*homeFlag)
}

func (r *uninstallRun) run(homeFlag string) int {
	d := r.d
	exe, exeErr := d.executable()
	if exeErr == nil {
		r.exe = exe
	}
	home, err := resolveInstallationHome(homeFlag, d.getenv, d.userHome, r.exe)
	if err != nil {
		fmt.Fprintln(d.stderr, "dropin-miner uninstall:", err)
		return exitUsage
	}
	r.home = home
	r.cfgPath = filepath.Join(home, setupConfigFile)

	// Refusals come before anything is locked, planned or changed.
	if r.purge && !r.dry && !d.interactive {
		fmt.Fprintln(d.stderr, "dropin-miner uninstall: -purge-state needs a terminal: it asks you to type a confirmation, and nothing else can answer it (-yes cannot). Nothing was changed.")
		return exitUsage
	}
	if r.binary {
		if exeErr != nil {
			fmt.Fprintln(d.stderr, "dropin-miner uninstall: cannot determine my own path:", exeErr)
			return exitTransport
		}
		if err := r.checkBinaryOwnership(); err != nil {
			fmt.Fprintf(d.stderr, "dropin-miner uninstall -binary: %v\nNothing was changed.\n", err)
			return exitUsage
		}
	}
	if r.purge {
		if err := checkPurgeTarget(home, d.userHome); err != nil {
			fmt.Fprintf(d.stderr, "dropin-miner uninstall -purge-state: %v\nNothing was changed.\n", err)
			return exitUsage
		}
	}
	if !r.dry && !r.purge && !d.interactive && !r.yes {
		fmt.Fprintln(d.stderr, "dropin-miner uninstall: not an interactive shell; pass -yes to remove what is listed by -dry-run. Nothing was changed.")
		return exitUsage
	}

	// Coordination, then the config: never the other way round.
	if !r.dry {
		ex, err := r.exclude()
		if err != nil {
			r.printExclusionError(err)
			return exitTransport
		}
		defer ex.release()
		r.ex = ex
	}
	if lexists(r.cfgPath) {
		cfg, _, err := loadConfig(r.cfgPath, d.getenv)
		switch {
		case err == nil:
			r.cfg = cfg
		case r.purge:
			fmt.Fprintf(d.stderr, "dropin-miner uninstall: cannot load %s: %v\n%s Fix it, or move it aside and run uninstall again.\n", r.cfgPath, err, noParticipantChange)
			return exitTransport
		}
	}
	if r.purge {
		if err := r.checkPurgeConfig(); err != nil {
			fmt.Fprintf(d.stderr, "dropin-miner uninstall -purge-state: %v\n%s\n", err, noParticipantChange)
			return exitUsage
		}
	}
	r.candidates = r.binaryCandidates()

	r.plan()

	switch {
	case r.dry:
		// The same section the real run closes with, in both modes (#129).
		// A dry run is the form a participant reads before deciding, and
		// "these files will still be here, and here is why" is exactly what
		// a decision is made on; it was only ever printed from closing(),
		// which a dry run does not reach. Computed from the files present
		// now, which is what it says: a dry run takes no exclusion, so it
		// creates none of them and none of them is about to go.
		r.sayLeftoverLocks()
		r.printf("\nDry run: nothing was changed, asked or contacted.\n")
		return exitOK
	case r.purge:
		if code, ok := r.confirmPurge(); !ok {
			return code
		}
	default:
		remove, err := r.ask("Remove what is listed above?")
		if err != nil {
			fmt.Fprintf(d.stderr, "\ndropin-miner uninstall: %s; %s\n", promptAbortedReason, noParticipantChange)
			return exitUsage
		}
		if !remove {
			r.say(noParticipantChange)
			return exitOK
		}
	}

	// Past the decision: the operation lock files this run's exclusion had
	// to create are now part of the installation it is changing, not
	// something an abort has to undo (#86, lifecycle.go's release).
	r.ex.proceeded()

	r.applyIntegrations()
	r.applyEnvironment()
	revocation := ""
	if r.purge {
		revocation = r.revokeAuthorization()
		r.applyPurge()
	}
	if r.binary {
		r.applyBinary()
	}
	if r.purge {
		r.removeHomeIfEmpty()
	}
	r.closing(revocation)
	if r.failures > 0 {
		return exitTransport
	}
	return exitOK
}

// ── refusals ────────────────────────────────────────────────────────────

func (r *uninstallRun) binaryName() string {
	if r.d.windows {
		return "dropin-miner.exe"
	}
	return "dropin-miner"
}

func (r *uninstallRun) ownedBinary() string {
	return filepath.Join(r.home, "bin", r.binaryName())
}

// checkBinaryOwnership admits -binary only for a native executable that is
// this installation's own copy.
func (r *uninstallRun) checkBinaryOwnership() error {
	if kind := classifyLaunch(r.exe, r.d.getenv("DROPIN_MINER_LAUNCH")); kind != launchNative {
		return errors.New(npmRemovalGuidance(kind, "remove"))
	}
	owned := r.ownedBinary()
	info, err := os.Lstat(owned)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("this installation has no binary of its own at %s; %s was not installed there, so remove it the way it was installed", owned, r.exe)
	}
	if !sameFile(r.exe, owned) {
		return fmt.Errorf("%s is not this installation's own binary (%s); remove it the way it was installed", r.exe, owned)
	}
	return nil
}

func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	return err == nil && os.SameFile(ai, bi)
}

// checkPurgeTarget refuses every directory a purge must never be aimed at:
// a filesystem root, the user's home, a symlink, and anything that is not an
// installation.
func checkPurgeTarget(home, userHome string) error {
	if filepath.Dir(home) == home {
		return fmt.Errorf("%s is a filesystem root", home)
	}
	if userHome != "" && (filepath.Clean(userHome) == home || sameFile(userHome, home)) {
		return fmt.Errorf("%s is your home directory, not an installation", home)
	}
	info, err := os.Lstat(home)
	if err != nil {
		return fmt.Errorf("there is no installation at %s: %w", home, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; name the directory it points to with -home", home)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", home)
	}
	if !hasInstallation(home) && !lexists(filepath.Join(home, setupConfigFile)) {
		return fmt.Errorf("%s holds no installation: no wallet key, registration, stored key or tokendrop.toml", home)
	}
	return nil
}

// exclude takes the lifecycle exclusion a destructive run needs, or, for a
// default run, only the gate and setup.lock: removing integrations does not
// disturb a connect or a flush, but it must not cross a setup writing them.
func (r *uninstallRun) exclude() (*lifecycleExclusion, error) {
	if r.purge || r.binary {
		ex, err := excludeLifecycle(r.home, r.d.getenv, lifecycleForegroundWait)
		if err != nil || !r.binary {
			return ex, err
		}
		// -binary also takes the binary's own update lock, the identity an
		// upgrade holds (lock order: gate, setup, connect, flush, then this).
		// A held lock is an upgrade of this binary in progress; refusing now
		// leaves every integration, the environment and every binary file
		// as they are, and the leftovers beside the binary may be that
		// upgrade's own staging.
		resolved, rerr := selfupdate.ResolveExecutable(r.exe)
		if rerr != nil {
			ex.release()
			return nil, rerr
		}
		r.updateLock = resolved + updateLockSuffix
		if err := ex.hold("upgrade", r.updateLock); err != nil {
			ex.release()
			return nil, err
		}
		return ex, nil
	}
	if _, err := os.Stat(filepath.Dir(r.home)); errors.Is(err, fs.ErrNotExist) {
		return &lifecycleExclusion{home: r.home}, nil // nothing can be set up there
	}
	gate, err := acquireLifecycleGate(lifecycleGatePath(r.home), lifecycleForegroundWait)
	if err != nil {
		return nil, err
	}
	// removeCreated for the same reason excludeLifecycle sets it: a
	// default run that is declined at "Remove what is listed above?"
	// prints that nothing was changed, and a setup.lock this probe made
	// would make that false too (#86). The issue reported -purge-state,
	// but nothing about the defect was particular to it.
	ex := &lifecycleExclusion{home: r.home, gate: gate, removeCreated: true}
	if err := ex.hold("setup", filepath.Join(r.home, setupLockFile)); err != nil {
		ex.release()
		return nil, err
	}
	return ex, nil
}

func (r *uninstallRun) printExclusionError(err error) {
	var active *lifecycleActiveError
	var cfgErr *lifecycleConfigError
	switch {
	case errors.Is(err, errLifecycleBusy):
		r.fail("%v (%s). %s Run uninstall again shortly.", err, r.home, noParticipantChange)
	case errors.As(err, &active) && active.Operation == "upgrade":
		r.fail("an upgrade of %s is running (%s is held). %s Let it finish and run uninstall again.", r.exe, active.Lock, noParticipantChange)
	case errors.As(err, &active):
		r.fail("%v. %s Let it finish, or stop it, and run uninstall again.", err, noParticipantChange)
	case errors.As(err, &cfgErr):
		r.fail("%v. %s Fix it, or move it aside and run uninstall again; the installer layout's locks are then checked instead.", err, noParticipantChange)
	default:
		r.fail("cannot coordinate with other DropinMiner commands for %s: %v. %s", r.home, err, noParticipantChange)
	}
}

// ── planning ────────────────────────────────────────────────────────────

// binaryCandidates are the binaries whose integrations this installation
// owns: the one running, and the installation's own copy.
func (r *uninstallRun) binaryCandidates() []string {
	var out []string
	for _, c := range []string{r.exe, r.ownedBinary()} {
		if c == "" {
			continue
		}
		dup := false
		for _, o := range out {
			dup = dup || o == c
		}
		if !dup {
			out = append(out, c)
		}
	}
	return out
}

// plan prints everything the run would do, computed exactly as it will be
// done: the integrations are planned against an overlay that applies each
// step's plan before the next is computed.
func (r *uninstallRun) plan() {
	verb := "Uninstalling"
	if r.dry {
		verb = "Dry run: uninstalling"
	}
	r.say(fmt.Sprintf("%s from %s", verb, r.home))

	r.say("Coding agents")
	overlay := newPlanOverlay(r.d.agents)
	changed, left := r.uninstallTargets(overlay.ops(), func(p *agentPlan) {
		printPlan(&agentPlan{writes: p.writes, removes: p.removes, refused: p.refused}, r.d.agents.home, r.d.stdout)
		_ = commitPlan(overlay.ops(), p, io.Discard, io.Discard)
	})
	if !changed {
		r.printf("  nothing of this installation's is installed\n")
	}
	for _, l := range left {
		r.printf("  %s\n", l)
	}

	if r.d.windows {
		r.say("User environment")
		r.environmentWindows(false)
	} else {
		r.say("Shell profile")
		edits, notes := r.profilePlan()
		for _, e := range edits {
			r.printf("  remove the dropin-miner block from %s\n", tilde(r.d.userHome, e.display))
		}
		if len(edits) == 0 && len(notes) == 0 {
			r.printf("  no dropin-miner block for this installation\n")
		}
		for _, n := range notes {
			r.printf("  %s\n", n)
		}
	}

	if r.purge {
		r.say("Participant state — destroyed, with no way back")
		inside, outside := r.purgeSet()
		for _, p := range inside {
			r.printf("  remove %s\n", p)
		}
		for _, p := range outside {
			r.printf("  left:  %s (configured outside %s; not removed)\n", p, r.home)
		}
		r.printf("  %s\n", r.revocationPlan())
		r.printf("  Close any open coding-agent sessions first: searches and hooks are not paused by\n" +
			"  uninstall, and one that runs afterwards can recreate intake/ or sessions/.\n")
	}
	if r.binary {
		r.say("Binary")
		for _, p := range r.binarySet() {
			if r.d.windows && p == r.ownedBinary() {
				r.printf("  move %s aside: Windows cannot delete a running binary, so it stays under a new name until nothing runs it\n", p)
				continue
			}
			r.printf("  remove %s\n", p)
		}
		r.printf("  remove the binary's update lock once the binary has left %s\n", r.ownedBinary())
	}
}

// uninstallTargets plans, for every registered target of every kind, the
// removal of what runs one of this installation's binaries, and hands each
// plan to apply before planning the next. A target whose skill runs a
// different binary is another installation's: only the parts that name one
// of ours are taken out, and the rest is left and reported.
func (r *uninstallRun) uninstallTargets(ops agentOps, apply func(p *agentPlan)) (changed bool, left []string) {
	paths := ops.paths(r.d.getenv)
	ref := installationRef{bins: r.candidates, cfg: r.cfgPath}
	for _, t := range r.d.targets {
		var agnostic agentPlan
		t.PlanUninstall(ops, paths, binEntry{command: uninstallProbeCommand, cfg: r.cfgPath}, r.d.getenv, &agnostic)
		skip := map[string]bool{}
		hold := func(why string) {
			for _, w := range agnostic.writes {
				skip[w.path] = true
			}
			for _, rm := range agnostic.removes {
				skip[rm.path] = true
			}
			left = append(left, fmt.Sprintf("%s: %s", t.Label(), why))
		}
		// Only a target that HAS something here can have it left: an empty
		// agnostic plan is a host that is simply not installed, and saying
		// "left in place" about a file that does not exist would be a lie in
		// the one place a participant is checking what survived.
		kind, other := attributionOurs, ""
		if !agnostic.empty() {
			kind, other = attributeRemoved(ops, agnostic.removedPaths(), ref, r.d.windows)
		}
		switch kind {
		case attributionForeign:
			hold(leftForeign(other))
		case attributionUnknown:
			// #73: an uninstall that cannot attribute a file leaves it and
			// says so. The case is narrow — every skill and hook command has
			// named its config since v0.2.9, so the only artifacts naming none
			// are the JavaScript adapters from before they carried
			// INSTALL_CONFIG — and claiming those by their binary alone is
			// precisely the opencode-plugin complaint. So the message names
			// the files and gives the participant the instruction that makes
			// the problem go away by itself: one `agents install` stamps them,
			// and the next uninstall can then remove them unaided.
			hold(fmt.Sprintf("left in place; %s names no installation, so this one cannot claim it. "+
				"Running `%s agents install -config %s` once would stamp it, and a later uninstall could then remove it; "+
				"otherwise remove it by hand",
				unattributedPaths(ops, agnostic), displayPath(r.exe), displayPath(r.cfgPath)))
		}
		for _, c := range r.candidates {
			var p agentPlan
			t.PlanUninstall(ops, paths, binEntry{command: c, cfg: r.cfgPath}, r.d.getenv, &p)
			p = planWithout(p, skip)
			if p.empty() && len(p.refused) == 0 {
				continue
			}
			changed = true
			apply(&p)
		}
	}
	return changed, left
}

func planWithout(p agentPlan, skip map[string]bool) agentPlan {
	out := agentPlan{refused: p.refused, notes: p.notes}
	for _, w := range p.writes {
		if !skip[w.path] {
			out.writes = append(out.writes, w)
		}
	}
	for _, rm := range p.removes {
		if !skip[rm.path] {
			out.removes = append(out.removes, rm)
		}
	}
	return out
}

// A rendered word, as any of this client's renderers may have written it: a
// double-quoted one (cmd's literal, and v0.2.9's %q), or a single-quoted one
// (POSIX and PowerShell). Both regexes below capture the WHOLE word, quotes
// included, because reading it back is unquoteRenderedPath's job and there
// must be exactly one function that does it.
const renderedWordRe = `"(?:[^"\\\n]|\\.)*"|'[^'\n]*(?:(?:'\\''|'')[^'\n]*)*'`

// installedCommand finds the binaries a rendered skill runs: the quoted path
// before " search", " hook" or " agents prefer".
//
// Several spellings, because several renderers have written this file. v0.2.9
// quoted every path with Go's %q; from H2 a skill is rendered for its host's
// shell, which single-quotes the path (POSIX and PowerShell) or double-quotes
// it literally (cmd). Matching only some of them would make a file written by
// a current install look like one that names no binary at all.
var installedCommand = regexp.MustCompile(`(` + renderedWordRe + `) (?:search|hook|agents prefer)\b`)

// installedConfig finds the installation a rendered artifact declares: the
// `-config <path>` of a command it teaches, or the INSTALL_CONFIG line a
// JavaScript adapter carries.
//
// The adapter line exists because opencode's plugin names no binary and no
// command at all — it rewrites commands, it does not run any — so until it
// carried one, the attribution had nothing to match and read it as unowned.
// A disposable installation's purge therefore removed the main installation's
// plugin, which is #73's own last comment.
// The last alternative is a BARE path: Hermes' splitter takes one, and
// hermesQuoteArg deliberately leaves an ordinary POSIX path unquoted because
// the same string is the snippet a participant is asked to paste by hand.
var installedConfig = regexp.MustCompile(`(?:-config\s+|INSTALL_CONFIG\s*=\s*)(` + renderedWordRe + `|[^\s"'\n]+)`)

// leftForeign is the sentence for an integration that is another
// installation's. One function, because every command that finds one says
// the same thing about it: uninstall's plan, and an upgrade's re-render.
func leftForeign(other string) string {
	return "left in place; " + belongsTo(other)
}

// belongsTo is that sentence without the removal verb, for the command that
// is not removing anything: `agents status`, which reports what is on this
// machine and had been calling another installation's skill "installed".
// The words after the semicolon are the same words in both places on
// purpose -- a participant reading status and then uninstall should be told
// the same thing about the same file -- and "left in place" is dropped here
// because status leaves everything in place and the phrase would be noise.
func belongsTo(other string) string {
	return "it belongs to " + other + ", not this installation"
}

// attribution is what the files a target would remove say about who they
// belong to.
type attribution int

const (
	attributionOurs    attribution = iota // named this installation
	attributionForeign                    // named another installation
	attributionUnknown                    // named none, or none we can read
)

// attributeRemoved decides, from the bytes on disk, whether the files a
// target would remove are this installation's.
//
// Both halves must agree, which is the whole of #73: two installations that
// share a binary are told apart only by the config each names, and matching
// on the binary alone made every uninstall plan the removal of both. A file
// that names an installation other than this one is foreign; a file that
// names none — a v0.2.9 artifact, or an adapter written before this version —
// is UNKNOWN, and unknown is left alone and reported rather than assumed to
// be ours.
func attributeRemoved(ops agentOps, removes []string, ref installationRef, windows bool) (attribution, string) {
	other := ""
	sawAny := false
	for _, path := range removes {
		for _, content := range readRemoved(ops, path) {
			bins, cfgs := namedInArtifact(content)
			if len(bins) == 0 && len(cfgs) == 0 {
				continue
			}
			sawAny = true
			if binsInclude(bins, ref.bins, windows) && configsInclude(cfgs, ref.cfg) {
				return attributionOurs, ""
			}
			if other == "" {
				other = describeOther(bins, cfgs, ref)
			}
		}
	}
	switch {
	case other != "":
		return attributionForeign, other
	case sawAny:
		// Every artifact named something, and none of it named us.
		return attributionForeign, "another installation"
	}
	return attributionUnknown, ""
}

// namedInArtifact is the one place that decides HOW an installed artifact is
// read: a JSON file is decoded and its strings are read as the rendered
// commands they are; anything else — a skill, a JavaScript adapter, Hermes'
// YAML — is scanned as the rendered text it is.
//
// The distinction belongs in one function because leaving it implicit has now
// cost three defects of one shape. A hook file is JSON, so a command inside it
// is escaped twice: the shell quoting first, then JSON's. On POSIX nothing in
// a path needs escaping and a byte scan reads correctly by luck; on Windows a
// cmd-rendered path is `"C:\Users\…"` and the file holds
// `\"C:\\Users\\…\"`, which no reader of rendered text can make sense of.
// Every one of those three read correctly on POSIX and wrongly on Windows,
// and every one was found by the Windows runners rather than by us.
//
// Nothing here has to know which artifact it is looking at: a file that
// decodes as a JSON object is decoded, and one that does not is scanned.
func namedInArtifact(content string) (bins, cfgs []string) {
	m, err := decodeJSONObject([]byte(content))
	if err != nil {
		return namedBinaries(content), namedConfigs(content)
	}
	for _, s := range jsonStringLeaves(m) {
		bins = append(bins, namedBinaries(s)...)
		cfgs = append(cfgs, namedConfigs(s)...)
	}
	return bins, cfgs
}

// jsonStringLeaves is every string in a decoded JSON value. A hook entry's
// command may sit at any depth — Claude Code nests its under a matcher group,
// Cursor's is a top-level field — and which is which is the host's business,
// not this reader's.
func jsonStringLeaves(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, e := range t {
			out = append(out, jsonStringLeaves(e)...)
		}
		return out
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys) // stable order, so a report names the same thing twice running
		var out []string
		for _, k := range keys {
			out = append(out, jsonStringLeaves(t[k])...)
		}
		return out
	}
	return nil
}

// namedBinaries and namedConfigs both read a rendered path back through
// unquoteRenderedPath, and that is the point: there is one function in this
// codebase that reads a rendered path, so the two halves of an attribution
// cannot disagree about what a Windows path says.
//
// They used to share a different helper, which read a double-quoted word only
// through strconv.Unquote. A cmd-rendered Windows path — "C:\Users\…", a
// literal, which is what Cursor's hooks.json holds on Windows — fails that on
// \U, and the helper answered "no path here". The config half went red on the
// Windows runners; the binary half did not, because an artifact naming no
// binary is treated as contradicting nothing, so it failed OPEN and quietly
// widened what uninstall would claim. One reading, not two.
// unattributedPaths names the files that could not be attributed, tilde-
// shortened, so the message points at something the participant can look at
// rather than at a host label. Bounded: a plan that would remove a directory
// names the directory, not every file in it.
func unattributedPaths(ops agentOps, p agentPlan) string {
	seen := map[string]bool{}
	var out []string
	for _, rm := range p.removes {
		display := tilde(ops.home, rm.path)
		if seen[display] {
			continue
		}
		seen[display] = true
		out = append(out, display)
	}
	switch len(out) {
	case 0:
		return "what is installed"
	case 1:
		return out[0]
	}
	return strings.Join(out[:len(out)-1], ", ") + " and " + out[len(out)-1]
}

func namedBinaries(content string) []string {
	var out []string
	for _, m := range installedCommand.FindAllStringSubmatch(content, -1) {
		out = append(out, unquoteRenderedPath(m[1])...)
	}
	return out
}

func namedConfigs(content string) []string {
	var out []string
	for _, m := range installedConfig.FindAllStringSubmatch(content, -1) {
		out = append(out, unquoteRenderedPath(m[1])...)
	}
	return out
}

func binsInclude(named, candidates []string, windows bool) bool {
	if len(named) == 0 {
		return true // an artifact that names no binary cannot contradict one
	}
	for _, bin := range named {
		for _, c := range candidates {
			if bin == c || (windows && strings.EqualFold(bin, c)) || sameFile(bin, c) {
				return true
			}
		}
	}
	return false
}

func configsInclude(named []string, cfg string) bool {
	if len(named) == 0 {
		return true // as above: silence is not a contradiction
	}
	for _, c := range named {
		if samePath(c, cfg) {
			return true
		}
	}
	return false
}

// describeOther names the other installation the way the profile block's
// refusal does: by what the artifact actually says, so the participant can
// see which one it is.
//
// It shows the DECODED reading of the word it names, which is the LAST one
// unquoteRenderedPath returns: a double-quoted word yields the literal
// reading first -- what cmd would run, and what a %q- or JSON-quoted Windows
// path spells with every separator doubled -- and the escaped reading after
// it. Every reading of one word names the same file and matching reads them
// all (samePath), so nothing about attribution changed when this printed the
// first one; only the sentence was wrong, and only on Windows, where
// opencode's INSTALL_CONFIG line is JSON-quoted and came out as
// C:\\Users\\... on both runners (#123, and #112's own message before it).
// Taking the last reading of the last word that is not ours is decoded
// whichever word it comes from, because a word's readings are contiguous and
// its decoded one is last.
//
// filepath.Clean is the second step and not the fix: it would collapse those
// doubled separators on Windows and do nothing at all on any other OS, where
// the same wrong reading would still be printed. It is here to tidy a path a
// participant wrote, not to undo a quoting this function should not have been
// reading in the first place.
func describeOther(bins, cfgs []string, ref installationRef) string {
	if c := lastNotOurs(cfgs, ref.cfg); c != "" {
		return "the installation configured by " + c
	}
	if len(bins) > 0 {
		return displayNamedPath(bins[len(bins)-1])
	}
	return "another installation"
}

// lastNotOurs is the last reading in named that does not name cfg, ready to
// show, or "" when every reading is ours.
func lastNotOurs(named []string, cfg string) string {
	for i := len(named) - 1; i >= 0; i-- {
		if !samePath(named[i], cfg) {
			return displayNamedPath(named[i])
		}
	}
	return ""
}

// displayNamedPath is a path read out of an artifact, in the spelling to show
// a person.
func displayNamedPath(p string) string {
	if p == "" {
		return p
	}
	return filepath.Clean(p)
}

// readRemoved is the text of a file, or of the regular files directly in a
// directory, that a plan would remove; each bounded, anything else skipped.
func readRemoved(ops agentOps, path string) []string {
	const maxRead = 1 << 20
	info, err := ops.stat(path)
	if err != nil {
		return nil
	}
	files := []string{path}
	if info.IsDir() {
		files = nil
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil
		}
		for _, e := range entries {
			if e.Type().IsRegular() {
				files = append(files, filepath.Join(path, e.Name()))
			}
		}
	}
	var out []string
	for _, f := range files {
		if fi, err := ops.stat(f); err != nil || fi.Size() > maxRead {
			continue
		}
		if b, err := ops.readFile(f); err == nil {
			out = append(out, string(b))
		}
	}
	return out
}

// profileEdit is one profile losing its dropin-miner block.
type profileEdit struct {
	display string
	target  string
	next    []byte
	mode    fs.FileMode
}

// profilePlan looks only where setup can have written — ~/.zshrc and
// ~/.bashrc, whichever shell is current now — and removes a block only when
// it is one well-formed block exporting this installation's config.
func (r *uninstallRun) profilePlan() (edits []profileEdit, notes []string) {
	if r.d.userHome == "" {
		return nil, nil
	}
	seen := map[string]bool{}
	for _, name := range []string{".zshrc", ".bashrc"} {
		candidate := filepath.Join(r.d.userHome, name)
		if !lexists(candidate) {
			continue
		}
		target, existing, mode, err := profileTarget(candidate)
		if err != nil {
			notes = append(notes, fmt.Sprintf("not touching %s: %v", tilde(r.d.userHome, candidate), err))
			continue
		}
		if seen[target] {
			continue
		}
		seen[target] = true
		next, block, found, err := removeProfileBlock(existing)
		if errors.Is(err, errProfileMalformed) {
			notes = append(notes, fmt.Sprintf("not touching %s: %v. Remove the dropin-miner lines between %q and %q by hand",
				tilde(r.d.userHome, candidate), err, profileMarkerStart, profileMarkerEnd))
			continue
		}
		if !found {
			continue
		}
		if !profileBlockNamesConfig(block, r.cfgPath) {
			value, _ := profileBlockConfig(block)
			notes = append(notes, fmt.Sprintf("left the dropin-miner block in %s: it sets TOKENDROP_CONFIG=%s, not this installation's %s",
				tilde(r.d.userHome, candidate), value, r.cfgPath))
			continue
		}
		edits = append(edits, profileEdit{display: candidate, target: target, next: next, mode: mode})
	}
	return edits, notes
}

// environmentWindows plans (apply false) or performs (apply true) the
// compare-and-revert of the User environment against setup-env.json.
func (r *uninstallRun) environmentWindows(apply bool) {
	journalPath := filepath.Join(r.home, setupEnvJournalFile)
	binDir := filepath.Join(r.home, "bin")
	manual := func(why string) {
		r.printf("  %s; nothing in your user environment is changed.\n", why)
		r.printf("  If it still holds them: remove %s from your user Path, and TOKENDROP_CONFIG if it is %s\n", binDir, r.cfgPath)
	}
	env := r.d.userEnv
	if env == nil {
		manual("no user environment is reachable")
		return
	}
	j, err := readEnvJournal(journalPath)
	if err != nil {
		r.envKept = true
		manual(fmt.Sprintf("setup's record of what it changed cannot be trusted (%v)", err))
		return
	}
	if j == nil {
		pathValue, _, perr := env.Get("Path")
		cfgValue, _, cerr := env.Get("TOKENDROP_CONFIG")
		if perr == nil && cerr == nil && (pathHasEntry(pathValue, binDir) || strings.EqualFold(cfgValue, r.cfgPath)) {
			manual(fmt.Sprintf("%s is missing, so nothing records what setup changed and nothing is guessed", journalPath))
		} else if !apply {
			r.printf("  nothing of this installation's in your user environment\n")
		}
		return
	}
	plan, err := planUserEnvironmentRevert(env, *j)
	if err != nil {
		r.envKept = true
		if apply {
			r.fail("user environment: %v; %s is kept, run uninstall again", err, journalPath)
		} else {
			r.printf("  cannot read your user environment: %v\n", err)
		}
		return
	}
	if !apply {
		if plan.removePath {
			r.printf("  remove %s from your user Path\n", j.Path.Entry)
		}
		switch plan.config {
		case configRestore:
			r.printf("  put TOKENDROP_CONFIG back to %s, its value before setup\n", plan.restore)
		case configDelete:
			r.printf("  remove TOKENDROP_CONFIG, which setup created\n")
		case configCeded:
			r.printf("  leave TOKENDROP_CONFIG: it was changed after setup, so it is yours now\n")
		}
		r.printf("  remove %s\n", journalPath)
		return
	}
	if err := applyUserEnvironmentRevert(env, journalPath, plan); err != nil {
		r.envKept = true
		r.fail("user environment: %v; %s is kept, so uninstall can finish this later — run it again", err, journalPath)
		return
	}
	r.printf("reverted what setup changed in your user environment\n")
	if plan.config == configCeded {
		r.printf("left TOKENDROP_CONFIG as it is: it was changed after setup\n")
	}
}

// purgeSet is every participant-state entry under home a purge removes, in
// the order it removes them — lock-bearing entries last, so the operation
// locks are held as long as possible — and every configured state path
// outside home, which is left alone.
func (r *uninstallRun) purgeSet() (inside, outside []string) {
	join := func(name string) string { return filepath.Join(r.home, name) }
	first := []string{
		join("wallet"), join("sessions"), join("intake"), join("spool"),
		join(credentialsFile), join(setupConfigFile), join(preferFile), join("flush.json"),
	}
	if entries, err := os.ReadDir(r.home); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "state.unenrolled-") || strings.HasPrefix(e.Name(), "wallet.incomplete-") {
				first = append(first, join(e.Name()))
			}
		}
	}
	if !r.envKept {
		first = append(first, join(setupEnvJournalFile))
	}
	last := []string{join("state"), join("flush.lock"), join(setupLockFile)}

	if r.cfg != nil {
		journal := join(setupEnvJournalFile)
		for _, c := range configuredStatePaths(r.cfg) {
			p := c.path
			// checkPurgeConfig refused every conflict before confirmation;
			// skipping one here too keeps this set safe on its own. The
			// protected set wins over the config: a journal kept because its
			// revert failed cannot come back in through a configured path.
			if r.purgeConflict(p) != "" || (r.envKept && pathsOverlap(p, journal)) {
				continue
			}
			if !pathWithin(p, r.home) {
				if lexists(p) && !containsString(outside, p) {
					outside = append(outside, p)
				}
				continue
			}
			covered := false
			for _, q := range append(append([]string{}, first...), last...) {
				covered = covered || pathWithin(p, q)
			}
			if !covered {
				last = append(last, p)
			}
		}
	}
	// The prediction is -purge-state's alone (#134). Only its exclusion
	// takes the flush lock, so only there is an absent one about to exist
	// and about to be removed; a default run holds the gate and setup.lock
	// and nothing else, and prints this set as what REMAINS, where a file
	// that is not there must not be named — the leftover-locks summary in
	// the same output already applies that test.
	flushLock := ""
	if r.purge {
		flushLock = r.predictedFlushLockPath()
	}
	for _, p := range append(first, last...) {
		if lexists(p) || (p == flushLock && flushLock != "" && lockableDir(flushLock)) {
			inside = append(inside, p)
		}
	}
	sort.Strings(outside)
	return inside, outside
}

// predictedFlushLockPath is the flush lock a real -purge-state run's own
// lifecycle exclusion (excludeLifecycle, lifecycle.go) tries to hold before
// this plan is ever computed, resolved the same way excludeLifecycle
// itself resolves it: through operationLockPaths, not a hand-rolled
// re-reading of r.cfg. That matters beyond staying in sync with a single
// source of truth — a config that loads but names no miner.intake_dir (no
// [miner] block at all, or a bare proxy config) makes operationLockPaths
// return "" for the flush lock, meaning the exclusion takes no lock at
// all; treating a nil r.cfg and an r.cfg with no intake dir the same way,
// as an earlier version of this function did, wrongly predicted
// home/flush.lock for the latter too. Holding the resolved lock opens the
// file with O_CREATE (tryLockFile, minerlock_*.go), so an absent lock the
// exclusion can still take is created as a side effect of a real run, not
// left for purgeSet to find. The exclusion is skipped on a dry run (its
// own O_CREATE would itself be an undisclosed write), so this predicts the
// same outcome instead of reading a file that a dry run never gave the
// chance to appear. "" means operationLockPaths could not resolve one at
// all (a config that exists but fails to load) — a purge refuses before
// ever reaching this on that ground (see the case r.purge branch in run()),
// so there is no real run's outcome left to predict.
func (r *uninstallRun) predictedFlushLockPath() string {
	_, flushLock, err := operationLockPaths(r.home, r.d.getenv)
	if err != nil {
		return ""
	}
	return flushLock
}

// configuredPath is one participant path the config names or derives.
type configuredPath struct {
	name string
	path string
}

// configuredStatePaths is every participant path a config names — state,
// spool, intake, sessions — and the ones derived from them: the flush stamp
// in the state directory, and beside the intake the stored credentials, the
// earlier flush stamp and the flush lock.
func configuredStatePaths(cfg *config.Config) []configuredPath {
	m, mn := cfg.Mining, cfg.Miner
	out := []configuredPath{
		{"mining.state_dir", m.StateDir}, {"mining.spool_dir", m.SpoolDir},
		{"miner.intake_dir", mn.IntakeDir}, {"miner.sessions_dir", mn.SessionsDir},
		{"the flush stamp in mining.state_dir", flushStampPath(m)},
	}
	if mn.IntakeDir != "" {
		out = append(out,
			configuredPath{"the stored credentials beside miner.intake_dir", filepath.Join(minerRoot(mn), credentialsFile)},
			configuredPath{"the earlier flush stamp beside miner.intake_dir", legacyFlushStampPath(mn)},
			configuredPath{"the flush lock beside miner.intake_dir", flushLockPath(mn)})
	}
	var named []configuredPath
	for _, c := range out {
		if c.path != "" {
			named = append(named, c)
		}
	}
	return named
}

// purgeConflict says why a configured participant path cannot be purged as
// state without destroying what is not state — the installation directory
// itself, the software under bin/ or src/, or the lifecycle gate — and ""
// when it can. Only a path strictly below the installation and clear of
// those roots is purgeable.
func (r *uninstallRun) purgeConflict(p string) string {
	abs, err := lifecycleIdentity(p)
	if err != nil {
		return "not a path uninstall can resolve"
	}
	bin, src, gate := filepath.Join(r.home, "bin"), filepath.Join(r.home, "src"), lifecycleGatePath(r.home)
	switch {
	case pathWithin(abs, r.home) && pathWithin(r.home, abs):
		return "the installation directory itself"
	case pathsOverlap(abs, bin):
		return "inside or around the binary directory " + bin
	case pathsOverlap(abs, src):
		return "inside or around the installer's source directory " + src
	case pathsOverlap(abs, gate):
		return "inside or around the lifecycle lock " + gate
	}
	return ""
}

// checkPurgeConfig refuses a purge whose config makes state and software
// inseparable, before anything is planned, asked or changed.
func (r *uninstallRun) checkPurgeConfig() error {
	if r.cfg == nil {
		return nil
	}
	for _, c := range configuredStatePaths(r.cfg) {
		if why := r.purgeConflict(c.path); why != "" {
			return fmt.Errorf("%s is %s, which is %s: state and program ownership cannot be separated for that path. Move it, or fix it in %s, then run uninstall again",
				c.name, c.path, why, r.cfgPath)
		}
	}
	return nil
}

func pathsOverlap(a, b string) bool { return pathWithin(a, b) || pathWithin(b, a) }

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// pathWithin reports whether path is dir or inside it, lexically.
func pathWithin(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// binarySet is what -binary removes, in order: the self-updater's own staging
// leftovers beside the binary, the one-level .previous, the executable's
// update lock, the setup script the installer put beside an old binary, the
// installer's source checkout, and last the binary itself. Nothing else in
// the directory is touched.
func (r *uninstallRun) binarySet() []string {
	owned := r.ownedBinary()
	leftovers, _ := selfupdate.StagingLeftovers(filepath.Dir(owned))
	var out []string
	for _, p := range append(leftovers,
		selfupdate.PreviousPath(owned),
		filepath.Join(r.home, "bin", "dropin-miner-setup.sh"),
		filepath.Join(r.home, "src"),
		owned) {
		if lexists(p) {
			out = append(out, p)
		}
	}
	return out
}

// revocationPlan says, before anything is asked, what a purge will do about
// the AS authorization.
func (r *uninstallRun) revocationPlan() string {
	switch reason, try := r.revocationDecision(); {
	case try:
		return fmt.Sprintf("authorization: revoke this installation's AS token family first (best effort, at most %s)", purgeRevokeTimeout)
	default:
		return "authorization: " + reason
	}
}

// revocationDecision is whether a purge attempts revocation, and why not.
func (r *uninstallRun) revocationDecision() (reason string, try bool) {
	if r.cfg == nil {
		return "no config, so no authorization to revoke", false
	}
	m := r.cfg.Mining
	if m.StateDir == "" || !pathWithin(m.StateDir, r.home) {
		return fmt.Sprintf("the state directory %s is outside %s: left alone, and not revoked", m.StateDir, r.home), false
	}
	store, err := auth.OpenStoreExisting(m.StateDir)
	if err != nil {
		return "no stored authorization to revoke", false
	}
	if _, has, err := store.LoadRefreshToken(); err != nil || !has {
		return "no stored authorization to revoke", false
	}
	if !miningASConfigured(m) {
		return "no authorization server configured, so the stored authorization cannot be revoked", false
	}
	return "", true
}

// ── confirmation ────────────────────────────────────────────────────────

// ask is uninstall's [Y/n], answered yes by -yes. Its error is
// errPromptAborted and nothing else: a read that ended without a line is
// not the "no" this used to return. Both answers leave the installation
// alone here, so the difference a participant sees is the exit code and
// the sentence — an operation that was never answered did not decline,
// and a script must be able to tell those apart (prompt.go).
func (r *uninstallRun) ask(question string) (bool, error) {
	if r.yes {
		r.printf("\n%s [Y/n]: yes (-yes)\n", question)
		return true, nil
	}
	line, err := promptSetup(r.d.stdout, "\n"+question+" [Y/n]: ", r.d.stdin)
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true, nil
	}
	return false, nil
}

// purgeConfirmation is what the participant must type: the wallet's address
// when a valid one can be read from its public sidecar, else the
// installation path.
func purgeConfirmation(home string) (token, what string) {
	var sc sidecar
	if err := readWalletFile(filepath.Join(home, "wallet"), walletSidecarFile, &sc); err == nil {
		if hrp, _, err := auth.DecodeBech32Address(sc.Address); err == nil && hrp == auth.TwilightHRP {
			return sc.Address, "the wallet address"
		}
	}
	return home, "the installation path"
}

// confirmPurge asks for the typed confirmation. -yes has no part in it, and
// nothing has been changed or sent when it refuses.
func (r *uninstallRun) confirmPurge() (code int, ok bool) {
	token, what := purgeConfirmation(r.home)
	line, err := promptSetup(r.d.stdout, fmt.Sprintf(
		"\nThis destroys the participant state listed above. If the wallet holds funds and you\n"+
			"have not kept its 24 words, they are lost. This confirmation guards against accidents;\n"+
			"it cannot tell a person from a program typing at this terminal.\n"+
			"Type %s to permanently remove this participant state:\n  %s\n> ", what, token), r.d.stdin)
	if err != nil {
		// Already the rule prompt.go now states, and the reason it is
		// stated as one: this prompt was the only one in the binary that
		// had it. It stays here so a reader sees the same shape in all
		// of them, and so the guard below cannot be lost by accident.
		r.say("No confirmation was typed. " + noParticipantChange)
		return exitUsage, false
	}
	if strings.TrimRight(line, "\r") != token {
		r.say("That does not match. " + noParticipantChange)
		return exitUsage, false
	}
	return exitOK, true
}

// ── applying ────────────────────────────────────────────────────────────

func (r *uninstallRun) applyIntegrations() {
	ops := r.d.agents
	r.uninstallTargets(ops, func(p *agentPlan) {
		for _, ref := range p.refused {
			r.fail("%s", ref)
		}
		if failures := commitPlan(ops, p, r.d.stdout, r.d.stderr); failures > 0 {
			r.failures += failures
		}
	})
}

func (r *uninstallRun) applyEnvironment() {
	if r.d.windows {
		r.environmentWindows(true)
		return
	}
	edits, notes := r.profilePlan()
	for _, e := range edits {
		if err := fsx.WriteFileAtomic(filepath.Dir(e.target), filepath.Base(e.target), e.next, e.mode); err != nil {
			r.fail("could not remove the dropin-miner block from %s: %v", e.display, err)
			continue
		}
		r.printf("removed the dropin-miner block from %s\n", tilde(r.d.userHome, e.display))
	}
	for _, n := range notes {
		r.printf("%s\n", n)
	}
}

// otherInstallationHint completes the restore hint for a home that is not
// this machine's default installation. setup -home leaves the shell profile
// and the coding agents alone for such a home (#84) — they belong to the
// default one — so "run setup -home" on its own would promise a restore it
// no longer performs. It says what does, both ways: the agents command for an
// installation that really is a separate one, and TOKENDROP_HOME for one that
// is this machine's own, kept somewhere else.
func (r *uninstallRun) otherInstallationHint() string {
	def, other := otherInstallation(r.home, r.home, r.d.getenv("TOKENDROP_HOME"), r.d.userHome)
	if !other {
		return ""
	}
	return fmt.Sprintf("%s is not this machine's default installation (%s), so that leaves the shell profile and\n"+
		"the coding agents alone; configure agents for it with: dropin-miner agents install -config %s\n"+
		"If it IS this machine's installation, kept somewhere else, run setup with TOKENDROP_HOME set to it\n"+
		"instead of -home, which sets up the profile and the agents too.\n", r.home, def, r.cfgPath)
}

// revokeAuthorization attempts the one bounded revocation and returns the
// sentence the closing message carries about it.
func (r *uninstallRun) revokeAuthorization() string {
	reason, try := r.revocationDecision()
	if !try {
		return "Authorization: " + reason + "."
	}
	ctx, cancel := context.WithTimeout(context.Background(), purgeRevokeTimeout)
	defer cancel()
	if err := r.d.revoke(ctx, r.cfg.Mining); err != nil {
		return fmt.Sprintf("Authorization: local state was removed, but this installation's MINIS/AS authorization could not be confirmed revoked (%v).", err)
	}
	return "Authorization: this installation's AS token family was revoked."
}

func (r *uninstallRun) applyPurge() {
	inside, _ := r.purgeSet()
	for _, p := range inside {
		// Windows will not delete an open file: an operation lock inside p is
		// released immediately before, with the gate still held.
		if r.ex != nil {
			for _, l := range r.ex.ops {
				if pathWithin(l.path, p) {
					r.ex.releaseOperation(l.path)
				}
			}
		}
		if err := os.RemoveAll(p); err != nil {
			r.fail("remove %s: %v", p, err)
			continue
		}
		r.printf("removed %s\n", p)
	}
}

func (r *uninstallRun) applyBinary() {
	owned := r.ownedBinary()
	for _, p := range r.binarySet() {
		switch {
		case p == owned && r.d.windows:
			// The probe's result: a running image can be renamed, not
			// deleted. It is moved out of its name and reported, never
			// claimed as removed.
			aside, err := selfupdate.MoveAside(owned)
			if err != nil {
				r.fail("move %s aside: %v", owned, err)
				continue
			}
			r.residual = append(r.residual, aside)
			r.printf("moved %s aside to %s: Windows cannot delete a running binary; delete that file once no DropinMiner or agent process is running it\n", owned, aside)
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			r.fail("remove %s: %v; delete it once nothing is using it", p, err)
			r.residual = append(r.residual, p)
			continue
		}
		r.printf("removed %s\n", p)
	}
	// The update lock was held across all of that. It goes only once the
	// canonical binary has left its path; while the binary is still there,
	// the lock file stays for the next upgrade or uninstall to take.
	if r.updateLock != "" && r.ex != nil {
		if lexists(owned) {
			r.printf("left %s: the binary is still at %s\n", r.updateLock, owned)
		} else {
			r.ex.releaseOperation(r.updateLock)
			if err := os.Remove(r.updateLock); err == nil {
				r.printf("removed %s\n", r.updateLock)
			}
		}
	}
	bin := filepath.Join(r.home, "bin")
	if entries, err := os.ReadDir(bin); err == nil && len(entries) == 0 {
		if err := os.Remove(bin); err == nil {
			r.printf("removed %s\n", bin)
		}
	}
}

func (r *uninstallRun) removeHomeIfEmpty() {
	entries, err := os.ReadDir(r.home)
	if err != nil || len(entries) > 0 {
		return
	}
	if err := os.Remove(r.home); err == nil {
		r.printf("removed %s\n", r.home)
	}
}

func (r *uninstallRun) closing(revocation string) {
	rule := strings.Repeat("─", 78)
	r.printf("%s\n", rule)
	if r.failures > 0 {
		r.printf("Uninstall finished with %d problem(s), each named above; run it again once they are fixed.\n", r.failures)
	} else {
		r.printf("Uninstall complete.\n")
	}
	if len(r.residual) > 0 {
		r.printf("\nLeft behind, because something may still be running it; delete once nothing is:\n")
		for _, p := range r.residual {
			r.printf("  %s\n", p)
		}
	}
	if !r.purge {
		inside, _ := r.purgeSet()
		r.printf("\nYour participant state remains at %s:\n", r.home)
		for _, p := range inside {
			r.printf("  %s\n", p)
		}
		if len(inside) == 0 {
			r.printf("  (nothing is there)\n")
		}
		if !r.binary {
			// #88: the old wording ended on `connect`, which read as the
			// next step. It is the opposite — setup -home is what reuses
			// this registration, and a bare connect is what replaces it,
			// because uninstall has just removed the profile block (on
			// Windows, the user environment) that pointed at this
			// installation. With nothing pointing at it, connect reads the
			// default state location, finds no registration there, and
			// makes a new one.
			r.printf("\nTo keep using this installation, run:\n"+
				"  dropin-miner setup -home %s\n"+
				"It finds this state and uses it: the same agent, the same wallet, no new registration.\n"+
				r.otherInstallationHint()+
				"Or, to use this binary without setting it up again, pass -config %s to each command.\n"+
				"Do not run `dropin-miner connect` on its own to come back: with nothing naming this\n"+
				"installation any more, it would register this machine anew.\n", r.home, r.cfgPath)
		} else {
			r.printf("\nTo come back, reinstall and run setup; it finds this state and uses it.\n")
		}
		r.printf("Nothing was revoked: uninstall without -purge-state leaves every authorization as it is.\n")
		r.sayLeftoverLocks()
		return
	}
	r.printf("\n%s\n", revocation)
	r.printf("Platform authorization: revoked only at the console; removing local state does not revoke it.\n")
	if entries, err := os.ReadDir(r.home); err == nil && len(entries) > 0 {
		r.printf("\nLeft in %s:\n", r.home)
		for _, e := range entries {
			r.printf("  %s\n", e.Name())
		}
	}
	if _, outside := r.purgeSet(); len(outside) > 0 {
		r.printf("\nLeft outside %s, as configured:\n", r.home)
		for _, p := range outside {
			r.printf("  %s\n", p)
		}
	}
	r.sayLeftoverLocks()
	r.printf("Other commands were excluded while the locks were held; an agent session still open can\n" +
		"recreate intake/ or sessions/ by searching, which is harmless.\n")
}

// sayLeftoverLocks names every lock file this installation still carries and
// says it is safe to delete. The gate always said this of itself; #103 and
// #115 are the same sentence owed by the rest of them -- the update lock an
// upgrade leaves in bin/, and the setup.lock and connect.lock a setup or a
// connect leaves in the installation.
//
// Named rather than removed, and that is a conclusion rather than an
// omission. Removing an operation lock is only safe while the GATE is held,
// because the gate is what every contender passes before it opens one: with
// it held, nobody can be between opening a lock file and locking it, so
// unlinking the name cannot strand a contender on an inode that no longer
// has one. An ordinary operation has deliberately released the gate by the
// time it holds its own lock -- connect gives it up before its poll loop --
// and taking it back at the end would be the reverse of the one lock order
// (gate, setup.lock, connect.lock, flush.lock). That reversal is not a
// hypothetical: it is what TestSetupConnectsUnderItsOwnAdmission exists to
// forbid, and a first attempt at removing these files in place tripped it on
// the first run. The destructive exclusion is the one operation that holds
// the gate throughout, which is why L5's removal lives there and only there.
//
// The reason recorded at excludeForUpgrade therefore still holds, and this
// is the half of it that was missing: a reader of the installation is told.
func (r *uninstallRun) sayLeftoverLocks() {
	locks := r.leftoverLocks()
	if len(locks) == 0 {
		return
	}
	r.printf("\nLeft, and safe to delete — the lock files DropinMiner commands coordinate through.\n" +
		"Each holds nothing once the command that made it has finished, and is made again by the\n" +
		"next command that needs it:\n")
	for _, p := range locks {
		r.printf("  %s\n", p)
	}
}

// refreshTokenLockFile is the refresh-token lock pkg/auth takes in the state
// directory (refreshLockFile, pkg/auth/refreshlock.go), named here because
// that package does not export it; TestTheRefreshLockNameIsPkgAuths holds
// the two spellings together.
const refreshTokenLockFile = "refresh.token.lock"

// leftoverLocks is the lock files that still exist for this installation:
// first in lock order the gate, setup.lock, connect.lock, flush.lock and the
// binary's update lock, then the two that belong to other subsystems and to
// no lock order -- the refresh-token lock beside connect.lock in the state
// directory, and the wallet's creation lock (#136). All seven are the same
// kind of file: empty, held only while their command runs, made again by the
// next one. A path that is gone -- uninstall -binary takes the update lock
// with the binary -- is left out rather than named.
func (r *uninstallRun) leftoverLocks() []string {
	candidates := []string{lifecycleGatePath(r.home), filepath.Join(r.home, setupLockFile)}
	stateDir := ""
	if connectLock, flushLock, err := operationLockPaths(r.home, r.d.getenv); err == nil {
		candidates = append(candidates, connectLock, flushLock)
		if connectLock != "" {
			stateDir = filepath.Dir(connectLock)
		}
	}
	if owned := r.ownedBinary(); owned != "" {
		candidates = append(candidates, owned+updateLockSuffix)
	}
	if stateDir != "" {
		candidates = append(candidates, filepath.Join(stateDir, refreshTokenLockFile))
	}
	candidates = append(candidates, filepath.Join(r.home, "wallet", walletLockFile))
	var out []string
	seen := map[string]bool{}
	for _, p := range candidates {
		if p == "" || seen[p] || !lexists(p) {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// ── the dry-run overlay ─────────────────────────────────────────────────

// planOverlay lets a sequence of plans be computed as if each were applied,
// without writing anything: writes and removes land in memory, and reads
// see them.
type planOverlay struct {
	base    agentOps
	written map[string][]byte
	modes   map[string]os.FileMode
	removed map[string]bool
}

func newPlanOverlay(base agentOps) *planOverlay {
	return &planOverlay{base: base, written: map[string][]byte{}, modes: map[string]os.FileMode{}, removed: map[string]bool{}}
}

func (o *planOverlay) gone(path string) bool {
	if _, ok := o.written[path]; ok {
		return false
	}
	for p := path; ; p = filepath.Dir(p) {
		if o.removed[p] {
			return true
		}
		if filepath.Dir(p) == p {
			return false
		}
	}
}

func (o *planOverlay) ops() agentOps {
	ops := o.base
	ops.readFile = func(path string) ([]byte, error) {
		if o.gone(path) {
			return nil, fs.ErrNotExist
		}
		if b, ok := o.written[path]; ok {
			return append([]byte(nil), b...), nil
		}
		return o.base.readFile(path)
	}
	ops.stat = func(path string) (os.FileInfo, error) {
		if o.gone(path) {
			return nil, fs.ErrNotExist
		}
		if b, ok := o.written[path]; ok {
			return overlayInfo{name: filepath.Base(path), size: int64(len(b)), mode: o.modes[path]}, nil
		}
		return o.base.stat(path)
	}
	ops.writeFile = func(path string, b []byte, mode os.FileMode) error {
		o.written[path] = append([]byte(nil), b...)
		o.modes[path] = mode
		return nil
	}
	ops.mkdirAll = func(string, os.FileMode) error { return nil }
	ops.removeAll = func(path string) error {
		for p := range o.written {
			if pathWithin(p, path) {
				delete(o.written, p)
			}
		}
		o.removed[path] = true
		return nil
	}
	return ops
}

type overlayInfo struct {
	name string
	size int64
	mode os.FileMode
}

func (i overlayInfo) Name() string       { return i.name }
func (i overlayInfo) Size() int64        { return i.size }
func (i overlayInfo) Mode() os.FileMode  { return i.mode }
func (i overlayInfo) ModTime() time.Time { return time.Time{} }
func (i overlayInfo) IsDir() bool        { return false }
func (i overlayInfo) Sys() any           { return nil }
