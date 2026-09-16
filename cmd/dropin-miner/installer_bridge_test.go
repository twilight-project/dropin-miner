package main

// The installers' rollout bridge, exercised on the scripts themselves.
//
// Until v0.3.0 is the latest release, install.sh and install.ps1 on main
// download a binary that may predate setup. They decide which path to take
// by probing `<bin> setup -h`, and these tests drive both branches of that
// decision offline through TOKENDROP_INSTALL_BIN — never the network, never
// a real GitHub release:
//
//   - a stub binary whose `setup -h` exits 2 (what v0.2.8 does) must send
//     the script down its legacy path: setup.sh on POSIX, the PowerShell
//     config-and-connect blocks on Windows;
//   - the real binary built from this tree must be handed `setup`, and on
//     Windows the legacy blocks must not run at all.
//
// install.sh runs on POSIX only (it refuses other systems itself), and
// install.ps1 on the Windows runner only.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

var bridgeBinaries struct {
	mu        sync.Mutex
	real, old string
}

func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// buildBridgeBinaries builds the real binary from this tree and a stand-in
// for a pre-setup release, once per test process.
func buildBridgeBinaries(t *testing.T) (real, old string) {
	t.Helper()
	bridgeBinaries.mu.Lock()
	defer bridgeBinaries.mu.Unlock()
	if bridgeBinaries.real != "" {
		return bridgeBinaries.real, bridgeBinaries.old
	}
	dir, err := os.MkdirTemp("", "dropin-miner-bridge")
	if err != nil {
		t.Fatal(err)
	}
	realDir := filepath.Join(dir, "real")
	oldDir := filepath.Join(dir, "old")
	for _, d := range []string{realDir, oldDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	real = filepath.Join(realDir, exeName("dropin-miner"))
	if out, err := exec.Command("go", "build", "-o", real, ".").CombinedOutput(); err != nil { // #nosec G204 -- this test's own temp path
		t.Fatalf("build: %v\n%s", err, out)
	}

	// The stand-in answers the way v0.2.8 does where it matters: `setup` is
	// an unknown command (exit 2), `version` and `connect` succeed. Every
	// invocation is appended to $STUB_LOG so a test can see what ran.
	src := filepath.Join(dir, "stub")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	stub := `package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	if log := os.Getenv("STUB_LOG"); log != "" {
		f, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintln(f, strings.Join(os.Args[1:], " "))
			f.Close()
		}
	}
	if len(os.Args) > 1 && os.Args[1] == "setup" {
		fmt.Fprintln(os.Stderr, "dropin-miner: unknown command \"setup\"")
		os.Exit(2)
	}
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("dropin-miner 0.2.8")
	}
}
`
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(stub), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "go.mod"), []byte("module stub\n\ngo 1.25\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old = filepath.Join(oldDir, exeName("dropin-miner"))
	build := exec.Command("go", "build", "-o", old, ".") // #nosec G204 -- this test's own temp path
	build.Dir = src
	build.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build stub: %v\n%s", err, out)
	}
	bridgeBinaries.real, bridgeBinaries.old = real, old
	return real, old
}

func scriptPath(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "scripts", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// installerEnv is a minimal environment: the sandboxed home, the stubs, and
// PATH — nothing of the developer's own configuration leaks in.
func installerEnv(userHome string, extra map[string]string) []string {
	env := []string{"HOME=" + userHome, "USERPROFILE=" + userHome, "PATH=" + os.Getenv("PATH")}
	for _, k := range []string{"SystemRoot", "SYSTEMROOT", "TEMP", "TMP", "ComSpec", "PATHEXT", "windir", "ProgramFiles"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

func runScript(t *testing.T, cmd *exec.Cmd) (code int, out string) {
	t.Helper()
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode(), buf.String()
	}
	if err != nil {
		t.Fatalf("run %v: %v\n%s", cmd.Args, err, buf.String())
	}
	return 0, buf.String()
}

// install.ps1 is pure ASCII. Windows PowerShell reads a script file saved
// without a byte-order mark in the system's ANSI code page, so a UTF-8 em
// dash arrives as three cp1252 characters, the last of which is a right
// double quotation mark that PowerShell accepts as a string delimiter: a
// quoted string with an em dash in it ends early and the file stops parsing.
// This runs on every OS, so an editor's typographic dash cannot wait for the
// Windows runner to be caught.
func TestInstallPs1IsPureASCII(t *testing.T) {
	b, err := os.ReadFile(scriptPath(t, "install.ps1")) // #nosec G304 -- this repo's own script
	if err != nil {
		t.Fatal(err)
	}
	line, col := 1, 1
	for _, c := range b {
		if c > 0x7f {
			t.Fatalf("scripts/install.ps1:%d:%d holds byte 0x%02x; the file must be pure ASCII for Windows PowerShell", line, col, c)
		}
		if c == '\n' {
			line, col = line+1, 1
		} else {
			col++
		}
	}
}

func TestInstallShTakesTheLegacyPathForABinaryWithoutSetup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is POSIX-only; install.ps1 has its own test")
	}
	_, old := buildBridgeBinaries(t)
	root := t.TempDir()
	userHome := filepath.Join(root, "user")
	binDir := filepath.Join(root, "old")
	if err := os.MkdirAll(userHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stubBin := filepath.Join(binDir, "dropin-miner")
	copyFile(t, old, stubBin, 0o700)
	copyFile(t, scriptPath(t, "setup.sh"), filepath.Join(binDir, "setup.sh"), 0o600)
	log := filepath.Join(root, "stub.log")
	home := filepath.Join(userHome, ".tokendrop")

	// The next step it prints names setup.sh, not setup.
	cmd := exec.Command("sh", scriptPath(t, "install.sh")) // #nosec G204 -- this repo's own script
	cmd.Env = installerEnv(userHome, map[string]string{"TOKENDROP_INSTALL_BIN": stubBin, "TOKENDROP_HOME": home, "TOKENDROP_INSTALL_NO_SETUP": "1", "STUB_LOG": log})
	code, out := runScript(t, cmd)
	if code != 0 || !strings.Contains(out, "Next: TOKENDROP_BIN="+stubBin+" sh "+filepath.Join(binDir, "setup.sh")) {
		t.Fatalf("NO_SETUP legacy next step: exit %d\n%s", code, out)
	}

	cmd = exec.Command("sh", scriptPath(t, "install.sh")) // #nosec G204 -- this repo's own script
	cmd.Env = installerEnv(userHome, map[string]string{"TOKENDROP_INSTALL_BIN": stubBin, "TOKENDROP_HOME": home, "STUB_LOG": log, "SHELL": "/bin/sh"})
	code, out = runScript(t, cmd)
	if code != 0 {
		t.Fatalf("install.sh exited %d\n%s", code, out)
	}
	if !strings.Contains(out, "Using binary: "+stubBin) {
		t.Errorf("setup.sh did not run:\n%s", out)
	}
	calls, _ := os.ReadFile(log) // #nosec G304 -- this test's own log
	if !strings.Contains(string(calls), "setup -h") || !strings.Contains(string(calls), "connect -config "+filepath.Join(home, "tokendrop.toml")) {
		t.Errorf("stub saw %q; want the probe, then setup.sh's connect", calls)
	}
	if strings.Contains(strings.ReplaceAll(string(calls), "setup -h", ""), "setup") {
		t.Errorf("the legacy path still invoked setup: %q", calls)
	}
	if cfg, err := os.ReadFile(filepath.Join(home, "tokendrop.toml")); err != nil || !strings.Contains(string(cfg), "# the GATEWAY, not the verification API") { // #nosec G304 -- sandbox file
		t.Errorf("setup.sh's config was not written: %v\n%s", err, cfg)
	}
}

func TestInstallShHandsOffToSetupWhenTheBinaryHasIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is POSIX-only; install.ps1 has its own test")
	}
	real, _ := buildBridgeBinaries(t)
	platform := newStubPlatform(t)
	as := newStubAS(t)
	platform.claim("credits")
	root := t.TempDir()
	userHome := filepath.Join(root, "user")
	if err := os.MkdirAll(userHome, 0o700); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(userHome, ".tokendrop")
	// No setup.sh beside the real binary: a legacy branch taken by mistake
	// would fail on its absence instead of passing by accident.
	env := map[string]string{ // #nosec G101 -- env var names and stub URLs, no credential
		"TOKENDROP_INSTALL_BIN":    real,
		"TOKENDROP_HOME":           home,
		"TOKENDROP_AS_URL":         as.srv.URL,
		"TOKENDROP_ROUTER_URL":     "https://router.invalid.test",
		"TOKENDROP_PLATFORM_URL":   platform.srv.URL,
		"TOKENDROP_AGENTS_API_URL": platform.srv.URL,
		"SHELL":                    "/bin/sh",
	}

	noSetup := map[string]string{"TOKENDROP_INSTALL_NO_SETUP": "1"}
	for k, v := range env {
		noSetup[k] = v
	}
	cmd := exec.Command("sh", scriptPath(t, "install.sh")) // #nosec G204 -- this repo's own script
	cmd.Env = installerEnv(userHome, noSetup)
	code, out := runScript(t, cmd)
	if code != 0 || !strings.Contains(out, "Next: "+real+" setup\n") {
		t.Fatalf("NO_SETUP next step: exit %d\n%s", code, out)
	}

	cmd = exec.Command("sh", scriptPath(t, "install.sh")) // #nosec G204 -- this repo's own script
	cmd.Env = installerEnv(userHome, env)
	code, out = runScript(t, cmd)
	if code != 0 {
		t.Fatalf("install.sh exited %d\n%s", code, out)
	}
	cfg, err := os.ReadFile(filepath.Join(home, "tokendrop.toml")) // #nosec G304 -- sandbox file
	if err != nil || strings.Contains(string(cfg), "# the GATEWAY") {
		t.Fatalf("setup did not write the config (or setup.sh did): %v\n%s", err, cfg)
	}
	if !strings.Contains(out, "Setup complete") || !strings.Contains(out, "(looked for:") && !strings.Contains(out, "Not an interactive shell — not touching any agent") {
		t.Errorf("setup's own narration missing:\n%s", out)
	}
	if reg, _, _ := platform.counts(); reg != 1 {
		t.Errorf("register calls = %d, want 1", reg)
	}
}

func copyFile(t *testing.T, from, to string, mode os.FileMode) {
	t.Helper()
	b, err := os.ReadFile(from) // #nosec G304 -- test-owned paths
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, mode); err != nil { // #nosec G703 -- test-owned path
		t.Fatal(err)
	}
}

// powershell is Windows PowerShell, the host `irm … | iex` runs in.
func powershell(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Fatal("powershell.exe is required on Windows to test install.ps1")
	}
	return p
}

func TestInstallPs1HandsOffToSetupAndSkipsTheLegacyBlocks(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("install.ps1 runs on the Windows runner")
	}
	real, _ := buildBridgeBinaries(t)
	platform := newStubPlatform(t)
	as := newStubAS(t)
	platform.claim("credits")
	root := t.TempDir()
	userHome := filepath.Join(root, "user")
	if err := os.MkdirAll(userHome, 0o700); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(userHome, ".tokendrop")
	cmd := exec.Command(powershell(t), "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath(t, "install.ps1")) // #nosec G204 -- this repo's own script
	cmd.Env = installerEnv(userHome, map[string]string{                                                                                      // #nosec G101 -- env var names and stub URLs, no credential
		"TOKENDROP_INSTALL_BIN":    real,
		"TOKENDROP_HOME":           home,
		"TOKENDROP_AS_URL":         as.srv.URL,
		"TOKENDROP_ROUTER_URL":     "https://router.invalid.test",
		"TOKENDROP_PLATFORM_URL":   platform.srv.URL,
		"TOKENDROP_AGENTS_API_URL": platform.srv.URL,
		"LOCALAPPDATA":             filepath.Join(userHome, "AppData", "Local"),
		"APPDATA":                  filepath.Join(userHome, "AppData", "Roaming"),
	})
	code, out := runScript(t, cmd)
	if code != 0 {
		t.Fatalf("install.ps1 exited %d\n%s", code, out)
	}
	for _, legacy := range []string{"==> Wrote", "==> Connecting", "Installed and connected.", "==> Added"} {
		if strings.Contains(out, legacy) {
			t.Errorf("the legacy block ran on the setup path (%q):\n%s", legacy, out)
		}
	}
	if !strings.Contains(out, "Setup complete") {
		t.Errorf("setup did not run:\n%s", out)
	}
	if reg, _, _ := platform.counts(); reg != 1 {
		t.Errorf("register calls = %d, want 1", reg)
	}
}

func TestInstallPs1TakesTheLegacyPathForABinaryWithoutSetup(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("install.ps1 runs on the Windows runner")
	}
	// The legacy path writes the real User environment (Path and
	// TOKENDROP_CONFIG) and runs icacls, exactly as it always has. It runs
	// on CI, where the runner is discarded, and the values are restored
	// afterwards regardless.
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		t.Skip("the legacy install.ps1 path changes the User environment; it runs on CI only")
	}
	restoreUserEnvironmentAfter(t, "Path", "TOKENDROP_CONFIG")

	_, old := buildBridgeBinaries(t)
	root := t.TempDir()
	userHome := filepath.Join(root, "user")
	if err := os.MkdirAll(userHome, 0o700); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(userHome, ".tokendrop")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(root, "stub.log")
	cmd := exec.Command(powershell(t), "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath(t, "install.ps1")) // #nosec G204 -- this repo's own script
	cmd.Env = installerEnv(userHome, map[string]string{
		"TOKENDROP_INSTALL_BIN": old,
		"TOKENDROP_HOME":        home,
		"STUB_LOG":              log,
		"USERNAME":              os.Getenv("USERNAME"),
	})
	code, out := runScript(t, cmd)
	if code != 0 {
		t.Fatalf("install.ps1 exited %d\n%s", code, out)
	}
	if !strings.Contains(out, "==> Wrote") || !strings.Contains(out, "==> Connecting") {
		t.Errorf("the legacy config-and-connect blocks did not run:\n%s", out)
	}
	calls, _ := os.ReadFile(log) // #nosec G304 -- this test's own log
	if !strings.Contains(string(calls), "setup -h") || !strings.Contains(string(calls), "connect -config") {
		t.Errorf("stub saw %q; want the probe, then the legacy connect", calls)
	}
	if strings.Contains(strings.ReplaceAll(string(calls), "setup -h", ""), "setup") {
		t.Errorf("the legacy path still invoked setup: %q", calls)
	}
}
