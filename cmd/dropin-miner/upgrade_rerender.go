package main

// After a replacement commits, the host integrations this installation owns
// are rendered again by the binary that is now installed (#111).
//
// `upgrade` used to replace the binary and touch no host file. The skill text
// and the hook entries are rendered from the binary's own tables when
// `agents install` runs, so a fix that lives in a rendered file shipped in
// the release and reached nobody who upgraded: 0.2.11's Cursor fix is skill
// text, and its changelog had to tell participants to run a second command by
// hand.
//
// Three rules, each of which is the reason for a line below.
//
// It runs only after the transaction returned no error — after the canonical
// path validated and .previous was committed — because an upgrade that fails
// restores the prior binary, and the host files must then still be the prior
// binary's. The call sits after Install and after Rollback for that reason
// and no other.
//
// It never fails the upgrade. The binary is already in place and .previous
// already committed; an exit code that said otherwise would send a
// participant to retry an upgrade that succeeded. A failure is reported with
// the one command that finishes the job, and the exit code stays what the
// replacement earned.
//
// It touches only hosts this installation owns (ownedHosts, H5's rule), and
// installs nothing new: the hosts are named to the child one by one with
// -client, so a host that was detected and never set up stays not set up, and
// a host whose skill is another installation's is left and said.
//
// The render is done by a CHILD PROCESS, the binary now at the path this one
// was launched by, and that is the whole point rather than a detail: the
// process running `upgrade` is the version being replaced, and its tables are
// the old ones. Rendering in-process would rewrite every host file with
// exactly the text the upgrade exists to supersede. After a rollback the same
// holds in the other direction — the restored binary renders, so the host
// files teach what the installed binary can run.
//
// The child is given `agents install -yes -config … -client …`, a form every
// release that can be rolled back to understands, rather than a new
// subcommand only this one would.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// rerenderTimeout bounds the child inside the operation's own deadline, the
// way the candidate's five seconds are: the upgrade as a whole still ends by
// OperationTimeout, and a child that hangs cannot hold the exclusion longer
// than this.
const rerenderTimeout = 30 * time.Second

// rerenderOwned runs after the replacement committed. exe is the path this
// process was launched by and resolved the file it names; both are this
// installation's binaries for ownership, and exe is what the child is started
// as, because os.Executable is what `agents install` spelled the binary with
// when it wrote the host files, and a child started under another spelling
// would not recognize its own hook entries and would add beside them.
func (d upgradeDeps) rerenderOwned(ctx context.Context, exe, resolved, home string) {
	cfg := filepath.Join(home, setupConfigFile)
	if !lexists(cfg) {
		// No config of its own: what the child would resolve instead depends
		// on its working directory and environment, not on this installation,
		// so which host files are "this installation's" has no answer here.
		return
	}
	bins := []string{exe}
	if resolved != exe {
		bins = append(bins, resolved)
	}
	ops := d.agents
	owned, left := ownedHosts(ops, ops.paths(d.getenv), bins, cfg, runtime.GOOS == "windows", d.getenv)
	for _, l := range left {
		fmt.Fprintf(d.stdout, "  %s\n", l)
	}
	if len(owned) == 0 {
		return
	}

	args := []string{"agents", "install", "-yes", "-config", cfg}
	for _, t := range owned {
		args = append(args, "-client", t.ID())
	}
	finish := displayPath(exe) + " " + strings.Join(displayArgs(args[:2], args[3:]), " ")

	fmt.Fprintf(d.stdout, "refreshing the agent integrations this installation owns: %s\n", strings.Join(labels(owned), ", "))
	rctx, cancel := context.WithTimeout(ctx, rerenderTimeout)
	defer cancel()
	stdout, stderr, err := d.runner.Run(rctx, exe, args, d.environ())
	_, _ = d.stdout.Write(stdout)
	if err != nil {
		_, _ = d.stderr.Write(stderr)
		fmt.Fprintf(d.stderr, "dropin-miner upgrade: the binary was replaced, but the agent integrations were not all refreshed (%v)\n  finish with: %s\n", err, finish)
	}
}

// displayArgs joins argument groups for a command a participant is asked to
// run, quoting each the way displayPath quotes a path.
func displayArgs(groups ...[]string) []string {
	var out []string
	for _, g := range groups {
		for _, a := range g {
			out = append(out, displayPath(a))
		}
	}
	return out
}

// upgradeEnviron is the child's environment in production: this process's
// own, whole. Unlike a candidate's version check, which runs with almost
// nothing, the re-render has to find the same host directories this process
// would — HOME, CODEX_HOME, CLAUDE_CONFIG_DIR, XDG_CONFIG_HOME, HERMES_HOME.
func upgradeEnviron() []string { return os.Environ() }
