package main

// The wallet CLI: init and address. The crypto layers are pinned by
// specification vectors in internal/auth/walletkeys_test.go; these tests
// cover the operator-facing behavior.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/twilight-project/dropin-miner/pkg/auth"
)

// walletScratchDir returns a 0700 dir: t.TempDir() is 0755 on some
// platforms and openWalletDir rightly refuses group/world access.
func walletScratchDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "wallet")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestWalletInitCreatesAWalletAndRefusesToOverwriteIt(t *testing.T) {
	dir := walletScratchDir(t)
	env := envOf(map[string]string{walletPassphraseEnv: "test-passphrase"})

	var out, errOut bytes.Buffer
	code := cmdWallet([]string{"init", "-dir", dir, "-print-anyway"},
		strings.NewReader(""), &out, &errOut, env)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "shown ONCE") || !strings.Contains(out.String(), "address: twilight1") {
		t.Fatalf("init output missing the essentials: %s", out.String())
	}
	for _, name := range []string{walletKeyFile, walletSidecarFile} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if posixModes && info.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is group/world-accessible: %04o", name, info.Mode().Perm())
		}
	}

	// Second init: refused, and the keyfile bytes stay identical.
	before, _ := os.ReadFile(filepath.Join(dir, walletKeyFile)) // #nosec G304 G703 -- test-owned temp dir
	out.Reset()
	errOut.Reset()
	if code := cmdWallet([]string{"init", "-dir", dir, "-print-anyway"},
		strings.NewReader(""), &out, &errOut, env); code == 0 {
		t.Fatal("a second init overwrote an existing wallet")
	}
	if !strings.Contains(errOut.String(), "refusing to overwrite") {
		t.Errorf("refusal should explain itself: %q", errOut.String())
	}
	after, _ := os.ReadFile(filepath.Join(dir, walletKeyFile)) // #nosec G304 G703 -- test-owned temp dir
	if !bytes.Equal(before, after) {
		t.Fatal("the refused init still modified the keyfile")
	}
}

// The mnemonic exists on the console and nowhere else: after init, no
// window of consecutive mnemonic words may appear in any file the wallet
// wrote.
func TestWalletInitNeverPersistsTheMnemonic(t *testing.T) {
	dir := walletScratchDir(t)
	var out, errOut bytes.Buffer
	code := cmdWallet([]string{"init", "-dir", dir, "-print-anyway"},
		strings.NewReader(""), &out, &errOut,
		envOf(map[string]string{walletPassphraseEnv: "test-passphrase"}))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}

	// Recover the printed words: the indented lines before the warning
	// paragraph (the "Next:" section is indented too, and is not part of
	// the phrase).
	var words []string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "Anyone with these words") {
			break
		}
		if strings.HasPrefix(line, "    ") {
			words = append(words, strings.Fields(line)...)
		}
	}
	if len(words) != 24 {
		t.Fatalf("expected to recover 24 printed words, got %d", len(words))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	needle := strings.Join(words[:3], " ")
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name())) // #nosec G304 -- test-owned temp dir
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), needle) || strings.Contains(string(data), words[0]) && strings.Contains(string(data), words[12]) && strings.Contains(string(data), words[23]) {
			t.Errorf("file %s appears to contain mnemonic material", e.Name())
		}
	}
}

func TestWalletAddressPrintsTheSidecarAddress(t *testing.T) {
	dir := walletScratchDir(t)
	var out, errOut bytes.Buffer
	if code := cmdWallet([]string{"init", "-dir", dir, "-print-anyway"},
		strings.NewReader(""), &out, &errOut,
		envOf(map[string]string{walletPassphraseEnv: "p-test-1"})); code != 0 {
		t.Fatalf("init: %s", errOut.String())
	}
	// The address line from init.
	var fromInit string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "address: ") {
			fromInit = strings.TrimPrefix(line, "address: ")
		}
	}

	out.Reset()
	if code := cmdWallet([]string{"address", "-dir", dir}, strings.NewReader(""), &out, &errOut, noEnv); code != 0 {
		t.Fatalf("address: %s", errOut.String())
	}
	got := strings.TrimSpace(out.String())
	if got == "" || got != fromInit {
		t.Fatalf("wallet address printed %q, init printed %q", got, fromInit)
	}
	if _, _, err := walletDecode(got); err != nil {
		t.Fatalf("printed address does not decode: %v", err)
	}
}

func TestWalletInitRefusesANonTerminalWithoutPrintAnyway(t *testing.T) {
	dir := walletScratchDir(t)
	var out, errOut bytes.Buffer
	code := cmdWallet([]string{"init", "-dir", dir},
		strings.NewReader(""), &out, &errOut,
		envOf(map[string]string{walletPassphraseEnv: "p-test-1"}))
	if code == 0 {
		t.Fatal("init printed a mnemonic into a non-terminal without -print-anyway")
	}
	if !strings.Contains(errOut.String(), "-print-anyway") {
		t.Errorf("refusal should name the override: %q", errOut.String())
	}
	if _, err := os.Stat(filepath.Join(dir, walletKeyFile)); err == nil {
		t.Fatal("a refused init still wrote a keyfile")
	}
}

func walletDecode(s string) (string, []byte, error) { return auth.DecodeBech32Address(s) }

// register declares the address the wallet actually holds. The failure it
// exists to prevent is a person copying an address between two windows and
// getting one character wrong: nothing on the server side catches that
// until WALLET_SIGNATURE_V1, so the fix is to never retype it.
func TestWalletRegisterDeclaresTheWalletsOwnAddress(t *testing.T) {
	dir := walletScratchDir(t)
	var out, errOut bytes.Buffer
	if code := cmdWallet([]string{"init", "-dir", dir, "-print-anyway"},
		strings.NewReader(""), &out, &errOut,
		envOf(map[string]string{walletPassphraseEnv: "p-test-1"})); code != 0 {
		t.Fatalf("init: %s", errOut.String())
	}

	// The address register WOULD declare is the one in the sidecar, which
	// is the one init printed. Asserting the wiring without a live AS: the
	// sidecar is the only source, so there is nothing to mistype.
	sc, _, err := loadSidecar(dir, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	var printed string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "address: ") {
			printed = strings.TrimPrefix(line, "address: ")
		}
	}
	if sc.Address != printed {
		t.Fatalf("sidecar holds %q, init printed %q", sc.Address, printed)
	}
	if _, _, err := walletDecode(sc.Address); err != nil {
		t.Fatalf("the address register would declare does not decode: %v", err)
	}
}

// Without a wallet, register says how to make one rather than failing
// somewhere deep in the AS client.
func TestWalletRegisterWithoutAWalletSaysHowToMakeOne(t *testing.T) {
	dir := walletScratchDir(t)
	var out, errOut bytes.Buffer
	if code := cmdWallet([]string{"register", "-dir", dir}, strings.NewReader(""), &out, &errOut, noEnv); code == 0 {
		t.Fatal("register succeeded with no wallet present")
	}
	if !strings.Contains(errOut.String(), "wallet init") {
		t.Errorf("the refusal should name the fix: %q", errOut.String())
	}
}

// register needs a config to find the AS, and refuses before touching the
// network when it cannot find one. The refusal has to say WHICH failure it
// is: "no config anywhere" and "a config with no [mining] block" have
// different fixes, and the old wording described the second while nearly
// always meaning the first.
func TestWalletRegisterSaysNoConfigWasFound(t *testing.T) {
	dir := walletScratchDir(t)
	// A directory with no tokendrop.toml, so the cwd leg of the search
	// finds nothing either. t.Chdir keeps a developer's real one out.
	t.Chdir(t.TempDir())
	t.Setenv("TOKENDROP_CONFIG", "")

	var out, errOut bytes.Buffer
	if code := cmdWallet([]string{"init", "-dir", dir, "-print-anyway"},
		strings.NewReader(""), &out, &errOut,
		envOf(map[string]string{walletPassphraseEnv: "p-test-1"})); code != 0 {
		t.Fatalf("init: %s", errOut.String())
	}
	errText := captureStderr(t, func() {
		_ = cmdWallet([]string{"register", "-dir", dir}, strings.NewReader(""), &out, &errOut, noEnv)
	})
	for _, want := range []string{"no config file found", "TOKENDROP_CONFIG", "-config"} {
		if !strings.Contains(errText, want) {
			t.Errorf("the refusal should mention %q: %q", want, errText)
		}
	}
}

// The environment is a first-class way to name the config, exactly as it is
// for the daemon and the agent commands. Before this, these six commands
// were the only ones that ignored TOKENDROP_CONFIG and demanded the flag.
func TestOperatorCommandsHonorTokendropConfigFromTheEnvironment(t *testing.T) {
	dir := walletScratchDir(t)
	var out, errOut bytes.Buffer
	if code := cmdWallet([]string{"init", "-dir", dir, "-print-anyway"},
		strings.NewReader(""), &out, &errOut,
		envOf(map[string]string{walletPassphraseEnv: "p-test-1"})); code != 0 {
		t.Fatalf("init: %s", errOut.String())
	}

	// A config with [mining] enabled but an AS that does not exist: getting
	// PAST config resolution is the whole point, so the failure we want is
	// a network/discovery one, never "no config".
	cfg := filepath.Join(t.TempDir(), "tokendrop.toml")
	body := "[[provider]]\nname = \"search-router\"\nupstream = \"https://upstream.invalid\"\n\n" +
		"[mining]\nenabled = true\nas_url = \"https://as.invalid\"\nchain_id = \"c\"\nslot_id = 1\n" +
		"state_dir = \"" + dir + "\"\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOKENDROP_CONFIG", cfg)

	errText := captureStderr(t, func() {
		_ = cmdWallet([]string{"register", "-dir", dir}, strings.NewReader(""), &out, &errOut, noEnv)
	})
	if strings.Contains(errText, "no config file found") || strings.Contains(errText, "-config is required") {
		t.Errorf("TOKENDROP_CONFIG was ignored: %q", errText)
	}
	if strings.Contains(errText, "no [mining] block") {
		t.Errorf("the config from the environment was not read: %q", errText)
	}
}

func TestWalletRejectsAnUnknownSubcommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := cmdWallet([]string{"send-everything"}, strings.NewReader(""), &out, &errOut, noEnv); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if code := cmdWallet(nil, strings.NewReader(""), &out, &errOut, noEnv); code != 2 {
		t.Errorf("no subcommand: exit %d, want 2", code)
	}
}

// The setup script puts the wallet beside a participant's other files
// rather than in the OS config dir, so every command has to be able to
// find it without -dir. Without this the documented examples would look
// in the wrong place and report "no wallet here yet".
func TestWalletDirComesFromTheEnvironmentWhenNoFlagIsGiven(t *testing.T) {
	dir := walletScratchDir(t)
	env := envOf(map[string]string{
		walletPassphraseEnv: "p-test-1",
		walletDirEnv:        dir,
	})
	var out, errOut bytes.Buffer
	// No -dir anywhere below: the env var is the only thing pointing here.
	if code := cmdWallet([]string{"init", "-print-anyway"}, strings.NewReader(""), &out, &errOut, env); code != 0 {
		t.Fatalf("init: %s", errOut.String())
	}
	if _, err := os.Stat(filepath.Join(dir, walletKeyFile)); err != nil {
		t.Fatalf("init did not write into %s: %v", dir, err)
	}
	var printed string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "address: ") {
			printed = strings.TrimPrefix(line, "address: ")
		}
	}

	out.Reset()
	if code := cmdWallet([]string{"address"}, strings.NewReader(""), &out, &errOut, env); code != 0 {
		t.Fatalf("address: %s", errOut.String())
	}
	if strings.TrimSpace(out.String()) != printed {
		t.Errorf("address read %q, init wrote %q", out.String(), printed)
	}

	// An explicit -dir still wins over the environment.
	other := walletScratchDir(t)
	out.Reset()
	errOut.Reset()
	if code := cmdWallet([]string{"address", "-dir", other}, strings.NewReader(""), &out, &errOut, env); code == 0 {
		t.Error("-dir was ignored in favor of the environment")
	}
}
