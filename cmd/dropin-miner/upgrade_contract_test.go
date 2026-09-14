package main

// The command-side contracts a self-upgrade depends on: who may upgrade
// natively, and what a release binary's version command prints.

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/twilight-project/dropin-miner/internal/selfupdate"
)

func TestUpgradeRefusesEveryCopyThePackageManagerOwns(t *testing.T) {
	root := t.TempDir()
	native := filepath.Join(root, "home", "bin", "dropin-miner")
	layout := filepath.Join(root, "pkg")
	writeFileT(t, filepath.Join(layout, "package.json"), `{"name":"dropin-miner"}`)
	writeFileT(t, filepath.Join(layout, "install.js"), "")
	writeFileT(t, filepath.Join(layout, "bin", "dropin-miner.js"), "")
	partial := filepath.Join(root, "partial")
	writeFileT(t, filepath.Join(partial, "install.js"), "")

	if err := checkUpgradeLaunch(native, ""); err != nil {
		t.Errorf("a native copy may upgrade itself: %v", err)
	}
	for name, tc := range map[string]struct {
		exe, marker, want string
	}{
		"npm global":        {native, "npm:global", "npm install -g dropin-miner@latest"},
		"npm local":         {native, "npm:local", "that project's dependencies (package.json and its lockfile)"},
		"npm unknown":       {native, "npm:unknown", "package manager that installed it"},
		"npm exec cache":    {filepath.Join(root, "_npx", "1", "dropin-miner"), "", "package manager that installed it"},
		"node_modules":      {filepath.Join(root, "node_modules", "dropin-miner", "bin", "dropin-miner"), "", "package manager"},
		"npm layout":        {filepath.Join(layout, "bin", "dropin-miner"), "", "package manager"},
		"ambiguous layout":  {filepath.Join(partial, "bin", "dropin-miner"), "", "unclear"},
		"launcher override": {native, "npm:global", "npm install -g"},
	} {
		err := checkUpgradeLaunch(tc.exe, tc.marker)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want a refusal saying %q, got %v", name, tc.want, err)
		}
	}
}

// TestVersionOutputIsAReleaseCompatibilityContract is permanent. Every
// installed updater validates a candidate — and, after replacement, the
// canonical binary — by running `version` and accepting only exactly
// "dropin-miner X.Y.Z\n" with nothing on stderr, under a stripped
// environment. A release whose version command prints anything else can
// never be upgraded into. The test builds this command the way the release
// does and runs it through the updater's own validator.
func TestVersionOutputIsAReleaseCompatibilityContract(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	root := moduleRoot(t)
	goreleaser, err := os.ReadFile(filepath.Join(root, ".goreleaser.yaml")) // #nosec G304 -- the module's own release config
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(goreleaser, []byte(`-X main.version={{.Version}}`)) {
		t.Fatal("the release must stamp the bare version ({{.Version}}, X.Y.Z) into main.version; a v prefix breaks the version contract")
	}

	bin := filepath.Join(t.TempDir(), "dropin-miner")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command("go", "build", "-trimpath", "-ldflags", "-X main.version=0.3.0", "-o", bin, "./cmd/dropin-miner") // #nosec G204 -- fixed arguments
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	got, err := selfupdate.CandidateVersion(context.Background(), selfupdate.ExecRunner{}, bin)
	if err != nil {
		t.Fatalf("a release build's version command must satisfy the updater's contract: %v", err)
	}
	if got.String() != "0.3.0" {
		t.Errorf("version reported %s, want 0.3.0", got)
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(bin, "version") // #nosec G204 -- the binary this test built
	cmd.Env = []string{"LANG=C", "LC_ALL=C", "SYSTEMROOT=" + os.Getenv("SYSTEMROOT")}
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil || stdout.String() != "dropin-miner 0.3.0\n" || stderr.Len() != 0 {
		t.Errorf("version: err=%v stdout=%q stderr=%q; want exactly %q and nothing on stderr", err, stdout.String(), stderr.String(), "dropin-miner 0.3.0\n")
	}
}
