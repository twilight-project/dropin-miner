// Package config resolves the proxy configuration exactly once at startup —
// defaults, then a TOML file, then TOKENDROP_* environment variables, then
// flags — into an immutable struct. Nothing re-reads configuration at request
// time. Unsafe states are rejected here rather than warned about: a
// non-loopback TCP listener (no override flag exists), content logging
// (the key is accepted only in order to be rejected), and a non-https or
// credential-carrying upstream.
package config

import (
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Defaults per design_plan §5.7, §5.8, §6.1, §6.4 and §11.
const (
	DefaultListen      = "127.0.0.1:8787"
	DefaultAdminListen = "127.0.0.1:8788"
	defaultUpstream    = "https://openrouter.ai/api"
	// defaultPlatformBaseURL is the search platform's human-facing portal
	// and claim pages (agent onboarding design, search-platform-agent-
	// onboarding-design.md §5): what a printed claim_url is checked
	// against, invariant 12. It is NOT where connect/mining enable's own
	// requests go — see defaultAgentsAPIURL for that, and the doc comment
	// on Platform for why these are two separate values rather than one.
	defaultPlatformBaseURL = "https://platform.nyks.dev"
	// defaultAgentsAPIURL is where connect/mining enable actually send
	// register/status/enroll (search-router's own naming: the "Agents"
	// host, distinct from the "Platform" host above). Confirmed live: a
	// register call against platform.nyks.dev 404s — that route only
	// exists on this separate host — while the claim_url it returns
	// correctly points back at platform.nyks.dev. One shared origin was
	// this package's original assumption, matching the design doc's own
	// wording; the real deployment splits it in two, and search-router's
	// skill file is what corrected this.
	defaultAgentsAPIURL        = "https://agents-v1.nyks.dev"
	defaultProviderName        = "openrouter"
	defaultShutdownGrace       = 5 * time.Second
	defaultMaxRequestBodyBytes = 32 << 20
	defaultMaxInboundConns     = 256

	defaultObservationBudget = 48 << 20
	defaultRingBytes         = 256 << 10
	defaultMaxEventBytes     = 1 << 20
	defaultMaxEvents         = 100_000
	defaultMaxObservedBody   = 64 << 20
	defaultMaxDepth          = 32
	defaultMaxJSONKeyBytes   = 256
)

// Listen is a parsed listener specification: loopback TCP (the default) or a
// Unix socket given as unix:///absolute/path.
type Listen struct {
	Network string // "tcp" or "unix"
	Address string // host:port, or the socket path
}

func (l Listen) IsUnix() bool { return l.Network == "unix" }

func (l Listen) String() string {
	if l.IsUnix() {
		return "unix://" + l.Address
	}
	return l.Address
}

// Observe carries the observation bounds. Defined now so the keys are stable;
// consumed from Phase 2 onward.
type Observe struct {
	MemoryBudgetBytes    int64
	RingBytes            int64
	MaxEventBytes        int64
	MaxEvents            int64
	MaxObservedBodyBytes int64
	MaxDepth             int
	MaxJSONKeyBytes      int
}

// RingCount derives the global ring count from the budget. It is derived, not
// configured: two knobs that can disagree about one ceiling is how an earlier
// draft of the design got a bound nobody chose (ADR-0002).
func (o Observe) RingCount() int {
	if o.RingBytes <= 0 {
		return 0
	}
	return int(o.MemoryBudgetBytes / o.RingBytes)
}

// Mining is the AS-facing mining-plane configuration (contract §18–§19).
// Enabled is only the scripted first answer consumed by onboarding; the
// persisted mining decision is runtime authority, while a non-empty ASBaseURL
// says AS work is configured. A proxy without a [mining] block behaves exactly
// as it did before mining integration existed.
type Mining struct {
	Enabled     bool
	ASBaseURL   string
	ChainID     string
	SlotID      uint64
	MetadataTTL time.Duration
	// StateDir holds the installation's secret material (ADR-0008:
	// owner-only files — DPoP key, refresh authorization,
	// participation_secret). Defaults to os.UserConfigDir()/tokendrop/state.
	StateDir string
	// SpoolDir holds durably-written observations awaiting delivery.
	// Separate from StateDir because its contents are evidence, not
	// secrets: it is written on every metered request, and it is the one
	// directory an operator may legitimately inspect or back up.
	// Defaults to StateDir/spool.
	SpoolDir string
	// TargetEpoch pins the reward epoch this installation participates
	// in. It is now an OVERRIDE, not the only source: the AS advertises
	// a current-target endpoint, and the daemon asks it every tick and
	// joins whatever is open.
	//
	// A pointer because absent and zero are different answers — epoch 0
	// is as legal an epoch as slot 0 is a legal slot, and a bare uint64
	// would silently read "the operator said nothing" as "the operator
	// said epoch 0". nil means "ask the AS"; non-nil pins that exact
	// epoch and suppresses the query entirely, which is the escape
	// hatch for an AS too old to advertise the endpoint, and for
	// pinning one target during a demo or a test.
	TargetEpoch *uint64
	// Collector tuning. Zero means "use the collector's own defaults",
	// which are the tested values; these exist so a demo can shorten the
	// idle poll without a rebuild.
	CollectorInterval    time.Duration
	CollectorBaseBackoff time.Duration
	CollectorMaxBackoff  time.Duration
	CollectorMaxAttempts int
	// PayoutAddress is the scripted-install answer to connect/mining
	// enable's terminal question (agent onboarding design §5.5): a
	// pre-decided payout destination, so an install with no terminal
	// can still enable mining without a wallet ever being created here.
	PayoutAddress string
	// PlatformSlot names which platform-advertised mining slot connect
	// should enroll into, required only when the platform ever offers
	// more than one (WP2-review judgment call 1's ruling: one slot
	// offered, take it; more than one, refuse and require this — no
	// automatic AS-audience matching on the client's part).
	PlatformSlot string
}

// Platform is the search platform connect and mining enable talk to
// (agent onboarding design, search-platform-agent-onboarding-design.md).
// Unlike Mining, this has no Enabled gate: both URLs are always
// resolved and validated, and nothing dials either unless connect or
// mining enable is actually invoked.
//
// Two URLs, not one: search-router runs the human-facing portal and
// claim pages on one host and the machine-facing /v1/agents/* API on
// another. BaseURL is the portal — the origin a printed claim_url is
// checked against (invariant 12). AgentsAPIURL is where the actual
// register/status/enroll requests go. A single shared origin was this
// package's original assumption; live testing found the real deployment
// splits it in two, and pkg/platform.Client now takes both rather than
// deriving one from the other.
type Platform struct {
	BaseURL      string
	AgentsAPIURL string
}

// Config is the resolved, immutable configuration.
// Miner is the drop-in miner: no daemon, no proxy. When enabled, the
// `search` command posts straight to the router, records the served
// request id under IntakeDir, and hands the mining-plane work (join,
// capability, spool, submit) to a detached one-shot `flush`. Everything
// under [mining] still applies — the miner is the same participant, run
// at four moments instead of on a ticker.
type Miner struct {
	Enabled bool
	// RouterURL is where searches go. Empty means the [[provider]]
	// upstream, which is the gateway a proxy would have forwarded to.
	RouterURL *url.URL
	// IntakeDir holds one small JSON file per served search until a flush
	// promotes it into the spool under a joined (slot, epoch).
	IntakeDir string
	// SessionsDir holds the per-workspace lineage sidecars the hooks
	// write and `search` reads.
	SessionsDir string
	// FlushInterval is how often a flush repeats the AS round trip
	// (target, join, capability). Intake promotion and spool delivery run
	// on every flush regardless.
	FlushInterval time.Duration
}

type Config struct {
	Listen                Listen
	AdminListen           Listen
	ShutdownGrace         time.Duration
	MaxRequestBodyBytes   int64
	MaxInboundConns       int
	ResponseHeaderTimeout time.Duration // 0 = disabled, deliberately (§5.8)

	ProviderName string
	ProviderTier string
	Upstream     *url.URL

	// UpstreamCAPool, when non-nil, REPLACES the system trust store for
	// upstream TLS — a pinning control (and the E2E fixture path). Replacing
	// rather than appending means the key can only narrow trust, never
	// widen it. Nil means system roots.
	UpstreamCAPool *x509.CertPool
	UpstreamCAFile string

	LogLevel slog.Level

	Observe  Observe
	Mining   Mining
	Miner    Miner
	Platform Platform

	// MiningEnabledExplicit is true when the config file itself wrote
	// `[mining] enabled` (true or false) — distinct from Mining.Enabled
	// being false because the file said nothing about it at all. There
	// is no TOKENDROP_* env override or flag for mining.enabled, so the
	// file is the only source this can come from. connect and mining
	// enable need the distinction: they ask their one interactive
	// question only when this is false (nobody has decided yet); when
	// it's true, whatever the file said is trusted outright, silently,
	// which is what makes a scripted install possible.
	MiningEnabledExplicit bool
}

// fileConfig mirrors the TOML document. Field names follow
// tokendrop-proxy-spec §10 and README.md.
type fileConfig struct {
	Proxy struct {
		Listen              string   `toml:"listen"`
		AdminListen         string   `toml:"admin_listen"`
		ShutdownGrace       duration `toml:"shutdown_grace"`
		MaxRequestBodyBytes int64    `toml:"max_request_body_bytes"`
		MaxInboundConns     int      `toml:"max_inbound_conns"`
	} `toml:"proxy"`
	Provider []struct {
		Name     string `toml:"name"`
		Tier     string `toml:"tier"`
		Upstream string `toml:"upstream"`
	} `toml:"provider"`
	Transport struct {
		ResponseHeaderTimeout duration `toml:"response_header_timeout"`
		UpstreamCAFile        string   `toml:"upstream_ca_file"`
	} `toml:"transport"`
	Privacy struct {
		LogContent bool `toml:"log_content"`
	} `toml:"privacy"`
	Log struct {
		Level string `toml:"level"`
	} `toml:"log"`
	Observe struct {
		MemoryBudgetBytes    int64 `toml:"observation_memory_budget"`
		RingBytes            int64 `toml:"ring_bytes"`
		MaxEventBytes        int64 `toml:"max_event_bytes"`
		MaxEvents            int64 `toml:"max_events"`
		MaxObservedBodyBytes int64 `toml:"max_observed_body_bytes"`
		MaxDepth             int   `toml:"max_depth"`
		MaxJSONKeyBytes      int   `toml:"max_json_key_bytes"`
	} `toml:"observe"`
	Mining struct {
		Enabled              bool     `toml:"enabled"`
		ASURL                string   `toml:"as_url"`
		ChainID              string   `toml:"chain_id"`
		SlotID               *uint64  `toml:"slot_id"`
		TargetEpoch          *uint64  `toml:"target_epoch"`
		MetadataTTL          duration `toml:"metadata_ttl"`
		StateDir             string   `toml:"state_dir"`
		SpoolDir             string   `toml:"spool_dir"`
		CollectorInterval    duration `toml:"collector_interval"`
		CollectorBaseBackoff duration `toml:"collector_base_backoff"`
		CollectorMaxBackoff  duration `toml:"collector_max_backoff"`
		CollectorMaxAttempts int      `toml:"collector_max_attempts"`
		// PayoutAddress answers connect/mining enable's terminal question
		// through configuration, for a scripted or headless install
		// (agent onboarding design §5.5). Read only when Enabled is also
		// explicit in the file — it names an address, not a decision.
		PayoutAddress string `toml:"payout_address"`
		// PlatformSlot names the platform-advertised mining slot to
		// enroll into, when the platform ever offers more than one
		// (WP2-review judgment call 1).
		PlatformSlot string `toml:"platform_slot"`
	} `toml:"mining"`
	Miner struct {
		Enabled       bool     `toml:"enabled"`
		RouterURL     string   `toml:"router_url"`
		IntakeDir     string   `toml:"intake_dir"`
		SessionsDir   string   `toml:"sessions_dir"`
		FlushInterval duration `toml:"flush_interval"`
	} `toml:"miner"`
	Platform struct {
		BaseURL      string `toml:"base_url"`
		AgentsAPIURL string `toml:"agents_api_url"`
	} `toml:"platform"`
}

type duration struct{ time.Duration }

func (d *duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// Load resolves the configuration from args (flags) and getenv. It returns
// showVersion=true when -version was passed. Precedence:
// flags > environment > TOML file > defaults.
func Load(args []string, getenv func(string) string) (cfg *Config, showVersion bool, err error) {
	fs := flag.NewFlagSet("tokendrop-proxy", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		flagConfig      = fs.String("config", "", "path to TOML config file")
		flagListen      = fs.String("listen", "", "listen address (host:port or unix:///path)")
		flagAdminListen = fs.String("admin-listen", "", "admin listen address (host:port or unix:///path)")
		flagLogLevel    = fs.String("log-level", "", "log level: debug|info|warn|error")
		flagVersion     = fs.Bool("version", false, "print version and exit")
	)
	if err := fs.Parse(args); err != nil {
		return nil, false, err
	}
	if fs.NArg() > 0 {
		return nil, false, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *flagVersion {
		return nil, true, nil
	}

	// Defaults.
	raw := rawConfig{
		listen:                DefaultListen,
		adminListen:           DefaultAdminListen,
		shutdownGrace:         defaultShutdownGrace,
		maxRequestBodyBytes:   defaultMaxRequestBodyBytes,
		maxInboundConns:       defaultMaxInboundConns,
		responseHeaderTimeout: 0,
		providerName:          defaultProviderName,
		providerTier:          "A",
		upstream:              defaultUpstream,
		logLevel:              "info",
		platformBaseURL:       defaultPlatformBaseURL,
		observe: Observe{
			MemoryBudgetBytes:    defaultObservationBudget,
			RingBytes:            defaultRingBytes,
			MaxEventBytes:        defaultMaxEventBytes,
			MaxEvents:            defaultMaxEvents,
			MaxObservedBodyBytes: defaultMaxObservedBody,
			MaxDepth:             defaultMaxDepth,
			MaxJSONKeyBytes:      defaultMaxJSONKeyBytes,
		},
	}

	// TOML file: explicit path (flag or env) is required to exist; the
	// conventional ./tokendrop.toml is picked up when present.
	path := *flagConfig
	if path == "" {
		path = getenv("TOKENDROP_CONFIG")
	}
	explicit := path != ""
	if path == "" {
		if _, statErr := os.Stat("tokendrop.toml"); statErr == nil {
			path = "tokendrop.toml"
		}
	}
	if path != "" {
		if err := raw.applyFile(path); err != nil {
			if explicit || !errors.Is(err, os.ErrNotExist) {
				return nil, false, err
			}
		}
	}

	// Environment.
	if v := getenv("TOKENDROP_LISTEN"); v != "" {
		raw.listen = v
	}
	if v := getenv("TOKENDROP_ADMIN_LISTEN"); v != "" {
		raw.adminListen = v
	}
	if v := getenv("TOKENDROP_UPSTREAM"); v != "" {
		raw.upstream = v
	}
	if v := getenv("TOKENDROP_UPSTREAM_CA_FILE"); v != "" {
		raw.upstreamCAFile = v
	}
	if v := getenv("TOKENDROP_LOG_LEVEL"); v != "" {
		raw.logLevel = v
	}

	// Flags.
	if *flagListen != "" {
		raw.listen = *flagListen
	}
	if *flagAdminListen != "" {
		raw.adminListen = *flagAdminListen
	}
	if *flagLogLevel != "" {
		raw.logLevel = *flagLogLevel
	}

	cfg, err = raw.finish()
	if err != nil {
		return nil, false, err
	}
	return cfg, false, nil
}

// rawConfig holds pre-validation values from all sources.
type rawConfig struct {
	listen                string
	adminListen           string
	shutdownGrace         time.Duration
	maxRequestBodyBytes   int64
	maxInboundConns       int
	responseHeaderTimeout time.Duration
	providerName          string
	providerTier          string
	upstream              string
	upstreamCAFile        string
	logLevel              string
	observe               Observe

	miningEnabled              bool
	miningASURL                string
	miningChainID              string
	miningSlotID               *uint64
	miningTargetEpoch          *uint64
	miningTTL                  time.Duration
	miningStateDir             string
	miningSpoolDir             string
	miningCollectorInterval    time.Duration
	miningCollectorBaseBackoff time.Duration
	miningCollectorMaxBackoff  time.Duration
	miningCollectorMaxAttempts int
	miningPayoutAddress        string
	miningPlatformSlot         string
	miningEnabledExplicit      bool

	minerEnabled       bool
	minerRouterURL     string
	minerIntakeDir     string
	minerSessionsDir   string
	minerFlushInterval time.Duration

	platformBaseURL string
	agentsAPIURL    string
}

func (r *rawConfig) applyFile(path string) error {
	data, err := os.ReadFile(path) // #nosec G304 -- the config path is operator-supplied by design
	if err != nil {
		return fmt.Errorf("config file: %w", err)
	}
	var f fileConfig
	md, err := toml.Decode(string(data), &f)
	if err != nil {
		return fmt.Errorf("config file %s: %w", path, err)
	}
	// Unknown keys are typos waiting to be silent; reject them.
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return fmt.Errorf("config file %s: unknown key(s): %s", path, strings.Join(keys, ", "))
	}

	if f.Privacy.LogContent {
		return errors.New("privacy.log_content = true is not a supported configuration: content is never logged, and no override exists")
	}

	if f.Proxy.Listen != "" {
		r.listen = f.Proxy.Listen
	}
	if f.Proxy.AdminListen != "" {
		r.adminListen = f.Proxy.AdminListen
	}
	if f.Proxy.ShutdownGrace.Duration != 0 {
		r.shutdownGrace = f.Proxy.ShutdownGrace.Duration
	}
	if f.Proxy.MaxRequestBodyBytes != 0 {
		r.maxRequestBodyBytes = f.Proxy.MaxRequestBodyBytes
	}
	if f.Proxy.MaxInboundConns != 0 {
		r.maxInboundConns = f.Proxy.MaxInboundConns
	}
	if f.Transport.ResponseHeaderTimeout.Duration != 0 {
		r.responseHeaderTimeout = f.Transport.ResponseHeaderTimeout.Duration
	}
	if f.Transport.UpstreamCAFile != "" {
		r.upstreamCAFile = f.Transport.UpstreamCAFile
	}
	switch len(f.Provider) {
	case 0:
	case 1:
		p := f.Provider[0]
		if p.Name != "" {
			r.providerName = p.Name
		}
		if p.Tier != "" {
			r.providerTier = p.Tier
		}
		if p.Upstream != "" {
			r.upstream = p.Upstream
		}
	default:
		return errors.New("config: exactly one [[provider]] is supported at launch (Tier A)")
	}
	if f.Log.Level != "" {
		r.logLevel = f.Log.Level
	}
	if f.Observe.MemoryBudgetBytes != 0 {
		r.observe.MemoryBudgetBytes = f.Observe.MemoryBudgetBytes
	}
	if f.Observe.RingBytes != 0 {
		r.observe.RingBytes = f.Observe.RingBytes
	}
	if f.Observe.MaxEventBytes != 0 {
		r.observe.MaxEventBytes = f.Observe.MaxEventBytes
	}
	if f.Observe.MaxEvents != 0 {
		r.observe.MaxEvents = f.Observe.MaxEvents
	}
	if f.Observe.MaxObservedBodyBytes != 0 {
		r.observe.MaxObservedBodyBytes = f.Observe.MaxObservedBodyBytes
	}
	if f.Observe.MaxDepth != 0 {
		r.observe.MaxDepth = f.Observe.MaxDepth
	}
	if f.Observe.MaxJSONKeyBytes != 0 {
		r.observe.MaxJSONKeyBytes = f.Observe.MaxJSONKeyBytes
	}
	r.miningEnabled = f.Mining.Enabled
	// IsDefined, not f.Mining.Enabled itself: connect/mining enable need
	// to tell "the file wrote enabled = false" apart from "the file
	// never mentioned [mining] at all", and both decode to Go's zero
	// value the same way. There is no env override for this key (see
	// Config.MiningEnabledExplicit's doc comment), so the file is the
	// only source of the distinction.
	r.miningEnabledExplicit = md.IsDefined("mining", "enabled")
	if f.Mining.PayoutAddress != "" {
		r.miningPayoutAddress = f.Mining.PayoutAddress
	}
	if f.Mining.PlatformSlot != "" {
		r.miningPlatformSlot = f.Mining.PlatformSlot
	}
	if f.Mining.ASURL != "" {
		r.miningASURL = f.Mining.ASURL
	}
	if f.Mining.ChainID != "" {
		r.miningChainID = f.Mining.ChainID
	}
	if f.Mining.SlotID != nil {
		r.miningSlotID = f.Mining.SlotID
	}
	if f.Mining.TargetEpoch != nil {
		r.miningTargetEpoch = f.Mining.TargetEpoch
	}
	if f.Mining.SpoolDir != "" {
		r.miningSpoolDir = f.Mining.SpoolDir
	}
	if f.Mining.CollectorInterval.Duration != 0 {
		r.miningCollectorInterval = f.Mining.CollectorInterval.Duration
	}
	if f.Mining.CollectorBaseBackoff.Duration != 0 {
		r.miningCollectorBaseBackoff = f.Mining.CollectorBaseBackoff.Duration
	}
	if f.Mining.CollectorMaxBackoff.Duration != 0 {
		r.miningCollectorMaxBackoff = f.Mining.CollectorMaxBackoff.Duration
	}
	if f.Mining.CollectorMaxAttempts != 0 {
		r.miningCollectorMaxAttempts = f.Mining.CollectorMaxAttempts
	}
	if f.Mining.MetadataTTL.Duration != 0 {
		r.miningTTL = f.Mining.MetadataTTL.Duration
	}
	if f.Mining.StateDir != "" {
		r.miningStateDir = f.Mining.StateDir
	}
	r.minerEnabled = f.Miner.Enabled
	if f.Miner.RouterURL != "" {
		r.minerRouterURL = f.Miner.RouterURL
	}
	if f.Miner.IntakeDir != "" {
		r.minerIntakeDir = f.Miner.IntakeDir
	}
	if f.Miner.SessionsDir != "" {
		r.minerSessionsDir = f.Miner.SessionsDir
	}
	if f.Miner.FlushInterval.Duration != 0 {
		r.minerFlushInterval = f.Miner.FlushInterval.Duration
	}
	if f.Platform.BaseURL != "" {
		r.platformBaseURL = f.Platform.BaseURL
	}
	if f.Platform.AgentsAPIURL != "" {
		r.agentsAPIURL = f.Platform.AgentsAPIURL
	}
	return nil
}

func (r *rawConfig) finish() (*Config, error) {
	listen, err := parseListen(r.listen)
	if err != nil {
		return nil, fmt.Errorf("proxy.listen: %w", err)
	}
	adminListen, err := parseListen(r.adminListen)
	if err != nil {
		return nil, fmt.Errorf("proxy.admin_listen: %w", err)
	}
	upstream, err := parseUpstream(r.upstream)
	if err != nil {
		return nil, fmt.Errorf("provider.upstream: %w", err)
	}
	level, err := parseLevel(r.logLevel)
	if err != nil {
		return nil, fmt.Errorf("log.level: %w", err)
	}
	if r.providerTier != "A" {
		return nil, fmt.Errorf("provider.tier: only Tier A ships at launch, got %q", r.providerTier)
	}
	if r.providerName == "" {
		return nil, errors.New("provider.name: must not be empty")
	}
	if r.maxRequestBodyBytes <= 0 {
		return nil, errors.New("proxy.max_request_body_bytes: must be positive")
	}
	if r.maxInboundConns < 1 {
		return nil, errors.New("proxy.max_inbound_conns: must be at least 1")
	}
	if r.shutdownGrace < 0 {
		return nil, errors.New("proxy.shutdown_grace: must not be negative")
	}
	if r.responseHeaderTimeout < 0 {
		return nil, errors.New("transport.response_header_timeout: must not be negative")
	}
	if err := validateObserve(r.observe); err != nil {
		return nil, err
	}
	var caPool *x509.CertPool
	if r.upstreamCAFile != "" {
		pem, err := os.ReadFile(r.upstreamCAFile) // #nosec G304 -- operator-supplied path by design
		if err != nil {
			return nil, fmt.Errorf("transport.upstream_ca_file: %w", err)
		}
		caPool = x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("transport.upstream_ca_file: %s contains no usable PEM certificate", r.upstreamCAFile)
		}
	}

	mining, err := r.finishMining()
	if err != nil {
		return nil, err
	}
	miner, err := r.finishMiner(upstream, mining)
	if err != nil {
		return nil, err
	}
	platformBaseURL, err := parsePlatformURL(r.platformBaseURL, "platform.base_url")
	if err != nil {
		return nil, err
	}
	// WP2-review must-fix: agents_api_url used to default unconditionally
	// to the real platform, so a dev/test config naming only a loopback
	// base_url (every existing stub-backed test does exactly this) would
	// silently register against production the moment connect/mining
	// enable actually ran — which is exactly what happened once, live,
	// before this fix. Three cases, only two of which have a safe
	// implicit answer:
	//   - base_url is exactly the real platform's default: agents_api_url
	//     defaults to the real agents API. The ordinary case.
	//   - base_url is loopback: assume the same local stub serves both
	//     roles, matching every existing test's own setup.
	//   - base_url names anything else (a devnet, a staging portal) and
	//     agents_api_url is unset: refused. A custom non-loopback base_url
	//     gives no safe signal about which API host it pairs with —
	//     defaulting to production here is exactly the footgun this whole
	//     fix exists to close, just for a devnet instead of a laptop.
	agentsAPIRaw := r.agentsAPIURL
	if agentsAPIRaw == "" {
		switch platformBaseURL {
		case defaultPlatformBaseURL:
			agentsAPIRaw = defaultAgentsAPIURL
		default:
			pu, perr := url.Parse(platformBaseURL)
			if perr != nil || !isLoopbackHost(pu.Hostname()) {
				return nil, errors.New("platform.agents_api_url: required when platform.base_url names anything other " +
					"than the default platform — a devnet or staging deployment must name both")
			}
			agentsAPIRaw = platformBaseURL
		}
	}
	agentsAPIURL, err := parsePlatformURL(agentsAPIRaw, "platform.agents_api_url")
	if err != nil {
		return nil, err
	}

	return &Config{
		Listen:                listen,
		AdminListen:           adminListen,
		ShutdownGrace:         r.shutdownGrace,
		MaxRequestBodyBytes:   r.maxRequestBodyBytes,
		MaxInboundConns:       r.maxInboundConns,
		ResponseHeaderTimeout: r.responseHeaderTimeout,
		ProviderName:          r.providerName,
		ProviderTier:          r.providerTier,
		Upstream:              upstream,
		UpstreamCAPool:        caPool,
		UpstreamCAFile:        r.upstreamCAFile,
		LogLevel:              level,
		Observe:               r.observe,
		Mining:                mining,
		Miner:                 miner,
		Platform:              Platform{BaseURL: platformBaseURL, AgentsAPIURL: agentsAPIURL},
		MiningEnabledExplicit: r.miningEnabledExplicit,
	}, nil
}

// parsePlatformURL validates a [platform] URL (base_url or
// agents_api_url) the same way mining.as_url and miner.router_url are
// (invariant 5): https, or http only for loopback — connect and mining
// enable send the platform-issued sr- key in Authorization to
// agents_api_url (base_url never carries a credential, only a printed
// claim_url is checked against it, but the same posture costs nothing
// to hold for both). Unlike as_url, there is no Enabled gate: both
// always resolve, since nothing dials either unless connect or mining
// enable actually runs, and the defaults are always valid https URLs.
func parsePlatformURL(raw, field string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("%s: must be an absolute http(s) URL", field)
	}
	if u.User != nil {
		return "", fmt.Errorf("%s: must not carry userinfo", field)
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return "", fmt.Errorf("%s: plain http is permitted only for loopback hosts, got %q — "+
			"use https, or a loopback address for local development", field, u.Host)
	}
	return raw, nil
}

// finishMiner resolves the [miner] block. Its directories default beside
// the mining state so one HOME-relative tree holds everything the
// participant owns; the router defaults to the provider upstream so an
// existing proxy config becomes a miner config by adding two lines.
func (r *rawConfig) finishMiner(upstream *url.URL, mining Mining) (Miner, error) {
	m := Miner{
		Enabled:       r.minerEnabled,
		IntakeDir:     r.minerIntakeDir,
		SessionsDir:   r.minerSessionsDir,
		FlushInterval: r.minerFlushInterval,
	}
	if m.FlushInterval == 0 {
		m.FlushInterval = 3 * time.Minute
	}
	base := ""
	if mining.StateDir != "" {
		base = filepath.Dir(mining.StateDir)
	}
	if m.IntakeDir == "" && base != "" {
		m.IntakeDir = filepath.Join(base, "intake")
	}
	if m.SessionsDir == "" && base != "" {
		m.SessionsDir = filepath.Join(base, "sessions")
	}
	if r.minerRouterURL != "" {
		u, err := url.Parse(r.minerRouterURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return Miner{}, errors.New("miner.router_url: must be an absolute http(s) URL")
		}
		if u.User != nil {
			return Miner{}, errors.New("miner.router_url: must not carry userinfo")
		}
		// as_url's rule, unchanged in substance: every search sends the
		// participant's sr- key in Authorization, so a routable plain-http
		// router is the same cleartext-credential exposure a routable
		// plain-http AS would be. The loopback carve-out is deliberate,
		// for local development without TLS; testing against a ROUTABLE
		// devnet router needs TLS on the devnet, or the client co-located
		// over loopback — that is the intended consequence, not a gap.
		if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
			return Miner{}, fmt.Errorf("miner.router_url: plain http is permitted only for loopback hosts, got %q — "+
				"use https, or a loopback address for local development", u.Host)
		}
		m.RouterURL = u
	} else {
		m.RouterURL = upstream
	}
	if !m.Enabled {
		return m, nil
	}
	if m.FlushInterval < 0 {
		return Miner{}, errors.New("miner.flush_interval: must not be negative")
	}
	if m.IntakeDir == "" || m.SessionsDir == "" {
		return Miner{}, errors.New("miner: intake_dir and sessions_dir are required when no mining.state_dir can be derived")
	}
	if mining.ASBaseURL == "" {
		return Miner{}, errors.New("miner.enabled = true needs a [mining] block naming an AS: the miner is that participant without the daemon")
	}
	return m, nil
}

// finishMining validates the mining block only when it is enabled: a
// disabled block never blocks proxy startup, whatever it contains —
// inference must not depend on mining configuration being right.
func (r *rawConfig) finishMining() (Mining, error) {
	m := Mining{
		Enabled:              r.miningEnabled,
		ASBaseURL:            r.miningASURL,
		ChainID:              r.miningChainID,
		MetadataTTL:          r.miningTTL,
		CollectorInterval:    r.miningCollectorInterval,
		CollectorBaseBackoff: r.miningCollectorBaseBackoff,
		CollectorMaxBackoff:  r.miningCollectorMaxBackoff,
		CollectorMaxAttempts: r.miningCollectorMaxAttempts,
		PayoutAddress:        r.miningPayoutAddress,
		PlatformSlot:         r.miningPlatformSlot,
	}
	if r.miningSlotID != nil {
		m.SlotID = *r.miningSlotID
	}
	// Copied, not aliased: the resolved configuration is immutable, and
	// sharing the pointer with rawConfig would leave a writable path
	// into it.
	if r.miningTargetEpoch != nil {
		pinned := *r.miningTargetEpoch
		m.TargetEpoch = &pinned
	}
	if m.MetadataTTL == 0 {
		m.MetadataTTL = 15 * time.Minute
	}
	m.StateDir = r.miningStateDir
	if m.StateDir == "" {
		if base, err := os.UserConfigDir(); err == nil {
			m.StateDir = filepath.Join(base, "tokendrop", "state")
		}
	}
	m.SpoolDir = r.miningSpoolDir
	if m.SpoolDir == "" && m.StateDir != "" {
		m.SpoolDir = filepath.Join(m.StateDir, "spool")
	}
	if !m.Enabled {
		return m, nil
	}
	if m.ASBaseURL == "" {
		return Mining{}, errors.New("mining.as_url: required when mining is enabled")
	}
	u, err := url.Parse(m.ASBaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return Mining{}, errors.New("mining.as_url: must be an absolute http(s) URL")
	}
	if u.User != nil {
		return Mining{}, errors.New("mining.as_url: must not carry userinfo (it would surface in Authorization headers and error strings)")
	}
	// §18, and rejected HERE rather than at first use. auth.NewDiscoverer
	// applies the same rule and keeps applying it — this is defense in depth,
	// not a move — but a config that loads and then fails on the first AS call
	// contradicts this package's own contract: unsafe states are rejected at
	// load, not warned about later. Every token, DPoP proof and observation
	// this client sends to the AS would otherwise cross the wire in the clear
	// on a routable plain-http host.
	//
	// The loopback carve-out is deliberate and is what makes local development
	// possible without TLS. It also means testing against a ROUTABLE devnet
	// requires TLS on the devnet, or the client co-located over loopback;
	// that is the intended consequence, not a gap.
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return Mining{}, fmt.Errorf("mining.as_url: plain http is permitted only for loopback hosts (§18), got %q — "+
			"use https, or a loopback address for local development", u.Host)
	}
	if m.ChainID == "" {
		return Mining{}, errors.New("mining.chain_id: required when mining is enabled")
	}
	if r.miningSlotID == nil {
		return Mining{}, errors.New("mining.slot_id: required when mining is enabled (one AS serves exactly one Core Slot)")
	}
	if m.MetadataTTL < 0 {
		return Mining{}, errors.New("mining.metadata_ttl: must not be negative")
	}
	// StateDir can still be empty here: it is only defaulted when
	// os.UserConfigDir() succeeds, and that error was swallowed above.
	// Catching it at config time turns a confusing runtime failure deep
	// in the key store into a startup message naming the actual fix.
	if m.StateDir == "" {
		return Mining{}, errors.New("mining.state_dir: required when mining is enabled (no user config directory could be determined for the default)")
	}
	if m.CollectorInterval < 0 || m.CollectorBaseBackoff < 0 || m.CollectorMaxBackoff < 0 {
		return Mining{}, errors.New("mining: collector durations must not be negative")
	}
	if m.CollectorMaxAttempts < 0 {
		return Mining{}, errors.New("mining.collector_max_attempts: must not be negative (0 means unlimited)")
	}
	return m, nil
}

func validateObserve(o Observe) error {
	switch {
	case o.RingBytes < 4096:
		return errors.New("observe.ring_bytes: must be at least 4096")
	case o.MemoryBudgetBytes < o.RingBytes:
		return errors.New("observe.observation_memory_budget: must be at least one ring_bytes")
	case o.MaxEventBytes <= 0:
		return errors.New("observe.max_event_bytes: must be positive")
	case o.MaxEvents <= 0:
		return errors.New("observe.max_events: must be positive")
	case o.MaxObservedBodyBytes <= 0:
		return errors.New("observe.max_observed_body_bytes: must be positive")
	case o.MaxDepth < 1:
		return errors.New("observe.max_depth: must be at least 1")
	case o.MaxJSONKeyBytes < 8:
		return errors.New("observe.max_json_key_bytes: must be at least 8")
	}
	return nil
}

// parseListen accepts "host:port" (which must be loopback — there is
// deliberately no override flag; a remote mode would be a separate,
// authenticated feature) or "unix:///absolute/path".
func parseListen(s string) (Listen, error) {
	if s == "" {
		return Listen{}, errors.New("must not be empty")
	}
	if path, ok := strings.CutPrefix(s, "unix://"); ok {
		if !filepath.IsAbs(path) {
			return Listen{}, fmt.Errorf("socket path must be absolute, got %q", path)
		}
		return Listen{Network: "unix", Address: path}, nil
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return Listen{}, fmt.Errorf("invalid address %q: %w", s, err)
	}
	if port == "" {
		return Listen{}, fmt.Errorf("missing port in %q", s)
	}
	if !isLoopbackHost(host) {
		return Listen{}, fmt.Errorf("%q is not loopback: the TCP listener must bind loopback only, and no override flag exists (use a unix:// socket or keep 127.0.0.1)", s)
	}
	return Listen{Network: "tcp", Address: s}, nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// parseUpstream validates the upstream as an origin plus fixed base path — not
// a general URL. A credential smuggled in as https://user:pass@host would
// become an Authorization header the proxy generates; a query or fragment has
// no business in a base; and anything but https would put the bearer
// credential on the wire in the clear.
func parseUpstream(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("invalid URL %q: %w", s, err)
	}
	switch {
	case u.Scheme != "https":
		return nil, fmt.Errorf("scheme must be https, got %q", u.Scheme)
	case u.User != nil:
		return nil, errors.New("must not carry userinfo")
	case u.RawQuery != "":
		return nil, errors.New("must not carry a query")
	case u.Fragment != "" || u.RawFragment != "":
		return nil, errors.New("must not carry a fragment")
	case u.Host == "":
		return nil, errors.New("missing host")
	case u.RawPath != "":
		return nil, errors.New("path must not be percent-encoded")
	case strings.HasSuffix(u.Path, "/"):
		return nil, errors.New("base path must not end with a slash")
	case u.Opaque != "":
		return nil, errors.New("must be an absolute URL with a host")
	}
	return u, nil
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown level %q (want debug|info|warn|error)", s)
	}
}
