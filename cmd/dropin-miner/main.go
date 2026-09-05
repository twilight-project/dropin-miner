// Command dropin-miner is the Twilight drop-in search miner.
//
// It is not a daemon, not a proxy, and not a tool server. It is one
// binary that a coding agent runs to search the web through the Twilight
// search router, plus the hooks that thread each search into the agent's
// trajectory, plus a one-shot flush that does the mining-plane work the
// proxy used to do on a ticker. Between tool calls nothing runs.
//
// The participant packages under pkg/ (identity, enrollment, join,
// capability, spool, submit, wallet, wire) are the proxy's, copied with
// their golden vectors; pkg/PROVENANCE names the commit.
package main

import (
	"fmt"
	"os"
	"runtime/debug"
)

// version is set at release time via:
//
//	go build -ldflags "-X main.version=v1.2.3"
var version = "dev"

const usageText = `usage: dropin-miner <command> [flags]

the tool (what an agent runs):
  search     one web search through the router: dropin-miner search
             [-tier fast] [-format json|model] <query words>. Records the
             served request for mining and starts a flush. Exit: 0=2xx,
             1=transport, 2=usage, 3=HTTP 4xx, 4=HTTP 5xx.
  agents     agents install|status|uninstall — find Claude Code, Codex,
             Cursor and opencode on this machine and give each the search
             skill and the hooks it supports. -dry-run previews, -yes skips
             the prompt, -client <name> picks one
  hook       internal: the hook runner the agents call around a search;
             TOKENDROP_TRACE=off turns traces off entirely
  flush      the mining plane, once: join the open epoch, promote recorded
             searches into the spool, submit. Started by search and by the
             session hooks; run it by hand to see what is pending
             (-force asks the AS even if the last flush just did)

enrollment (one-time, per participant): enroll -> payout -> join
  enroll     obtain an authorization grant. Default is the device flow (a
             person approves in a browser); -assertion redeems an
             enrollment token from stdin and needs no browser at all
  payout     payout set <address> proposes where to be paid; payout show
             reads the proposal back. A first address takes effect on
             arrival; a change waits for a Slot operator to approve it
  join       join the configured slot and the open target epoch (flush does
             this too; run it once after enrolling so the first hour counts)
  status     report what this installation has and has not completed

is it working, was I paid:
  doctor     checks in a participant's terms — connected, enrolled, joined,
             payout address in force, earning — each saying what to do
  earnings   what the chain has paid to your payout address

wallet (a reward address this installation controls):
  wallet init      generate a key: prints the recovery mnemonic ONCE, seals
                   the key in an encrypted file, prints the twilight address
  wallet address   print this wallet's address (no passphrase needed)
  wallet register  declare that address as the payout destination
  wallet balance   what the chain says this address holds
  wallet send      move funds to another twilight address: -to and -amount

Every command takes -config <file>, falling back to TOKENDROP_CONFIG, then
./tokendrop.toml. The [mining] block names the AS, chain and slot; the
[miner] block turns the drop-in miner on. TOKENDROP_API_KEY (your sr- key)
must be in the environment of the shell the agent runs from.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	os.Exit(dispatch(os.Args[1], os.Args[2:]))
}

func dispatch(name string, args []string) int {
	switch name {
	case "search":
		return cmdSearch(args, os.Stdout, os.Stderr, os.Getenv)
	case "agents":
		return cmdAgents(args, os.Stdin, os.Stdout, os.Stderr, os.Getenv)
	case "hook":
		return cmdHook(args, os.Stdin, os.Stdout, os.Stderr, os.Getenv)
	case "flush":
		return cmdFlush(args, os.Stdout, os.Stderr, os.Getenv)
	case "enroll":
		return cmdEnroll(args)
	case "join":
		return cmdJoin(args)
	case "provider":
		return cmdProvider(args)
	case "payout":
		return cmdPayout(args)
	case "wallet":
		return cmdWallet(args, os.Stdin, os.Stdout, os.Stderr, os.Getenv)
	case "status":
		return cmdStatus(args)
	case "doctor":
		return cmdDoctor(args, os.Stdout, os.Stderr)
	case "earnings":
		return cmdEarnings(args, os.Stdout, os.Stderr, os.Getenv)
	case "version", "-version", "--version":
		fmt.Fprintln(os.Stdout, "dropin-miner", buildVersion())
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(os.Stdout, usageText)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "dropin-miner: unknown command %q\n\n%s", name, usageText)
		return 2
	}
}

func buildVersion() string {
	if version != "dev" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	rev, dirty := "", ""
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "+dirty"
			}
		}
	}
	if rev == "" {
		return version
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	return version + " (" + rev + dirty + ")"
}
