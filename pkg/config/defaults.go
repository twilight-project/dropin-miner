package config

// Every network default the binary carries, in one place. Two hosts of
// truth are worse than one: before this file the same six literals
// (chain, AS, router, platform, agents API, and the wallet's node) were
// duplicated across this package and cmd/dropin-miner, plus a seventh
// copy in each of scripts/setup.sh and scripts/install.ps1 for the
// installers' generated config — five places a network cutover had to
// change in lockstep, with nothing that noticed a miss.
// TestInstallerDefaultsMatchGo reads the two scripts off disk and
// asserts their literals equal these; TestDefaultsAreTestnet asserts
// every value here is still the testnet one and every URL is https.
const (
	DefaultChainID   = "twilight-testnet-1"
	DefaultSlotID    = 3
	DefaultASBaseURL = "https://rewards.nyks.dev"
	DefaultRouterURL = "https://router-api.nyks.dev"
	// DefaultPlatformBaseURL is the search platform's human-facing portal
	// and claim pages: what a printed claim_url is checked against
	// (invariant 12). It is NOT where connect/mining enable's own
	// requests go — see DefaultAgentsAPIURL for that.
	DefaultPlatformBaseURL = "https://platform.nyks.dev"
	// DefaultAgentsAPIURL is where connect/mining enable actually send
	// register/status/enroll. Confirmed live: a register call against
	// platform.nyks.dev 404s — that route only exists on this separate
	// host — while the claim_url it returns correctly points back at
	// platform.nyks.dev. One shared origin was this package's original
	// assumption; the real deployment splits it in two.
	DefaultAgentsAPIURL = "https://agents-v1.nyks.dev"
	DefaultWalletDenom  = "utwlt"
	DefaultBech32HRP    = "twilight"
)

// DefaultWalletNodes is keyed by chain id: the CometBFT RPC node the
// wallet uses when nothing overrides it (-node, then
// TOKENDROP_WALLET_NODE, then this table). One row per chain we know; a
// chain with no row has no default — the wallet refuses before any
// network call rather than guessing. Adding a network is adding a row
// here.
//
// There is deliberately no liveness pre-flight for these: a dead node
// fails the first request with the existing "cannot reach the node"
// message, and a live node on the wrong chain fails the wallet's own
// chain-id comparison before it ever signs. That comparison — not this
// table — is what makes carrying a default safe across this testnet,
// later testnets, and mainnet: the client can never sign for a node
// whose chain id is not the one it was configured for.
var DefaultWalletNodes = map[string]string{
	DefaultChainID: "https://rpc.nyks.dev",
}
