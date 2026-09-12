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
  search     one web search through the router, two forms: dropin-miner
             search [-tier fast] [-format json|model] <query words> for a
             person, or dropin-miner search --stdin for an agent (one
             version-1 JSON request object on stdin, one JSON envelope on
             stdout; -format is ignored, the envelope is always JSON).
             -timeout bounds the whole search in either form, default
             1m0s. Records the served request for mining and starts a
             flush. Exit: 0=valid search response, 1=transport/timeout/
             cancel, 2=usage, 3=HTTP 4xx, 4=HTTP 5xx or invalid server response.
  agents     agents install|status|uninstall — find Claude Code, Codex,
             Cursor, opencode, Pi and Hermes on this machine and give each
             the search skill and the hooks it supports. -dry-run previews,
             -yes skips the prompt, -client <name> picks one (claude, codex,
             cursor, opencode, pi, hermes).
             agents prefer on|off — whether this search or the agent's own
             is the default (off keeps this one for when you name it);
             in the agent, /dropin-miner off and /dropin-miner on do the same
  hook       internal: the hook runner the agents call around a search;
             TOKENDROP_TRACE=off turns traces off entirely
  flush      the mining plane, once: join the open epoch, promote recorded
             searches into the spool, submit. Started by search and by the
             session hooks; run it by hand to see what is pending
             (-force asks the AS even if the last flush just did)

onboarding (one-time, per agent): register with the search platform, claim
it at one URL, done. search works the moment connect stores the key,
before the claim; mining (if granted) enrolls and declares a payout
unattended once claimed.
  connect    ask whether to mine, then register [-name …] (the answer hints
             the claim page's mining pre-tick); prints the claim URL and
             code and polls (bounded) until claimed. A second run resumes;
             so does the next search, automatically. -json emits one
             JSON envelope instead of narration; if a participant
             decision is required it reports that rather than prompting
  mining enable  turn mining on for an already-connected agent: asks for a
             payout address (empty creates a wallet — passphrase, mnemonic
             once, exactly like wallet init) or re-prints the claim URL if
             the scope was not granted yet
  mining disable stop mining here: revokes this installation's own AS
             family, best-effort — never blocks on the network. The
             platform's granted scope survives it; only a human at the
             console revokes that. mining enable mints a fresh family
  status     report what this installation has and has not completed.
             -json reports as one JSON object instead of text

manual enrollment (the portal's older path; still works, coexists with
connect): enroll -> payout -> join -> login
  login      store your sr- key for searches: reads it from stdin (or
             -key-env VAR), checks it against the router without spending,
             writes ~/.tokendrop/credentials.json owner-only. -show says
             where a search would get its key; -forget removes the file
  enroll     obtain an authorization grant. Default is the device flow (a
             person approves in a browser); -assertion redeems an
             enrollment token from stdin and needs no browser at all
  payout     payout set <address> proposes where to be paid; payout show
             reads the proposal back. A first address takes effect on
             arrival; a change waits for a Slot operator to approve it
  join       join the configured slot and the open target epoch (flush does
             this too; run it once after enrolling so the first hour counts)
  provider   register a participant provider verification key from stdin, or
             provider -status; only when the Slot accepts OPENROUTER_V1

is it working, was I paid:
  doctor     checks in a participant's terms — authorization server,
             enrolled, joined this epoch, payout address, earning, intake
             writable, recording — each saying what to do. Exits non-zero
             only when every check came back UNKNOWN: a NO is a successful
             diagnosis. "intake writable", when that check is active, runs
             one bounded probe operation using at most one inert non-.json
             file in the directory a search records into; cleanup is
             attempted and a leftover is reported by pathname.
             "recording" flags recent miner activity with
             nothing queued locally or verified at the AS. -json reports as
             one JSON object instead of text
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
[miner] block says router intake is configured (whether mining is ON is the
persisted decision, not a config key); [platform] names two hosts — base_url,
the portal a printed claim URL is checked against and nothing dials
(platform.nyks.dev), and agents_api_url, where connect and mining enable
actually send register/status/enroll (agents-v1.nyks.dev). Searches take
your sr- key from TOKENDROP_API_KEY if it is set, else from the file
login (or connect) wrote. Otherwise run connect or login to set up search.
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
		return cmdSearch(args, os.Stdin, os.Stdout, os.Stderr, os.Getenv)
	case "agents":
		return cmdAgents(args, os.Stdin, os.Stdout, os.Stderr, os.Getenv)
	case "hook":
		return cmdHook(args, os.Stdin, os.Stdout, os.Stderr, os.Getenv)
	case "flush":
		return cmdFlush(args, os.Stdout, os.Stderr, os.Getenv)
	case "login":
		return cmdLogin(args, os.Stdin, os.Stdout, os.Stderr, os.Getenv)
	case "connect":
		return cmdConnect(args, os.Stdin, os.Stdout, os.Stderr, os.Getenv)
	case "mining":
		return cmdMining(args, os.Stdin, os.Stdout, os.Stderr, os.Getenv)
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
