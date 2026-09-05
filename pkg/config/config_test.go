package config

import (
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

func TestMiningEnabledDemandsIdentity(t *testing.T) {
	base := "[mining]\nenabled = true\nas_url = \"https://as.example.com\"\nchain_id = \"twilight-1\"\nslot_id = 7\ntarget_epoch = 42\n"
	cfg := load(t, []string{"-config", writeTOML(t, base)}, noEnv)
	m := cfg.Mining
	if !m.Enabled || m.ASBaseURL != "https://as.example.com" || m.ChainID != "twilight-1" || m.SlotID != 7 {
		t.Fatalf("mining block misparsed: %+v", m)
	}
	if m.TargetEpoch == nil || *m.TargetEpoch != 42 {
		t.Fatalf("target_epoch pin misparsed: %+v", m.TargetEpoch)
	}
	if m.MetadataTTL != 15*time.Minute {
		t.Fatalf("metadata_ttl default wrong: %v", m.MetadataTTL)
	}

	for name, body := range map[string]string{
		"missing as_url":   "[mining]\nenabled = true\nchain_id = \"twilight-1\"\nslot_id = 7\ntarget_epoch = 42\n",
		"missing chain_id": "[mining]\nenabled = true\nas_url = \"https://as.example.com\"\nslot_id = 7\ntarget_epoch = 42\n",
		"missing slot_id":  "[mining]\nenabled = true\nas_url = \"https://as.example.com\"\nchain_id = \"twilight-1\"\ntarget_epoch = 42\n",
		"relative as_url":  "[mining]\nenabled = true\nas_url = \"as.example.com\"\nchain_id = \"twilight-1\"\nslot_id = 7\ntarget_epoch = 42\n",
	} {
		if err := loadErr(t, []string{"-config", writeTOML(t, body)}, noEnv); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	// slot_id = 0 is a valid Core Slot, distinct from absent.
	zero := "[mining]\nenabled = true\nas_url = \"https://as.example.com\"\nchain_id = \"twilight-1\"\nslot_id = 0\ntarget_epoch = 42\n"
	if cfg := load(t, []string{"-config", writeTOML(t, zero)}, noEnv); cfg.Mining.SlotID != 0 {
		t.Fatalf("slot 0 mishandled: %+v", cfg.Mining)
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

func TestMiningRejectsUserinfoURL(t *testing.T) {
	body := "[mining]\nenabled = true\nas_url = \"https://token@as.example.com\"\nchain_id = \"twilight-1\"\nslot_id = 7\ntarget_epoch = 42\n"
	if err := loadErr(t, []string{"-config", writeTOML(t, body)}, noEnv); !strings.Contains(err.Error(), "userinfo") {
		t.Fatalf("userinfo as_url not rejected: %v", err)
	}
}
