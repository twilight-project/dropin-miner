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
	DefaultListen              = "127.0.0.1:8787"
	DefaultAdminListen         = "127.0.0.1:8788"
	defaultUpstream            = "https://openrouter.ai/api"
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
// Disabled by default: a proxy without a [mining] block behaves exactly
// as it did before the mining integration existed, and every mining
// failure mode is invisible to inference by construction.
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

	Observe Observe
	Mining  Mining
	Miner   Miner
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
	} `toml:"mining"`
	Miner struct {
		Enabled       bool     `toml:"enabled"`
		RouterURL     string   `toml:"router_url"`
		IntakeDir     string   `toml:"intake_dir"`
		SessionsDir   string   `toml:"sessions_dir"`
		FlushInterval duration `toml:"flush_interval"`
	} `toml:"miner"`
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

	minerEnabled       bool
	minerRouterURL     string
	minerIntakeDir     string
	minerSessionsDir   string
	minerFlushInterval time.Duration
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
	}, nil
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
	if !mining.Enabled {
		return Miner{}, errors.New("miner.enabled = true needs a [mining] block: the miner is that participant without the daemon")
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
