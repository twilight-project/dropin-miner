package main

// Ruling D-R1 (issue #58): the config a command reads is resolved
// -config, then TOKENDROP_CONFIG, then ./tokendrop.toml, then the
// installation's own config ($TOKENDROP_HOME/tokendrop.toml, else
// ~/.tokendrop/tokendrop.toml, when that file exists), then built-in
// defaults — through the one function (describeConfigSource) that
// loadConfig, configGatePath and every command naming its source all call,
// so the gate always keys on exactly the file that gets loaded.
//
// The soak that found #58 read a stale, unclaimed installation under the
// compiled-in default state directory while a claimed, mining installation
// sat at ~/.tokendrop, because no step of the old order ever looked there.
// The tests below drive that exact shape, not just the function in
// isolation.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/twilight-project/dropin-miner/pkg/auth"
)

// writeInstallationConfig writes home/tokendrop.toml naming stateDir (under
// home, so cleanup is one RemoveAll) and returns the config path.
func writeInstallationConfig(t *testing.T, home string) (cfgPath, stateDir string) {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	stateDir = filepath.Join(home, "state")
	cfgPath = filepath.Join(home, setupConfigFile)
	body := fmt.Sprintf("[mining]\nstate_dir = %q\n", stateDir)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, stateDir
}

// TestConfigResolutionOrderEachStepWinsOverTheNext drives describeConfigSource
// through all four sources plus the defaults case, each one added on top of
// the last so a regression that drops a step shows up as the wrong (looser)
// source winning rather than as a total failure.
func TestConfigResolutionOrderEachStepWinsOverTheNext(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	// Nothing resolves at all: built-in defaults.
	if src := describeConfigSource("", noEnv); src != "" {
		t.Fatalf("step 0 (defaults): got %q, want \"\"", src)
	}

	// Step 4: the installation's own config.
	home := filepath.Join(dir, "home")
	installPath, _ := writeInstallationConfig(t, home)
	homeEnv := envOf(map[string]string{"TOKENDROP_HOME": home})
	if src := describeConfigSource("", homeEnv); src != installPath {
		t.Fatalf("step 4 (installation config): got %q, want %q", src, installPath)
	}

	// Step 3: ./tokendrop.toml beats the installation config.
	if err := os.WriteFile(filepath.Join(dir, "tokendrop.toml"), []byte("[mining]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if src := describeConfigSource("", homeEnv); src != "tokendrop.toml" {
		t.Fatalf("step 3 (cwd config): got %q, want tokendrop.toml", src)
	}

	// Step 2: TOKENDROP_CONFIG beats ./tokendrop.toml — named even though
	// the path itself does not exist, matching config.Load's own "explicit
	// sources are required to exist and error on their own" rule rather
	// than silently falling through to a weaker source.
	envPath := filepath.Join(dir, "env-named.toml")
	bothEnv := envOf(map[string]string{"TOKENDROP_HOME": home, "TOKENDROP_CONFIG": envPath})
	if src := describeConfigSource("", bothEnv); src != envPath {
		t.Fatalf("step 2 (TOKENDROP_CONFIG): got %q, want %q", src, envPath)
	}

	// Step 1: -config beats everything else, named the same way.
	flagPath := filepath.Join(dir, "flag-named.toml")
	if src := describeConfigSource(flagPath, bothEnv); src != flagPath {
		t.Fatalf("step 1 (-config): got %q, want %q", src, flagPath)
	}
}

// TestConfigGatePathKeysOnExactlyWhatLoadConfigWouldLoad proves the D1
// requirement directly: configGatePath's directory equals the directory of
// whatever describeConfigSource resolved, at every step of the order,
// including the defaults case (the installation home itself).
func TestConfigGatePathKeysOnExactlyWhatLoadConfigWouldLoad(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // empty: no ./tokendrop.toml here to shadow the cases below
	home := filepath.Join(dir, "home")
	writeInstallationConfig(t, home)

	installEnv := envOf(map[string]string{"TOKENDROP_HOME": home})
	cases := []struct {
		name    string
		cfgFlag string
		getenv  func(string) string
	}{
		{"defaults, no installation", "", noEnv},
		{"installation config", "", installEnv},
		{"-config beats everything", filepath.Join(dir, "flag.toml"), installEnv},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := describeConfigSource(c.cfgFlag, c.getenv)
			wantDir := filepath.Dir(src)
			if src == "" {
				wantDir = defaultTokendropHome(c.getenv) // configGatePath's own default-installation fallback
			}
			wantAbs, err := filepath.Abs(wantDir)
			if err != nil {
				t.Fatal(err)
			}
			want := lifecycleGatePath(wantAbs)

			got, err := configGatePath(c.cfgFlag, c.getenv)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("gate = %q, want %q (config source %q)", got, want, src)
			}
		})
	}
}

// TestStatusReadsTheInstallationConfigWhenNothingElseResolves reproduces
// the soak's exact finding: an installation at ~/.tokendrop, no
// TOKENDROP_CONFIG in the environment, and a cwd with no tokendrop.toml.
// Before D1, status silently read the compiled-in default state directory
// instead and reported an unrelated (here: absent) installation.
func TestStatusReadsTheInstallationConfigWhenNothingElseResolves(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	installHome := filepath.Join(home, ".tokendrop")
	_, stateDir := writeInstallationConfig(t, installHome)

	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMiningEnabled(true); err != nil {
		t.Fatal(err)
	}

	cwd := t.TempDir() // deliberately no tokendrop.toml here
	t.Chdir(cwd)

	var out, errOut bytes.Buffer
	// No -config flag and an env with no TOKENDROP_CONFIG/TOKENDROP_HOME:
	// exactly the "shell opened before the profile block loads" case #58
	// named. Resolution falls all the way to step 4.
	if code := statusMain(nil, &out, &errOut, noEnv); code != exitOK {
		t.Fatalf("status exited %d: %s", code, errOut.String())
	}
	text := out.String()
	if !strings.Contains(text, filepath.Join(installHome, setupConfigFile)) {
		t.Fatalf("status did not name the installation config it read:\n%s", text)
	}
	if !strings.Contains(text, stateDir) {
		t.Fatalf("status did not name the state directory it read:\n%s", text)
	}
	if !strings.Contains(text, "mining:  ON") {
		t.Fatalf("status read the wrong installation (mining should be ON here):\n%s", text)
	}

	// -json carries the same two facts as data, not prose.
	var jsonOut bytes.Buffer
	if code := statusMain([]string{"-json"}, &jsonOut, &errOut, noEnv); code != exitOK {
		t.Fatalf("status -json exited %d: %s", code, errOut.String())
	}
	env := decodeEnvelope(t, jsonOut.String())
	data, _ := env["data"].(map[string]any)
	if data["config_source"] != filepath.Join(installHome, setupConfigFile) {
		t.Fatalf("status -json config_source = %v, want the installation config", data["config_source"])
	}
	if data["config_defaulted"] != false {
		t.Fatalf("status -json config_defaulted = %v, want false", data["config_defaulted"])
	}
	if data["state_dir"] != stateDir {
		t.Fatalf("status -json state_dir = %v, want %q", data["state_dir"], stateDir)
	}
}

// TestDoctorNamesDefaultsWhenNoConfigFileExistsAnywhere is the negative
// case: with nothing at all to resolve, both renderers say so explicitly
// rather than silently reporting against built-in defaults with no hint.
func TestDoctorNamesDefaultsWhenNoConfigFileExistsAnywhere(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	// cmdDoctor (unlike statusMain/connectRun) has no injectable getenv —
	// it reads os.Getenv directly — so, unlike every other test in this
	// file, it is exposed to whatever TOKENDROP_* the developer's own
	// shell profile happens to export. Blank the three explicitly rather
	// than relying on HOME alone: a real TOKENDROP_CONFIG in this
	// process's environment would otherwise make this "nothing resolves"
	// test dial that developer's live installation.
	t.Setenv("TOKENDROP_CONFIG", "")
	t.Setenv("TOKENDROP_HOME", "")
	t.Setenv("TOKENDROP_WALLET_DIR", "")
	cwd := t.TempDir()
	t.Chdir(cwd)

	var out, errOut bytes.Buffer
	cmdDoctor(nil, &out, &errOut)
	if !strings.Contains(out.String(), "defaults/env, no config file found") {
		t.Fatalf("doctor did not say it fell back to defaults:\n%s", out.String())
	}

	var jsonOut bytes.Buffer
	cmdDoctor([]string{"-json"}, &jsonOut, &errOut)
	env := decodeEnvelope(t, jsonOut.String())
	data, _ := env["data"].(map[string]any)
	if data["config_defaulted"] != true {
		t.Fatalf("doctor -json config_defaulted = %v, want true", data["config_defaulted"])
	}
	if _, named := data["config_source"]; named {
		t.Fatalf("doctor -json named a config_source with nothing resolved: %v", data["config_source"])
	}
}

// TestConnectRefusesWithNoConfigFileAnywhere is D1's connect requirement:
// resolution finding no config file at all — not even the installation's
// own — refuses outright, names setup, and writes nothing under the
// default installation.
func TestConnectRefusesWithNoConfigFileAnywhere(t *testing.T) {
	withShortConnectTimings(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cwd := t.TempDir()
	t.Chdir(cwd)

	var out, errOut bytes.Buffer
	code := cmdConnect(nil, strings.NewReader(""), &out, &errOut, noEnv)
	if code != exitTransport {
		t.Fatalf("code = %d, want exitTransport; stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "setup") {
		t.Fatalf("refusal did not name setup: %s", errOut.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".tokendrop")); !os.IsNotExist(err) {
		t.Fatalf("connect wrote to the default installation with no config resolved: stat err = %v", err)
	}

	// -json: a distinct, non-retryable code with action fix_input, never
	// the generic connect_failed/retry an exitTransport otherwise implies.
	var jsonOut bytes.Buffer
	code = cmdConnect([]string{"-json"}, strings.NewReader(""), &jsonOut, &errOut, noEnv)
	if code != exitTransport {
		t.Fatalf("json code = %d, want exitTransport", code)
	}
	env := decodeEnvelope(t, jsonOut.String())
	if env["code"] != "config_not_found" {
		t.Fatalf("json code field = %v, want config_not_found", env["code"])
	}
	if env["retryable"] != false {
		t.Fatalf("json retryable = %v, want false", env["retryable"])
	}
	if env["action"] != actionFixInput {
		t.Fatalf("json action = %v, want %q", env["action"], actionFixInput)
	}
	if _, err := os.Stat(filepath.Join(home, ".tokendrop")); !os.IsNotExist(err) {
		t.Fatalf("connect -json wrote to the default installation with no config resolved: stat err = %v", err)
	}
}

// TestConnectStopsWithTheFileNamedWhenTheInstallationConfigCannotBeParsed
// is D-R1's second connect rule: an installation config that exists but is
// unreadable stops connect with that exact file named, not a generic
// defaults message.
func TestConnectStopsWithTheFileNamedWhenTheInstallationConfigCannotBeParsed(t *testing.T) {
	withShortConnectTimings(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	installHome := filepath.Join(home, ".tokendrop")
	if err := os.MkdirAll(installHome, 0o700); err != nil {
		t.Fatal(err)
	}
	badPath := filepath.Join(installHome, setupConfigFile)
	if err := os.WriteFile(badPath, []byte("not valid toml `{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	t.Chdir(cwd)

	var out, errOut bytes.Buffer
	code := cmdConnect(nil, strings.NewReader(""), &out, &errOut, noEnv)
	if code != exitTransport {
		t.Fatalf("code = %d, want exitTransport; stderr=%s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), badPath) {
		t.Fatalf("refusal did not name the unreadable installation config %q: %s", badPath, errOut.String())
	}
}
