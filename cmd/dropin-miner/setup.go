package main

// setup: everything after the binary, asked as it goes.
//
// It is scripts/setup.sh's flow, in order, in the binary — so it runs the
// same on Windows, needs no POSIX shell, and can be tested in-process:
//
//  1. the binary it is running as (refusing an npm copy that is not a
//     global install: hooks written from it would point at a directory npm
//     or the project will discard)
//  2. a previous installation, in the installation directory or set aside
//     beside it — adopted by bundle, and only when a terminal says yes
//  3. the directories, owner-only before any secret moves; then adoption
//  4. the config: written fresh, or an existing one migrated (never rewritten)
//  5. connect, in-process, which asks the mining question itself
//  6. the environment: a profile block on POSIX, the User environment on
//     Windows
//  7. the coding agents
//  8. the closing message
//
// The mining question is connect's, and nothing here answers it: -yes
// answers setup's own questions only, and `[mining] enabled = true` is
// written only by a run with no terminal and TOKENDROP_MINING=1, exactly as
// the script did. Setup runs no process other than itself.

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/twilight-project/dropin-miner/pkg/auth"
)

const setupUsage = `usage: dropin-miner setup [-home dir] [-yes] [-no-profile] [-no-agents] [-with id]... [-dry-run]

Everything after the binary, asked as it goes: a previous installation to
reuse, the config, connect (registration, the mining question and the claim
link), the shell profile (PATH, TOKENDROP_CONFIG and, when a wallet was made
here, TOKENDROP_WALLET_DIR; on Windows, the user PATH and TOKENDROP_CONFIG
only) and the coding agents found on this machine.

  -home dir     the installation directory (default $TOKENDROP_HOME, else ~/.tokendrop).
                A directory other than that default is a separate installation:
                setup leaves the shell profile (Windows: user environment) and the
                coding agents alone, -yes or not, because they belong to the
                default one, and names the agents command for this one instead
  -yes          answer yes to setup's shell-profile (Windows: user environment) and
                coding-agents questions, with or without a terminal; automated
                callers may pass it, and the installers never add it. Also yes to
                reusing a set-aside installation, at a terminal only. Never
                answers connect's mining question
  -no-profile   leave the shell profile (on Windows, the user environment) alone
  -no-agents    do not look for coding agents; -with still sets up what it names
  -with id      set up this target whether or not it was found; repeatable
                (` + "{{TARGET_IDS}}" + `)
  -dry-run      print every file that would be written or moved; change nothing,
                and run neither connect nor the agents step
`

func renderSetupUsage() string {
	return strings.Replace(setupUsage, "{{TARGET_IDS}}", allTargetIDs(), 1)
}

// setupDeps is everything setup reaches outside itself, so a test can run the
// whole command in-process against a sandbox.
type setupDeps struct {
	stdin       io.Reader
	stdout      io.Writer
	stderr      io.Writer
	getenv      func(string) string
	userHome    string
	executable  func() (string, error)
	interactive bool
	agents      agentOps
	connect     func(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int
	userEnv     userEnvironment
	now         func() time.Time
	// move and restrict are adoption's file operations; nil means
	// fsx.MoveFileDurable and restrictToOwner. Tests inject failures here.
	move     func(from, to string) error
	restrict func(path string, dir bool) error
	// agentPlanObserver, when non-nil, is called with the plan agentsStep
	// builds — after buildInstallPlan, before anything is printed or
	// committed — so a test can inspect exactly what production code
	// planned, for a dry run and a real run alike, without buildInstallPlan
	// ever needing to be called a second time by the test itself. nil in
	// production; production behavior is unchanged either way.
	agentPlanObserver func(agentPlan)
}

func (d setupDeps) restrictFn() func(string, bool) error {
	if d.restrict != nil {
		return d.restrict
	}
	return restrictToOwner
}

func cmdSetup(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	return setupMain(systemSetupDeps(stdin, stdout, stderr, getenv), args)
}

// systemSetupDeps is setup against the real machine.
func systemSetupDeps(stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) setupDeps {
	home, _ := os.UserHomeDir()
	interactive := false
	if f, ok := stdin.(*os.File); ok {
		// Not a mode check: on Windows the NUL device reports itself as a
		// character device, and a redirected install must not look like a
		// person at a console.
		interactive = term.IsTerminal(int(f.Fd()))
	}
	return setupDeps{
		stdin:       stdin,
		stdout:      stdout,
		stderr:      stderr,
		getenv:      getenv,
		userHome:    home,
		executable:  os.Executable,
		interactive: interactive,
		agents:      realAgentOps(),
		connect:     connectAdmitted,
		userEnv:     systemUserEnvironment(),
		now:         time.Now,
	}
}

// setupRun is one invocation's state.
type setupRun struct {
	d setupDeps

	yes, noProfile, noAgents, dry bool

	exe     string
	home    string
	cfgPath string
	values  setupValues

	targets         []installTarget
	targetSignals   map[string]string // host id → what made it count as present
	explicitTargets bool

	adoptFrom     string // the accepted adoption source, if any
	foundInPlace  bool   // an installation was already in home when setup started
	changed       bool   // setup wrote or moved something it owns
	shortCommands bool   // the profile or user environment carries PATH and TOKENDROP_CONFIG

	// otherHome: -home names a directory that is not this machine's
	// installation, defaultHome. The profile and the agents belong to that
	// one, so both steps are skipped whatever else was passed (#84).
	otherHome   bool
	defaultHome string

	// skipped names steps this run declined to touch — no terminal without
	// -yes, or -no-profile/-no-agents (#75) — as distinct from a step that
	// found nothing to do (already in place, or no agent to configure).
	// closing() uses it: "already in place" is true only when this stays
	// empty, never when a step was merely never attempted.
	skipped []string

	// configPlanData is set by config() to the bytes it is about to publish
	// (configFresh or configMigrated; nil for configLeft, which changes
	// nothing) — agentsStep() needs it: on a dry run, config() prints what
	// it would write but never calls publishSetupConfig, so nothing is on
	// disk yet for the agents step's own config read to find. It is set
	// whether or not the run is dry, but only a dry run's agentsStep reads
	// it — the real run has already published it to r.cfgPath by then.
	configPlanData []byte

	lineIn io.Reader
}

func (r *setupRun) printf(format string, args ...any) { fmt.Fprintf(r.d.stdout, format, args...) }
func (r *setupRun) say(msg string)                    { fmt.Fprintf(r.d.stdout, "\n%s\n", msg) }
func (r *setupRun) skip(step string)                  { r.skipped = append(r.skipped, step) }

// ask is a yes/no question, [Y/n], answered yes by -yes. Without -yes it is
// only reached with a terminal: every caller decides that case first. The
// environment and agents questions take -yes with or without a terminal, so
// an automated caller may pass it (the installers never add it: a person
// running one answers at the terminal); adopting a set-aside installation is never
// asked without a terminal, -yes or not.
//
// The error is errPromptAborted and nothing else: a read that ended
// without a line is not the "no" this used to return (prompt.go). Every
// caller propagates it as an exit code rather than carrying on, which is
// why the steps that ask return one.
func (r *setupRun) ask(question string) (bool, error) {
	if r.yes {
		r.printf("\n%s [Y/n]: yes (-yes)\n", question)
		return true, nil
	}
	line, err := promptSetup(r.d.stdout, "\n"+question+" [Y/n]: ", r.lineIn)
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true, nil
	}
	return false, nil
}

// abort renders the one abort message. what names, in the step's own
// words, the thing that was therefore not done.
func (r *setupRun) abort(what string) int {
	fmt.Fprintf(r.d.stderr, "\ndropin-miner setup: %s; %s\n", promptAbortedReason, what)
	return exitUsage
}

// readSetupLine reads one line a byte at a time. connect reads the same
// stdin after setup's first question, so setup must never buffer past the
// newline it was answered with.
func readSetupLine(r io.Reader) (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if n == 1 {
			if buf[0] == '\n' {
				return b.String(), nil
			}
			b.WriteByte(buf[0])
		}
		if err != nil {
			return b.String(), err
		}
	}
}

// displayPath quotes a path for a command a participant copies, only when it
// needs it. A plain function as well as a method, because uninstall prints
// such a command too and the two must quote identically.
func (r *setupRun) displayPath(p string) string { return displayPath(p) }

func displayPath(p string) string {
	if strings.ContainsAny(p, " \t\n'\"\\$`&;|<>()*?[]#~!{}") && filepath.Separator == '/' {
		return shellQuote(p)
	}
	if strings.ContainsAny(p, " &;") {
		return `"` + p + `"`
	}
	return p
}

// errNpmLaunch is setup's refusal to install from an npm copy other than a
// global one.
var errNpmLaunch = errors.New("setup must run from a global npm install: npm install -g dropin-miner, then dropin-miner setup")

// errNpmDirect is a binary inside node_modules run by its own path, around the
// launcher: nothing then says which kind of install it is, so setup cannot
// tell a global copy from one a project will discard.
var errNpmDirect = errors.New("this is npm's copy of the binary, run directly; run the dropin-miner command npm installed, which tells setup how it was installed")

func setupMain(d setupDeps, args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	fs.Usage = func() {}
	homeFlag := fs.String("home", "", "the installation directory")
	r := &setupRun{d: d, lineIn: d.stdin}
	fs.BoolVar(&r.yes, "yes", false, "answer yes to setup's own questions")
	fs.BoolVar(&r.noProfile, "no-profile", false, "leave the shell profile alone")
	fs.BoolVar(&r.noAgents, "no-agents", false, "do not look for coding agents")
	var with multiFlag
	fs.Var(&with, "with", "set up this target; repeatable")
	fs.BoolVar(&r.dry, "dry-run", false, "print what would change, change nothing")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// The installers' capability probe: `setup -h` exiting 0 is how
			// they know this binary has setup at all.
			fmt.Fprint(d.stdout, renderSetupUsage())
			return exitOK
		}
		fmt.Fprint(d.stderr, renderSetupUsage())
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(d.stderr, "dropin-miner setup: unexpected argument %q\n%s", fs.Arg(0), renderSetupUsage())
		return exitUsage
	}
	return r.run(*homeFlag, with)
}

func (r *setupRun) run(homeFlag string, with []string) int {
	d := r.d

	// ── 1. the binary ──
	exe, err := d.executable()
	if err != nil {
		fmt.Fprintln(d.stderr, "dropin-miner setup: cannot determine my own path:", err)
		return exitTransport
	}
	if err := checkSetupLaunch(exe, d.getenv("DROPIN_MINER_LAUNCH")); err != nil {
		fmt.Fprintln(d.stderr, "dropin-miner setup:", err)
		return exitUsage
	}
	r.exe = exe

	paths := d.agents.paths(d.getenv)
	r.targets, r.targetSignals, r.explicitTargets, err = setupTargets(d.agents, paths, d.getenv, with, r.noAgents)
	if err != nil {
		fmt.Fprintln(d.stderr, "dropin-miner setup: -with:", err)
		return exitUsage
	}

	home := homeFlag
	if home == "" {
		home = d.getenv("TOKENDROP_HOME")
	}
	if home == "" {
		if d.userHome == "" {
			fmt.Fprintln(d.stderr, "dropin-miner setup: no home directory to install under; pass -home")
			return exitUsage
		}
		home = filepath.Join(d.userHome, ".tokendrop")
	}
	if home, err = filepath.Abs(home); err != nil {
		fmt.Fprintln(d.stderr, "dropin-miner setup:", err)
		return exitUsage
	}
	r.home = home
	r.cfgPath = filepath.Join(home, setupConfigFile)
	r.defaultHome, r.otherHome = otherInstallation(homeFlag, home, d.getenv("TOKENDROP_HOME"), d.userHome)
	if r.values, err = resolveSetupValues(home, d.getenv, d.interactive); err != nil {
		fmt.Fprintln(d.stderr, "dropin-miner setup:", err)
		return exitUsage
	}

	// A dry run writes nothing, so there is nothing for it to exclude or be
	// excluded from; every other run holds setup.lock to its end.
	if !r.dry {
		release, code := r.admit()
		if code != exitOK {
			return code
		}
		defer release()
	}

	r.say("Using binary: " + exe)
	r.printf("dropin-miner %s\n", buildVersion())
	if r.dry {
		r.printf("(dry run: nothing is written, moved or run)\n")
	}

	// ── 2. a previous installation ──
	if code := r.previousInstallation(); code != exitOK {
		return code
	}

	// ── 3. directories, owner-only before any secret moves; then adoption ──
	if code := r.directories(); code != exitOK {
		return code
	}
	if r.adoptFrom != "" {
		a := &adoption{src: r.adoptFrom, dst: home, dry: r.dry, now: d.now(), out: d.stdout, restrict: d.restrictFn(), moveFn: d.move}
		if err := a.run(); err != nil {
			printAdoptionError(d.stderr, err)
			return exitTransport
		}
		a.finish()
		if len(a.moved) > 0 {
			r.changed = true
		}
	}
	if code := r.walletAccess(); code != exitOK {
		return code
	}
	r.miningDecisionNote()

	// ── 4. config ──
	if code := r.config(); code != exitOK {
		return code
	}
	if code := r.flushLock(); code != exitOK {
		return code
	}

	// ── 5. connect ──
	r.say("Search context")
	r.printf("Recent agent context may accompany search to the Twilight search router as part of the trajectory/search product.\n")
	r.printf("TOKENDROP_TRACE=off disables trace transmission. Mining/AS receives metadata observations only.\n")
	r.say("Connecting")
	if r.dry {
		r.printf("(dry run) would run: %s connect -config %s\n", r.displayPath(exe), r.displayPath(r.cfgPath))
	} else {
		walletDir := filepath.Join(home, "wallet")
		getenv := func(k string) string {
			// A wallet connect creates lands beside the rest of this
			// installation, not in the OS config directory.
			if k == walletDirEnv {
				return walletDir
			}
			return d.getenv(k)
		}
		if code := d.connect([]string{"-config", r.cfgPath}, d.stdin, d.stdout, d.stderr, getenv); code != exitOK {
			fmt.Fprintf(d.stderr, "\ndropin-miner setup: connect failed (exit %d); run setup again when that is fixed — it picks up where this left off\n", code)
			return exitTransport
		}
	}

	// ── 6. environment ──
	if code := r.environmentStep(); code != exitOK {
		return code
	}

	// ── 7. coding agents ──
	if code := r.agentsStep(); code != exitOK {
		return code
	}

	// ── 8. closing ──
	r.closing()
	return exitOK
}

// otherInstallation: does -home name a directory other than this machine's
// installation — $TOKENDROP_HOME, else ~/.tokendrop — and if so, which is
// that? Only an explicit -home can: with no flag, home IS the default by
// construction, and `TOKENDROP_HOME=<dir> install.sh` stays the way to put
// the machine's installation somewhere else. When no default can be named at
// all there is nothing to compare against, and nothing is withheld.
//
// #84: `setup -home <scratch>` is the documented way to make a disposable
// installation for a destructive test, and it planned the real user's
// ~/.claude, ~/.codex, ~/.cursor, Pi and Hermes files and the user PATH and
// TOKENDROP_CONFIG all the same — repointing the participant's real agents
// and environment at the scratch config. The Windows tester avoided it only
// by reading the dry run first.
func otherInstallation(homeFlag, home, envHome, userHome string) (defaultHome string, other bool) {
	if homeFlag == "" {
		return "", false
	}
	def := envHome
	if def == "" && userHome != "" {
		def = filepath.Join(userHome, ".tokendrop")
	}
	if def == "" {
		return "", false
	}
	abs, err := filepath.Abs(def)
	if err != nil {
		return "", false
	}
	return abs, !sameInstallationDir(home, abs)
}

// sameInstallationDir: one directory, however it was spelled. The spelling
// first, as samePath compares it; then, since `-home` is typed by a person
// and the default may be reached through a link, what both resolve to.
func sameInstallationDir(a, b string) bool {
	if samePath(a, b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && samePath(ra, rb)
}

// leftForOtherInstallation is the first thing the profile step and the agents
// step ask. It uses the mechanism D4 added for #75 — the step is named as
// skipped, so the closing line cannot claim everything is in place — and it
// is asked before -yes, -with, -no-profile or the terminal are, because none
// of them changes whose profile and whose agents these are.
func (r *setupRun) leftForOtherInstallation(step, theirs, consequence string) bool {
	if !r.otherHome {
		return false
	}
	r.printf("Skipped: -home names %s, which is not this machine's installation (%s).\n"+
		"%s, and %s. -yes does not change this.\n",
		r.home, r.defaultHome, theirs, consequence)
	r.skip(step)
	return true
}

// admit passes the lifecycle gate and takes setup.lock for the rest of the
// run: gate first, then the installation directory (owner-only before
// anything is created in it), then setup.lock, then the gate is let go. An
// uninstall or upgrade holding the gate refuses this run before it creates
// anything; one that starts later finds setup.lock held and refuses itself.
// connect runs in-process under this admission (connectAdmitted).
func (r *setupRun) admit() (release func(), code int) {
	gatePath := lifecycleGatePath(r.home)
	gate, err := admitOrdinary(gatePath, admitForeground)
	if err != nil {
		fmt.Fprintf(r.d.stderr, "dropin-miner setup: %v (%s); nothing was changed, run setup again shortly\n", err, gatePath)
		return nil, exitTransport
	}
	defer gate.release()
	if !lexists(r.home) {
		if err := os.MkdirAll(r.home, 0o700); err != nil {
			fmt.Fprintln(r.d.stderr, "dropin-miner setup:", err)
			return nil, exitTransport
		}
		r.changed = true
	}
	if err := restrictToOwner(r.home, true); err != nil {
		fmt.Fprintf(r.d.stderr, "dropin-miner setup: restrict %s to its owner: %v\n", r.home, err)
		return nil, exitTransport
	}
	lockPath := filepath.Join(r.home, setupLockFile)
	lock, held, err := tryLockFile(lockPath)
	if err != nil {
		fmt.Fprintln(r.d.stderr, "dropin-miner setup: lock:", err)
		return nil, exitTransport
	}
	if !held {
		fmt.Fprintf(r.d.stderr, "dropin-miner setup: another setup is already running for %s (%s is held); nothing was changed\n", r.home, lockPath)
		return nil, exitTransport
	}
	return func() { _ = unlockFile(lock) }, exitOK
}

func (r *setupRun) previousInstallation() int {
	if hasInstallation(r.home) {
		r.foundInPlace = true
		r.say(fmt.Sprintf("Previous installation found in %s: %s", r.home, describeInstallation(r.home)))
		r.printf("Its wallet, registration and key are used as they are; only what is missing is set up.\n")
		return exitOK
	}
	found := setAsideInstallation(r.home)
	if found == "" {
		return exitOK
	}
	r.say("A previous installation is set aside at " + found)
	r.printf("It holds: %s\n", describeInstallation(found))
	r.printf("Using it means the same wallet, the same registration and no new tokens to generate.\n")
	switch {
	case r.dry:
		r.printf("(dry run) setup would ask whether to use it; if yes, it would move:\n")
		a := &adoption{src: found, dst: r.home, dry: true, now: r.d.now(), out: r.d.stdout}
		if err := a.run(); err != nil {
			r.printf("(dry run) setup would stop here, before connect:\n")
			printAdoptionError(r.d.stdout, err)
		}
	case !r.d.interactive:
		r.printf("Not an interactive shell — not touching it. Move it to %s yourself to reuse it.\n", r.home)
	default:
		use, err := r.ask("Use it?")
		if err != nil {
			// Nothing has been created in home yet at this point —
			// directories() runs after this step — so an abort here really
			// does leave both installations exactly as they were found.
			return r.abort("the set-aside installation was left where it is and nothing was set up")
		}
		if use {
			r.adoptFrom = found
		} else {
			r.printf("Left it alone. A fresh wallet and registration follow.\n")
		}
	}
	return exitOK
}

// printAdoptionError renders adoption's typed results. Every other error is
// rendered by its own text; there is no other kind today.
func printAdoptionError(w io.Writer, err error) {
	var conflict *identityConflict
	var failure *adoptionFailure
	switch {
	case errors.As(err, &conflict):
		printIdentityConflict(w, conflict)
	case errors.As(err, &failure):
		printAdoptionFailure(w, failure)
	default:
		fmt.Fprintf(w, "\ndropin-miner setup: adoption stopped: %v\nconnect was not run.\n", err)
	}
}

// printAdoptionFailure names the bundle, the reason and both installations,
// says what state they were left in, and what to do.
func printAdoptionFailure(w io.Writer, f *adoptionFailure) {
	fmt.Fprintf(w, "\ndropin-miner setup: could not adopt the %s\n  from %s\n  into %s\n  because: %s\n",
		f.Bundle, f.Source, f.Destination, f.Reason)
	if f.RollbackFailed != "" {
		fmt.Fprintf(w, "Putting it back failed too (%s). Setup stopped at once; the parts of the identity and the wallet are now at:\n", f.RollbackFailed)
		for _, p := range f.Surviving {
			fmt.Fprintf(w, "  %s\n", p)
		}
		fmt.Fprintf(w, "Nothing else was moved and connect was not run. Put those back together by hand before running setup again.\n")
		return
	}
	fmt.Fprintf(w, "Everything adoption had moved was put back: the identity and the wallet are both where they started.\n"+
		"Nothing else was moved, and connect was not run.\n"+
		"Fix the reason above, or keep %s out of %s and let setup start fresh, then run setup again.\n",
		f.Source, filepath.Dir(f.Destination))
}

// printIdentityConflict names both installations and the one thing to do.
func printIdentityConflict(w io.Writer, c *identityConflict) {
	fmt.Fprintf(w, "\ndropin-miner setup: two installations each hold an identity (a registration and its key):\n"+
		"  %s  (holds %s)\n  %s\n"+
		"Setup will not choose between them: nothing was moved, and connect was not run.\n"+
		"Choose one installation, move the other one out of %s, then run setup again.\n",
		c.Destination, c.Evidence, c.Source, filepath.Dir(c.Destination))
}

func (r *setupRun) directories() int {
	dirs := []string{r.home}
	for _, sub := range []string{"state", "spool", "intake", "sessions"} {
		dirs = append(dirs, filepath.Join(r.home, sub))
	}
	if r.dry {
		for _, dir := range dirs {
			if !lexists(dir) {
				r.printf("(dry run) would create %s (owner-only)\n", dir)
			}
		}
		return exitOK
	}
	// The installation directory is restricted first, so nothing created or
	// moved inside it is ever reachable by anyone else, even for a moment.
	for _, dir := range dirs {
		if !lexists(dir) {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				fmt.Fprintln(r.d.stderr, "dropin-miner setup:", err)
				return exitTransport
			}
			r.changed = true
		}
		if err := restrictToOwner(dir, true); err != nil {
			fmt.Fprintf(r.d.stderr, "dropin-miner setup: restrict %s to its owner: %v\n", dir, err)
			return exitTransport
		}
	}
	return exitOK
}

// walletAccess gives the installation's wallet directory, and every directory
// and file beneath it, its own owner-only access list where the platform has
// access lists to manage (wallet_acl.go). It runs after adoption, which has
// already secured a wallet it moved, and before connect. A wallet object it
// cannot secure stops setup rather than being a warning: a setup that finished
// would leave that object readable by whoever else the installation directory
// admits, while saying the installation is set up. A dry run changes no access
// list, as it creates no directory.
func (r *setupRun) walletAccess() int {
	dir := filepath.Join(r.home, "wallet")
	if r.dry || !lexists(dir) {
		return exitOK
	}
	changed, err := secureWalletTree(dir, r.d.restrictFn())
	if err != nil {
		fmt.Fprintf(r.d.stderr, "\ndropin-miner setup: could not make the wallet in %s readable only by you:\n%s\n"+
			"connect was not run. Fix the reason above, then run setup again.\n",
			dir, indentLines(err.Error(), "  "))
		return exitTransport
	}
	if changed {
		r.changed = true
		r.say("Made the wallet in " + dir + " readable only by you")
	}
	return exitOK
}

func indentLines(s, prefix string) string {
	return prefix + strings.ReplaceAll(s, "\n", "\n"+prefix)
}

// miningDecisionNote says what a decision that came with this installation
// is, rather than let connect's silence on the question read as "it forgot
// to ask": connect does not ask again while a decision is on file.
func (r *setupRun) miningDecisionNote() {
	switch storedMiningDecision(filepath.Join(r.home, "state")) {
	case auth.MiningEnabled:
		r.say("A stored mining decision came with this installation: mining is ON. connect will not ask again.")
	case auth.MiningDisabled:
		r.say("A stored mining decision came with this installation: mining is OFF.")
		r.printf("To turn it on: %s mining enable -config %s\n", r.displayPath(r.exe), r.displayPath(r.cfgPath))
	case auth.MiningDegraded:
		r.say("A stored mining decision came with this installation, and it cannot be read.")
		r.printf("Mining stays off until it is repaired: %s mining enable (or mining disable) -config %s\n", r.displayPath(r.exe), r.displayPath(r.cfgPath))
	}
}

func (r *setupRun) config() int {
	existing, err := readExistingConfig(r.cfgPath)
	if err != nil {
		fmt.Fprintln(r.d.stderr, "dropin-miner setup: refusing the config:", err)
		return exitTransport
	}
	validate := validateConfigSyntax
	if !r.dry {
		validate = validateConfigFile(r.home)
	}
	plan, err := planSetupConfig(r.cfgPath, existing, r.values, validate)
	if err != nil {
		fmt.Fprintf(r.d.stderr, "\ndropin-miner setup: %v\nNothing was changed in it. Fix or move the file aside, then run setup again.\n", err)
		return exitTransport
	}
	if plan.data != nil {
		r.configPlanData = plan.data
	}
	switch plan.outcome {
	case configLeft:
		r.say("Config already has a [miner] block: " + r.cfgPath + " (left as is)")
		return exitOK
	case configFresh:
		if r.dry {
			r.printf("(dry run) would write %s\n", r.cfgPath)
			return exitOK
		}
	case configMigrated:
		if r.dry {
			r.printf("(dry run) would add %s to %s, leaving its existing lines as they are\n", strings.Join(plan.added, " and "), r.cfgPath)
			return exitOK
		}
	}
	if err := publishSetupConfig(r.cfgPath, plan.data); err != nil {
		fmt.Fprintln(r.d.stderr, "dropin-miner setup:", err)
		return exitTransport
	}
	r.changed = true
	if plan.outcome == configMigrated {
		r.say(fmt.Sprintf("Added %s to %s; its existing lines are unchanged", strings.Join(plan.added, " and "), r.cfgPath))
	} else {
		r.say("Wrote " + r.cfgPath)
	}
	return exitOK
}

// flushLock creates the flush lock the config names when it is absent. A flush
// inside a sandbox that denies writes to the miner root can take the lock only
// read-only, which needs the file to exist already (flushlock.go).
func (r *setupRun) flushLock() int {
	path := filepath.Join(r.home, "flush.lock")
	if lexists(r.cfgPath) {
		cfg, _, err := loadConfig(r.cfgPath, r.d.getenv)
		if err != nil {
			fmt.Fprintf(r.d.stderr, "dropin-miner setup: config %s: %v\n", r.cfgPath, err)
			return exitTransport
		}
		path = ""
		if cfg.Miner.IntakeDir != "" {
			path = flushLockPath(cfg.Miner)
		}
	}
	if path == "" || lexists(path) {
		return exitOK
	}
	if r.dry {
		r.printf("(dry run) would create %s\n", path)
		return exitOK
	}
	created, err := ensureFlushLockFile(path)
	if err != nil {
		fmt.Fprintf(r.d.stderr, "dropin-miner setup: create the flush lock %s: %v\n", path, err)
		return exitTransport
	}
	r.changed = r.changed || created
	return exitOK
}

func (r *setupRun) closing() {
	cmd, hint := r.displayPath(r.exe)+" ", " -config "+r.displayPath(r.cfgPath)
	if r.shortCommands {
		cmd, hint = "dropin-miner ", ""
	}
	rule := strings.Repeat("─", 78)
	r.printf("%s\n", rule)
	switch {
	case r.dry:
		r.printf("Dry run complete: nothing was written, moved or run.\n")
	case r.otherHome:
		// Not the line below: "finish with setup -yes" would be false here,
		// since -yes does not override this. What does configure agents for
		// this installation is the command that already does exactly that.
		r.printf("Setup complete for the installation at %s.\nIt skipped the %s: -home names a directory that is not this machine's installation (%s),\n"+
			"and those belong to that one. To configure coding agents for this installation:\n\n    %s agents install -config %s\n\n"+
			"If %s is in fact this machine's installation, kept somewhere other than the default, run\n"+
			"setup with TOKENDROP_HOME set to it instead of -home: that names it as the default, and setup\n"+
			"then looks after the profile and the agents too.\n",
			r.home, joinLabels(r.skipped), r.defaultHome, r.displayPath(r.exe), r.displayPath(r.cfgPath), r.home)
	case len(r.skipped) > 0:
		r.printf("Setup complete, but setup skipped the %s (no terminal, or asked not to). Finish with:\n\n    %ssetup%s -yes\n",
			joinLabels(r.skipped), cmd, hint)
	case r.foundInPlace && !r.changed:
		r.printf("Setup complete. Everything setup looks after was already in place; nothing was changed.\n")
	default:
		r.printf("Setup complete.\n")
	}
	r.printf(`
1. If a claim URL was printed above and nobody has visited it yet, do that
   whenever you're ready — nothing else here depends on timing it. Once
   claimed, mining (if you said yes) enrolls and declares a payout on its
   own, from the next search.

2. Restart any coding agent that is already open, then search as usual. To
   try it by hand:
     %ssearch%s -format model "what is proof of authority consensus"

3. Check in any time:
     %sstatus%s   %sdoctor%s   %spayout show%s

Notes
  * Nothing runs between searches. Each search records itself and starts a
    short flush that joins the open epoch and submits; the session hooks do
    the same when an agent starts and stops.
  * First reward takes 1-2 hours — you join an epoch two ahead. One verified
    search per epoch makes you eligible; the pot splits equally.
%s
`, cmd, hint, cmd, hint, cmd, hint, cmd, hint, rule)
}
