package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func noEnv(string) string { return "" }

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// upstreamEnv builds the env lookup without a "TOKEN…" map-literal key, which
// gosec's G101 heuristic would misread as a hardcoded credential.
func upstreamEnv(u string) func(string) string {
	key := strings.Join([]string{"TOKENDROP", "UPSTREAM"}, "_")
	return func(k string) string {
		if k == key {
			return u
		}
		return ""
	}
}

func writeTOML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cfg.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func load(t *testing.T, args []string, getenv func(string) string) *Config {
	t.Helper()
	cfg, showVersion, err := Load(args, getenv)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if showVersion {
		t.Fatal("unexpected showVersion")
	}
	return cfg
}

func loadErr(t *testing.T, args []string, getenv func(string) string) error {
	t.Helper()
	_, _, err := Load(args, getenv)
	if err == nil {
		t.Fatal("expected error, got none")
	}
	return err
}

func TestDefaults(t *testing.T) {
	cfg := load(t, nil, noEnv)
	if cfg.Listen.String() != "127.0.0.1:8787" || cfg.Listen.IsUnix() {
		t.Errorf("listen default: %+v", cfg.Listen)
	}
	if cfg.AdminListen.String() != "127.0.0.1:8788" {
		t.Errorf("admin default: %+v", cfg.AdminListen)
	}
	if cfg.Upstream.String() != "https://openrouter.ai/api" {
		t.Errorf("upstream default: %s", cfg.Upstream)
	}
	if cfg.ProviderName != "openrouter" || cfg.ProviderTier != "A" {
		t.Errorf("provider default: %s/%s", cfg.ProviderName, cfg.ProviderTier)
	}
	if cfg.ShutdownGrace != 5*time.Second {
		t.Errorf("grace default: %v", cfg.ShutdownGrace)
	}
	if cfg.MaxRequestBodyBytes != 32<<20 || cfg.MaxInboundConns != 256 {
		t.Errorf("caps: %d %d", cfg.MaxRequestBodyBytes, cfg.MaxInboundConns)
	}
	if cfg.ResponseHeaderTimeout != 0 {
		t.Errorf("ResponseHeaderTimeout must default to 0 (disabled), got %v", cfg.ResponseHeaderTimeout)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("level default: %v", cfg.LogLevel)
	}
	o := cfg.Observe
	if o.MemoryBudgetBytes != 48<<20 || o.RingBytes != 256<<10 || o.RingCount() != 192 {
		t.Errorf("observe defaults: %+v ringCount=%d", o, o.RingCount())
	}
	if o.MaxEventBytes != 1<<20 || o.MaxEvents != 100_000 || o.MaxObservedBodyBytes != 64<<20 ||
		o.MaxDepth != 32 || o.MaxJSONKeyBytes != 256 {
		t.Errorf("observe defaults: %+v", o)
	}
}

func TestPrecedenceFileEnvFlag(t *testing.T) {
	path := writeTOML(t, `
[proxy]
listen = "127.0.0.1:1111"
admin_listen = "127.0.0.1:2222"

[log]
level = "warn"
`)
	env := envOf(map[string]string{
		"TOKENDROP_LISTEN":    "127.0.0.1:3333",
		"TOKENDROP_LOG_LEVEL": "error",
	})

	// File over defaults; env over file; flag over env.
	cfg := load(t, []string{"-config", path, "-listen", "127.0.0.1:4444"}, env)
	if cfg.Listen.Address != "127.0.0.1:4444" {
		t.Errorf("flag should win: %v", cfg.Listen)
	}
	if cfg.AdminListen.Address != "127.0.0.1:2222" {
		t.Errorf("file should apply: %v", cfg.AdminListen)
	}
	if cfg.LogLevel != slog.LevelError {
		t.Errorf("env should beat file: %v", cfg.LogLevel)
	}
}

func TestUnknownTOMLKeyRejected(t *testing.T) {
	path := writeTOML(t, "[proxy]\nlisten = \"127.0.0.1:8787\"\nbogus_key = \"typo\"\n")
	err := loadErr(t, []string{"-config", path}, noEnv)
	if !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("want unknown-key error, got: %v", err)
	}
}

func TestExplicitConfigMustExist(t *testing.T) {
	_ = loadErr(t, []string{"-config", filepath.Join(t.TempDir(), "absent.toml")}, noEnv)
}

func TestNonLoopbackRejected(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8787", "192.168.1.5:8787", "[2001:db8::1]:8787", "example.com:8787"} {
		err := loadErr(t, []string{"-listen", addr}, noEnv)
		if !strings.Contains(err.Error(), "loopback") {
			t.Errorf("%s: want loopback error, got: %v", addr, err)
		}
	}
	// Empty host binds all interfaces — also rejected.
	_ = loadErr(t, []string{"-listen", ":8787"}, noEnv)
	// Admin listener gets the same rule.
	_ = loadErr(t, []string{"-admin-listen", "0.0.0.0:9999"}, noEnv)
}

func TestLoopbackAccepted(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8787", "[::1]:8787", "localhost:8787", "127.0.0.2:8787"} {
		cfg := load(t, []string{"-listen", addr}, noEnv)
		if cfg.Listen.Network != "tcp" {
			t.Errorf("%s: %+v", addr, cfg.Listen)
		}
	}
}

func TestUnixSocketListen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets are a POSIX listener; the miner never listens")
	}
	cfg := load(t, []string{"-listen", "unix:///tmp/td/proxy.sock"}, noEnv)
	if !cfg.Listen.IsUnix() || cfg.Listen.Address != "/tmp/td/proxy.sock" {
		t.Errorf("socket: %+v", cfg.Listen)
	}
	_ = loadErr(t, []string{"-listen", "unix://relative/path.sock"}, noEnv)
}

func TestLogContentRejected(t *testing.T) {
	path := writeTOML(t, "[privacy]\nlog_content = true\n")
	err := loadErr(t, []string{"-config", path}, noEnv)
	if !strings.Contains(err.Error(), "log_content") {
		t.Errorf("want log_content rejection, got: %v", err)
	}
	// false is accepted (the key exists to be rejected when true).
	path = writeTOML(t, "[privacy]\nlog_content = false\n")
	load(t, []string{"-config", path}, noEnv)
}

func TestUpstreamValidation(t *testing.T) {
	bad := map[string]string{
		"http://openrouter.ai/api":        "https",
		"https://user:pw@openrouter.ai/x": "userinfo",
		"https://openrouter.ai/api?x=1":   "query",
		"https://openrouter.ai/api#frag":  "fragment",
		"https://openrouter.ai/api/":      "slash",
		"https://openrouter.ai/a%2Fpi":    "percent-encoded",
		"https://":                        "host",
	}
	for upstream, wantSubstr := range bad {
		err := loadErr(t, nil, upstreamEnv(upstream))
		if !strings.Contains(err.Error(), wantSubstr) {
			t.Errorf("%s: want %q in error, got: %v", upstream, wantSubstr, err)
		}
	}
	cfg := load(t, nil, upstreamEnv("https://127.0.0.1:4443/api"))
	if cfg.Upstream.Host != "127.0.0.1:4443" {
		t.Errorf("upstream: %s", cfg.Upstream)
	}
}

func TestProviderRules(t *testing.T) {
	// Two providers: rejected at launch.
	path := writeTOML(t, `
[[provider]]
name = "openrouter"
tier = "A"
upstream = "https://openrouter.ai/api"

[[provider]]
name = "ollama"
tier = "C"
upstream = "https://127.0.0.1:11434/v1"
`)
	_ = loadErr(t, []string{"-config", path}, noEnv)

	// Non-A tier: rejected at launch.
	path = writeTOML(t, "[[provider]]\nname = \"together\"\ntier = \"B\"\nupstream = \"https://api.together.xyz/v1\"\n")
	err := loadErr(t, []string{"-config", path}, noEnv)
	if !strings.Contains(err.Error(), "Tier A") {
		t.Errorf("want tier error, got: %v", err)
	}
}

func TestObserveValidation(t *testing.T) {
	path := writeTOML(t, "[observe]\nobservation_memory_budget = 1024\nring_bytes = 4096\n")
	_ = loadErr(t, []string{"-config", path}, noEnv) // budget < one ring

	path = writeTOML(t, "[observe]\nobservation_memory_budget = 1048576\nring_bytes = 65536\n")
	cfg := load(t, []string{"-config", path}, noEnv)
	if cfg.Observe.RingCount() != 16 {
		t.Errorf("ring count: %d", cfg.Observe.RingCount())
	}
}

func TestUpstreamCAFile(t *testing.T) {
	// Missing file: rejected at startup.
	path := writeTOML(t, "[transport]\nupstream_ca_file = \"/does/not/exist.pem\"\n")
	_ = loadErr(t, []string{"-config", path}, noEnv)

	// Present but not a certificate: rejected at startup.
	junk := filepath.Join(t.TempDir(), "junk.pem")
	if err := os.WriteFile(junk, []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	path = writeTOML(t, "[transport]\nupstream_ca_file = \""+filepath.ToSlash(junk)+"\"\n")
	err := loadErr(t, []string{"-config", path}, noEnv)
	if !strings.Contains(err.Error(), "PEM") {
		t.Errorf("want PEM error, got: %v", err)
	}

	// Unset: nil pool (system roots).
	cfg := load(t, nil, noEnv)
	if cfg.UpstreamCAPool != nil {
		t.Error("default must use system roots (nil pool)")
	}
}

func TestDurationParsing(t *testing.T) {
	path := writeTOML(t, "[proxy]\nshutdown_grace = \"11s\"\n\n[transport]\nresponse_header_timeout = \"2m\"\n")
	cfg := load(t, []string{"-config", path}, noEnv)
	if cfg.ShutdownGrace != 11*time.Second || cfg.ResponseHeaderTimeout != 2*time.Minute {
		t.Errorf("durations: %v %v", cfg.ShutdownGrace, cfg.ResponseHeaderTimeout)
	}
}

func TestVersionFlag(t *testing.T) {
	_, showVersion, err := Load([]string{"-version"}, noEnv)
	if err != nil || !showVersion {
		t.Fatalf("version flag: %v %v", showVersion, err)
	}
}

func TestUnexpectedArgRejected(t *testing.T) {
	_ = loadErr(t, []string{"serve"}, noEnv)
}

func TestLevelParsing(t *testing.T) {
	cfg := load(t, []string{"-log-level", "debug"}, noEnv)
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("level: %v", cfg.LogLevel)
	}
	_ = loadErr(t, []string{"-log-level", "verbose"}, noEnv)
}

func TestMiningDisabledByDefault(t *testing.T) {
	cfg := load(t, nil, noEnv)
	if cfg.Mining.Enabled {
		t.Fatal("mining must be disabled with no [mining] block")
	}
	// A present-but-disabled block never blocks startup, even when
	// incomplete: inference must not depend on mining config being right.
	path := writeTOML(t, "[mining]\nenabled = false\nchain_id = \"twilight-1\"\n")
	cfg = load(t, []string{"-config", path}, noEnv)
	if cfg.Mining.Enabled || cfg.Mining.ChainID != "twilight-1" {
		t.Fatalf("disabled block mishandled: %+v", cfg.Mining)
	}
}

func TestMiningASConfigurationValidationIsIndependentOfDecision(t *testing.T) {
	valid := func(enabled string, slot string) string {
		return fmt.Sprintf("[mining]\n%sas_url = \"https://as.example.com\"\nchain_id = \"twilight-1\"\n%smetadata_ttl = \"2m\"\ncollector_interval = \"3s\"\ncollector_base_backoff = \"1s\"\ncollector_max_backoff = \"10s\"\ncollector_max_attempts = 4\n", enabled, slot)
	}

	for _, tc := range []struct {
		name         string
		body         string
		wantEnabled  bool
		wantSlot     uint64
		wantExplicit bool
	}{
		{name: "enabled false still validates AS", body: valid("enabled = false\n", "slot_id = 7\n"), wantSlot: 7, wantExplicit: true},
		{name: "absent enabled still validates AS", body: valid("", "slot_id = 7\n"), wantSlot: 7},
		{name: "explicit slot zero succeeds", body: valid("enabled = false\n", "slot_id = 0\n"), wantSlot: 0, wantExplicit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := load(t, []string{"-config", writeTOML(t, tc.body)}, noEnv)
			if cfg.Mining.Enabled != tc.wantEnabled || cfg.Mining.SlotID != tc.wantSlot || cfg.MiningEnabledExplicit != tc.wantExplicit {
				t.Fatalf("resolved config = %+v explicit=%v", cfg.Mining, cfg.MiningEnabledExplicit)
			}
			if cfg.Mining.ASBaseURL == "" || cfg.Mining.ChainID != "twilight-1" || cfg.Mining.MetadataTTL != 2*time.Minute ||
				cfg.Mining.CollectorInterval != 3*time.Second || cfg.Mining.CollectorBaseBackoff != time.Second ||
				cfg.Mining.CollectorMaxBackoff != 10*time.Second || cfg.Mining.CollectorMaxAttempts != 4 {
				t.Fatalf("AS configuration was not fully resolved: %+v", cfg.Mining)
			}
		})
	}

	for name, body := range map[string]string{
		"omitted slot_id":  valid("enabled = false\n", ""),
		"missing chain_id": "[mining]\nenabled = false\nas_url = \"https://as.example.com\"\nslot_id = 7\n",
		"relative as_url":  "[mining]\nenabled = false\nas_url = \"as.example.com\"\nchain_id = \"twilight-1\"\nslot_id = 7\n",
	} {
		if err := loadErr(t, []string{"-config", writeTOML(t, body)}, noEnv); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}

	// Mining.Enabled is only a first-decision answer. It is representable
	// without an AS and must not fail merely because AS work is absent.
	local := load(t, []string{"-config", writeTOML(t, "[mining]\nenabled = true\n")}, noEnv)
	if !local.Mining.Enabled || local.Mining.ASBaseURL != "" {
		t.Fatalf("enabled local mining config was rejected or changed: %+v", local.Mining)
	}
}

// target_epoch is an override, not a requirement: absent means the
// daemon asks the AS which target is open. Absent and zero are
// different answers — epoch 0 is as legal as slot 0 — so the resolved
// value has to keep them apart.
func TestMiningTargetEpochIsAnOptionalOverride(t *testing.T) {
	unset := "[mining]\nenabled = true\nas_url = \"https://as.example.com\"\nchain_id = \"twilight-1\"\nslot_id = 7\n"
	cfg := load(t, []string{"-config", writeTOML(t, unset)}, noEnv)
	if !cfg.Mining.Enabled {
		t.Fatal("mining without target_epoch must still enable: the AS is the epoch source")
	}
	if cfg.Mining.TargetEpoch != nil {
		t.Fatalf("absent target_epoch must resolve to nil, got %d", *cfg.Mining.TargetEpoch)
	}

	pinnedZero := "[mining]\nenabled = true\nas_url = \"https://as.example.com\"\nchain_id = \"twilight-1\"\nslot_id = 7\ntarget_epoch = 0\n"
	cfg = load(t, []string{"-config", writeTOML(t, pinnedZero)}, noEnv)
	if cfg.Mining.TargetEpoch == nil || *cfg.Mining.TargetEpoch != 0 {
		t.Fatalf("target_epoch = 0 must pin epoch 0, not read as unset: %+v", cfg.Mining.TargetEpoch)
	}
}

// §18: plain http reaches the AS only on loopback, and the refusal happens at
// LOAD.
//
// The discoverer applies the same rule, so this is defense in depth rather
// than the only guard — but a config that loads and then fails on the first
// AS call contradicts this package's stated contract, and it hands an
// operator a working-looking setup that mines nothing. Every token, DPoP
// proof and observation would cross a routable plain-http hop in the clear.
func TestMiningRejectsRoutablePlainHTTPASURL(t *testing.T) {
	for _, host := range []string{"as.example.com", "203.0.113.10", "as.internal:8080"} {
		body := "[mining]\nenabled = true\nas_url = \"http://" + host + "\"\nchain_id = \"twilight-1\"\nslot_id = 7\ntarget_epoch = 42\n"
		err := loadErr(t, []string{"-config", writeTOML(t, body)}, noEnv)
		if err == nil {
			t.Errorf("plain http as_url %q was accepted at load", host)
			continue
		}
		if !strings.Contains(err.Error(), "loopback") {
			t.Errorf("as_url %q: the refusal does not name the rule: %v", host, err)
		}
	}
}

// The carve-out that makes local development work without TLS. Losing it
// would be as much a defect as losing the rule — it is what a developer runs
// against a local AS all day.
func TestMiningAllowsPlainHTTPOnLoopback(t *testing.T) {
	for _, host := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"} {
		body := "[mining]\nenabled = true\nas_url = \"http://" + host + "\"\nchain_id = \"twilight-1\"\nslot_id = 7\ntarget_epoch = 42\n"
		if _, _, err := Load([]string{"-config", writeTOML(t, body)}, noEnv); err != nil {
			t.Errorf("loopback as_url %q was refused: %v", host, err)
		}
	}
}

// https is unaffected on any host.
func TestMiningAllowsHTTPSAnywhere(t *testing.T) {
	body := "[mining]\nenabled = true\nas_url = \"https://as.example.com\"\nchain_id = \"twilight-1\"\nslot_id = 7\ntarget_epoch = 42\n"
	if _, _, err := Load([]string{"-config", writeTOML(t, body)}, noEnv); err != nil {
		t.Fatalf("https as_url was refused: %v", err)
	}
}

// The router_url sibling of the as_url rule above: every search sends the
// participant's sr- key in Authorization to router_url, so a routable
// plain-http router is the same cleartext-credential exposure a routable
// plain-http AS would be. This was the "known gap" AGENTS.md invariant 5
// stated honestly (as_url fixed, router_url not yet); closing it here is
// what makes that invariant true rather than aspirational.
//
// Inverted from the review's TestA7_RouterURLAcceptsCleartextHTTP, which
// demonstrated the leak (asserted http:// was accepted) and the asymmetry
// against parseUpstream, which already refused the identical host. Both
// checks are kept: the refusal, and that the asymmetry the repro found is
// now closed.
func TestMinerRejectsRoutablePlainHTTPRouterURL(t *testing.T) {
	for _, host := range []string{"router.example.com", "203.0.113.10", "router.internal:8080"} {
		body := "[miner]\nrouter_url = \"http://" + host + "\"\n"
		err := loadErr(t, []string{"-config", writeTOML(t, body)}, noEnv)
		if err == nil {
			t.Errorf("plain http router_url %q was accepted at load", host)
			continue
		}
		if !strings.Contains(err.Error(), "loopback") {
			t.Errorf("router_url %q: the refusal does not name the rule: %v", host, err)
		}
	}
	if _, err := parseUpstream("http://router.attacker.example"); err == nil {
		t.Fatal("parseUpstream unexpectedly accepted http:// — the asymmetry the repro found is gone")
	}
}

// The carve-out that makes local development work without TLS — losing it
// would be as much a defect as losing the rule. Testing against a ROUTABLE
// devnet router still needs TLS on the devnet, or the client co-located
// over loopback: the intended consequence, not a gap.
func TestMinerAllowsPlainHTTPRouterURLOnLoopback(t *testing.T) {
	for _, host := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"} {
		body := "[miner]\nrouter_url = \"http://" + host + "\"\n"
		if _, _, err := Load([]string{"-config", writeTOML(t, body)}, noEnv); err != nil {
			t.Errorf("loopback router_url %q was refused: %v", host, err)
		}
	}
}

// [miner] enabled = true used to require [mining] enabled = true too — a
// real bug the installer rewrite's own end-to-end test found: setup.sh
// always writes [miner] enabled = true (router intake is configured
// unconditionally) but no longer writes [mining] enabled = true
// unconditionally — that is the mining decision now, made by connect, not
// the installer. What [miner] actually needs is an AS to eventually talk
// to, not that mining is currently on.
func TestMinerEnabledDoesNotRequireMiningEnabled(t *testing.T) {
	// filepath.ToSlash: a raw t.TempDir() on Windows is C:\Users\..., and
	// TOML's basic-string escaping reads an un-slashed backslash as the
	// start of an escape sequence (\U needs eight hex digits) — the same
	// rule TestUpstreamCAFile above already works around.
	body := "[mining]\nas_url = \"https://as.example.com\"\nchain_id = \"twilight-1\"\nslot_id = 7\nstate_dir = \"" + filepath.ToSlash(t.TempDir()) + "\"\n\n" +
		"[miner]\nenabled = true\nintake_dir = \"" + filepath.ToSlash(t.TempDir()) + "\"\nsessions_dir = \"" + filepath.ToSlash(t.TempDir()) + "\"\n"
	if _, _, err := Load([]string{"-config", writeTOML(t, body)}, noEnv); err != nil {
		t.Fatalf("[miner] enabled = true with [mining] enabled unset (only as_url given) was refused: %v", err)
	}
}

// The genuine gap [miner] enabled = true still refuses: no [mining] block
// worth anything at all (no AS named) — a hand-edited config could reach
// this; the installer, which always names an AS, cannot.
func TestMinerEnabledStillRequiresAnASNamed(t *testing.T) {
	body := "[miner]\nenabled = true\nintake_dir = \"" + filepath.ToSlash(t.TempDir()) + "\"\nsessions_dir = \"" + filepath.ToSlash(t.TempDir()) + "\"\n"
	err := loadErr(t, []string{"-config", writeTOML(t, body)}, noEnv)
	if err == nil {
		t.Fatal("[miner] enabled = true with no [mining] as_url at all was accepted")
	}
	if !strings.Contains(err.Error(), "needs a [mining] block") {
		t.Errorf("the refusal does not name the rule: %v", err)
	}
}

func TestMiningRejectsUserinfoURL(t *testing.T) {
	body := "[mining]\nenabled = true\nas_url = \"https://token@as.example.com\"\nchain_id = \"twilight-1\"\nslot_id = 7\ntarget_epoch = 42\n"
	if err := loadErr(t, []string{"-config", writeTOML(t, body)}, noEnv); !strings.Contains(err.Error(), "userinfo") {
		t.Fatalf("userinfo as_url not rejected: %v", err)
	}
}

// platform.base_url defaults to the real platform, with no file at all.
func TestPlatformBaseURLDefaults(t *testing.T) {
	cfg := load(t, nil, noEnv)
	if cfg.Platform.BaseURL != "https://platform.nyks.dev" {
		t.Fatalf("got %q, want the default", cfg.Platform.BaseURL)
	}
}

// platform.base_url is the as_url/router_url rule again: connect and
// mining enable send the platform-issued sr- key in Authorization to it.
func TestPlatformBaseURLRejectsRoutablePlainHTTP(t *testing.T) {
	for _, host := range []string{"platform.example.com", "203.0.113.10", "platform.internal:8080"} {
		body := "[platform]\nbase_url = \"http://" + host + "\"\n"
		err := loadErr(t, []string{"-config", writeTOML(t, body)}, noEnv)
		if err == nil {
			t.Errorf("plain http platform.base_url %q was accepted at load", host)
			continue
		}
		if !strings.Contains(err.Error(), "loopback") {
			t.Errorf("platform.base_url %q: the refusal does not name the rule: %v", host, err)
		}
	}
}

func TestPlatformBaseURLAllowsPlainHTTPOnLoopback(t *testing.T) {
	body := "[platform]\nbase_url = \"http://127.0.0.1:9090\"\n"
	if _, _, err := Load([]string{"-config", writeTOML(t, body)}, noEnv); err != nil {
		t.Errorf("loopback platform.base_url was refused: %v", err)
	}
}

// platform.agents_api_url defaults to the real agents API, separate from
// platform.base_url (the human portal) — live testing found the two are
// different hosts in the real deployment, not one shared origin.
func TestAgentsAPIURLDefaults(t *testing.T) {
	cfg := load(t, nil, noEnv)
	if cfg.Platform.AgentsAPIURL != "https://agents-v1.nyks.dev" {
		t.Fatalf("got %q, want the default", cfg.Platform.AgentsAPIURL)
	}
	if cfg.Platform.AgentsAPIURL == cfg.Platform.BaseURL {
		t.Fatalf("agents_api_url and base_url defaulted to the same value; they are different hosts")
	}
}

// Same https-or-loopback rule as base_url (invariant 5): connect/mining
// enable send the platform-issued sr- key in Authorization to this one.
func TestAgentsAPIURLRejectsRoutablePlainHTTP(t *testing.T) {
	body := "[platform]\nagents_api_url = \"http://agents.example.com\"\n"
	err := loadErr(t, []string{"-config", writeTOML(t, body)}, noEnv)
	if err == nil {
		t.Fatal("plain http platform.agents_api_url was accepted at load")
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Errorf("the refusal does not name the rule: %v", err)
	}
}

func TestAgentsAPIURLAllowsPlainHTTPOnLoopback(t *testing.T) {
	body := "[platform]\nagents_api_url = \"http://127.0.0.1:9091\"\n"
	if _, _, err := Load([]string{"-config", writeTOML(t, body)}, noEnv); err != nil {
		t.Errorf("loopback platform.agents_api_url was refused: %v", err)
	}
}

// WP2-review must-fix: a dev/test config naming only a loopback
// base_url (every existing stub-backed test does exactly this) must not
// silently default agents_api_url to the real platform — connect/mining
// enable would then register against production while believing it was
// talking to a local stub, which is exactly what happened once, live,
// before this fix.
func TestAgentsAPIURLDefaultsToBaseURLWhenBaseURLIsLoopback(t *testing.T) {
	body := "[platform]\nbase_url = \"http://127.0.0.1:9999\"\n"
	cfg := load(t, []string{"-config", writeTOML(t, body)}, noEnv)
	if cfg.Platform.AgentsAPIURL != "http://127.0.0.1:9999" {
		t.Fatalf("agents_api_url defaulted to %q, want the loopback base_url reused, not the real platform",
			cfg.Platform.AgentsAPIURL)
	}
}

// The loopback default is a convenience, not a lock-in: an explicit
// agents_api_url still wins, including pointing a loopback base_url at
// a real (or a second, differently-loopback) agents API.
func TestAgentsAPIURLExplicitValueOverridesTheLoopbackDefault(t *testing.T) {
	body := "[platform]\nbase_url = \"http://127.0.0.1:9999\"\nagents_api_url = \"https://agents-v1.nyks.dev\"\n"
	cfg := load(t, []string{"-config", writeTOML(t, body)}, noEnv)
	if cfg.Platform.AgentsAPIURL != "https://agents-v1.nyks.dev" {
		t.Fatalf("got %q, want the explicit value", cfg.Platform.AgentsAPIURL)
	}
}

// The two [platform] URLs are independently configurable — setting one
// must not disturb the other's default.
func TestPlatformURLsAreIndependentlyConfigurable(t *testing.T) {
	body := "[platform]\nbase_url = \"https://portal.example.com\"\nagents_api_url = \"https://agents.example.com\"\n"
	cfg := load(t, []string{"-config", writeTOML(t, body)}, noEnv)
	if cfg.Platform.BaseURL != "https://portal.example.com" {
		t.Fatalf("base_url: got %q", cfg.Platform.BaseURL)
	}
	if cfg.Platform.AgentsAPIURL != "https://agents.example.com" {
		t.Fatalf("agents_api_url: got %q", cfg.Platform.AgentsAPIURL)
	}
}

// WP2-review edge case: a custom non-loopback base_url (a devnet, a
// staging portal) gives no safe signal about which API host pairs with
// it — defaulting to production here would be the same footgun the
// loopback fix above exists to close, just for a devnet instead of a
// laptop. Refused, not defaulted.
func TestNonDefaultNonLoopbackBaseURLRequiresExplicitAgentsAPIURL(t *testing.T) {
	body := "[platform]\nbase_url = \"https://portal.devnet.example.com\"\n"
	err := loadErr(t, []string{"-config", writeTOML(t, body)}, noEnv)
	if err == nil {
		t.Fatal("a devnet base_url with no agents_api_url was accepted, silently defaulting to production")
	}
	if !strings.Contains(err.Error(), "agents_api_url") {
		t.Errorf("the refusal does not name the missing key: %v", err)
	}
}

// MiningEnabledExplicit is the signal connect/mining enable use to decide
// whether to ask their terminal question at all. It must tell "the file
// wrote enabled = false" apart from "the file said nothing about
// [mining]" — both decode Mining.Enabled to false identically.
func TestMiningEnabledExplicitDistinguishesAbsentFromFalse(t *testing.T) {
	cfg := load(t, nil, noEnv)
	if cfg.MiningEnabledExplicit {
		t.Fatal("no config file at all: MiningEnabledExplicit should be false")
	}

	body := "[mining]\nenabled = false\n"
	cfg = load(t, []string{"-config", writeTOML(t, body)}, noEnv)
	if !cfg.MiningEnabledExplicit {
		t.Fatal("[mining] enabled = false was written explicitly; MiningEnabledExplicit should be true")
	}
	if cfg.Mining.Enabled {
		t.Fatal("enabled = false must still resolve to Mining.Enabled = false")
	}

	body = "[mining]\nenabled = true\nas_url = \"https://as.example.com\"\nchain_id = \"twilight-1\"\nslot_id = 7\ntarget_epoch = 42\n"
	cfg = load(t, []string{"-config", writeTOML(t, body)}, noEnv)
	if !cfg.MiningEnabledExplicit || !cfg.Mining.Enabled {
		t.Fatalf("explicit enabled = true: MiningEnabledExplicit=%v Enabled=%v, want true/true",
			cfg.MiningEnabledExplicit, cfg.Mining.Enabled)
	}

	body = "[proxy]\nlisten = \"127.0.0.1:9\"\n" // touches the file, never [mining]
	cfg = load(t, []string{"-config", writeTOML(t, body)}, noEnv)
	if cfg.MiningEnabledExplicit {
		t.Fatal("a file that never mentions [mining] should leave MiningEnabledExplicit false")
	}
}

// mining.payout_address is the scripted-install answer to the terminal
// question, read straight through regardless of Enabled — it names an
// address, connect/mining enable decide what it means.
func TestMiningPayoutAddressReadThrough(t *testing.T) {
	body := "[mining]\nenabled = true\nas_url = \"https://as.example.com\"\nchain_id = \"twilight-1\"\nslot_id = 7\ntarget_epoch = 42\npayout_address = \"twilight1abc\"\n"
	cfg := load(t, []string{"-config", writeTOML(t, body)}, noEnv)
	if cfg.Mining.PayoutAddress != "twilight1abc" {
		t.Fatalf("got %q, want twilight1abc", cfg.Mining.PayoutAddress)
	}
}
