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
	"strconv"
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
		r.printf("\nDry run: nothing was changed, asked or contacted.\n")
		return exitOK
	case r.purge:
		if code, ok := r.confirmPurge(); !ok {
			return code
		}
	default:
		if !r.ask("Remove what is listed above?") {
			r.say(noParticipantChange)
			return exitOK
		}
	}

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
	ex := &lifecycleExclusion{home: r.home, gate: gate}
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
	for _, t := range r.d.targets {
		var agnostic agentPlan
		t.PlanUninstall(ops, paths, binEntry{command: uninstallProbeCommand}, &agnostic)
		skip := map[string]bool{}
		if foreign := foreignBinary(ops, agnostic.removes, r.candidates, r.d.windows); foreign != "" {
			for _, w := range agnostic.writes {
				skip[w.path] = true
			}
			for _, rm := range agnostic.removes {
				skip[rm] = true
			}
			left = append(left, fmt.Sprintf("%s: left in place; it runs %s, not this installation", t.Label(), foreign))
		}
		for _, c := range r.candidates {
			var p agentPlan
			t.PlanUninstall(ops, paths, binEntry{command: c}, &p)
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
		if !skip[rm] {
			out.removes = append(out.removes, rm)
		}
	}
	return out
}

// installedCommand finds the binaries a rendered skill runs: the quoted path
// before " search", " hook" or " agents prefer", as renderSkill writes it.
var installedCommand = regexp.MustCompile(`"((?:[^"\\\n]|\\.)*)" (?:search|hook|agents prefer)\b`)

// foreignBinary returns a binary named in the files a target would remove
// when none of them names one of candidates, and "" otherwise — including
// when they name no binary at all.
func foreignBinary(ops agentOps, removes, candidates []string, windows bool) string {
	foreign := ""
	for _, path := range removes {
		for _, content := range readRemoved(ops, path) {
			for _, m := range installedCommand.FindAllStringSubmatch(content, -1) {
				bin, err := strconv.Unquote(`"` + m[1] + `"`)
				if err != nil {
					continue
				}
				for _, c := range candidates {
					if bin == c || (windows && strings.EqualFold(bin, c)) || sameFile(bin, c) {
						return ""
					}
				}
				if foreign == "" {
					foreign = bin
				}
			}
		}
	}
	return foreign
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
	flushLock := r.predictedFlushLockPath()
	for _, p := range append(first, last...) {
		if lexists(p) || (p == flushLock && flushLock != "" && dirExists(filepath.Dir(p))) {
			inside = append(inside, p)
		}
	}
	sort.Strings(outside)
	return inside, outside
}

// predictedFlushLockPath is the flush lock a real -purge-state run's own
// lifecycle exclusion (excludeLifecycle, lifecycle.go) tries to hold before
// this plan is ever computed — home/flush.lock with no config, or the
// config's own miner root once one loads, the same path operationLockPaths
// resolves. Holding it opens the file with O_CREATE (tryLockFile,
// minerlock_*.go), so an absent lock the exclusion can still take is
// created as a side effect of a real run, not left for purgeSet to find.
// The exclusion is skipped on a dry run (its own O_CREATE would itself be
// an undisclosed write), so this predicts the same outcome instead of
// reading a file that a dry run never gave the chance to appear.
func (r *uninstallRun) predictedFlushLockPath() string {
	if r.cfg == nil || r.cfg.Miner.IntakeDir == "" {
		return filepath.Join(r.home, "flush.lock")
	}
	return flushLockPath(r.cfg.Miner)
}

// dirExists is exactly the condition lifecycleExclusion.hold uses to decide
// whether an operation lock can be taken at all: a lock whose directory is
// missing is never created, by a dry run's prediction or a real run's own
// attempt.
func dirExists(dir string) bool {
	_, err := os.Stat(dir)
	return err == nil
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

func (r *uninstallRun) ask(question string) bool {
	if r.yes {
		r.printf("\n%s [Y/n]: yes (-yes)\n", question)
		return true
	}
	r.printf("\n%s [Y/n]: ", question)
	line, err := readSetupLine(r.d.stdin)
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true
	}
	return false
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
	r.printf("\nThis destroys the participant state listed above. If the wallet holds funds and you\n"+
		"have not kept its 24 words, they are lost. This confirmation guards against accidents;\n"+
		"it cannot tell a person from a program typing at this terminal.\n"+
		"Type %s to permanently remove this participant state:\n  %s\n> ", what, token)
	line, err := readSetupLine(r.d.stdin)
	if err != nil && line == "" {
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
			r.printf("\nTo use this binary without setting it up again, pass -config %s;\n"+
				"or run: dropin-miner setup -home %s\n"+
				"A bare `dropin-miner connect` afterwards would register this machine anew.\n", r.cfgPath, r.home)
		} else {
			r.printf("\nTo come back, reinstall and run setup; it finds this state and uses it.\n")
		}
		r.printf("Nothing was revoked: uninstall without -purge-state leaves every authorization as it is.\n")
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
	r.printf("\nLeft: %s — it coordinates DropinMiner commands, holds nothing, and is safe to delete.\n",
		lifecycleGatePath(r.home))
	r.printf("Other commands were excluded while the locks were held; an agent session still open can\n" +
		"recreate intake/ or sessions/ by searching, which is harmless.\n")
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
