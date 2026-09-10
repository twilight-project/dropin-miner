package main

// The Codex sandbox fix: a search's mining observation is written under the
// tokendrop home, which Codex's default workspace-write sandbox denies. The
// installer now widens that sandbox, and a blocked write is reported loudly
// instead of silently. All identifiers here are synthetic.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIntakeWriteBlockedClassifiesSandboxDenials(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"permission (Linux/Landlock EACCES)", fs.ErrPermission, true},
		{"wrapped permission", fmt.Errorf("mkdir: %w", fs.ErrPermission), true},
		{"read-only fs (macOS/Seatbelt EROFS)", errors.New("open x.tmp: read-only file system"), true},
		{"unrelated failure", io.ErrUnexpectedEOF, false},
	}
	for _, c := range cases {
		if got := intakeWriteBlocked(c.err); got != c.want {
			t.Errorf("%s: intakeWriteBlocked=%v want %v", c.name, got, c.want)
		}
	}
}

// A real filesystem denial (not a synthetic error) must classify as blocked,
// so the loud, actionable message fires rather than the generic one.
func TestWriteIntakeToUnwritableDirIsClassifiedBlocked(t *testing.T) {
	if !posixModes || os.Geteuid() == 0 {
		t.Skip("needs POSIX mode bits and a non-root uid to make a dir unwritable")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil { // #nosec G302 -- deliberately unwritable, to force a real permission denial
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) }) // #nosec G302 -- restore so t.TempDir cleanup can remove it

	_, err := writeIntake(filepath.Join(parent, "intake"), intakeRecord{RequestID: "r1", FinishedAt: time.Now()})
	if err == nil {
		t.Fatal("expected the write into a read-only parent to fail")
	}
	if !intakeWriteBlocked(err) {
		t.Errorf("a real permission denial was not classified as a sandbox block: %v", err)
	}
}

// sandboxTestConfig writes a real config (loadConfig reads the real fs) whose
// mining/miner dirs all sit under one tokendrop home, and returns that home.
func sandboxTestConfig(t *testing.T) (cfgPath, home string) {
	t.Helper()
	home = t.TempDir()
	for _, d := range []string{"state", "spool", "intake", "sessions"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	doc := fmt.Sprintf(`
[mining]
enabled = true
as_url = "https://as.example.invalid"
chain_id = "twilight-1"
slot_id = 7
state_dir = %q
spool_dir = %q

[miner]
enabled = true
router_url = "https://router.example.invalid"
intake_dir = %q
sessions_dir = %q
`, filepath.Join(home, "state"), filepath.Join(home, "spool"),
		filepath.Join(home, "intake"), filepath.Join(home, "sessions"))
	cfgPath = filepath.Join(home, "tokendrop.toml")
	if err := os.WriteFile(cfgPath, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, home
}

func TestCodexInstallConfiguresSandboxAndUninstallRemovesIt(t *testing.T) {
	cfgPath, home := sandboxTestConfig(t)
	m, ops := newFakeMachine("codex")
	// Codex already has an unrelated setting the tool must preserve.
	m.files["/home/u/.codex/config.toml"] = []byte("model = \"gpt-5\"\n")

	if code, out, _ := runAgents(t, ops, nil, "install", "-config", cfgPath, "-yes"); code != exitOK {
		t.Fatalf("install: %d\n%s", code, out)
	}
	got := string(m.files["/home/u/.codex/config.toml"])
	for _, want := range []string{
		"model = \"gpt-5\"",                                  // preserved
		agentsMarkerBegin,                                    // our block
		"[sandbox_workspace_write]",                          //
		"network_access = true",                              // both restrictions lifted
		"writable_roots = [" + fmt.Sprintf("%q", home) + "]", // the tokendrop home
		agentsMarkerEnd,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config.toml missing %q:\n%s", want, got)
		}
	}

	// Idempotent: a second install changes nothing.
	before := got
	if code, _, _ := runAgents(t, ops, nil, "install", "-config", cfgPath, "-yes"); code != exitOK {
		t.Fatal("second install failed")
	}
	if after := string(m.files["/home/u/.codex/config.toml"]); after != before {
		t.Errorf("second install was not idempotent:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}

	// Uninstall removes only our block; the user's setting survives.
	if code, _, _ := runAgents(t, ops, nil, "uninstall", "-config", cfgPath, "-yes"); code != exitOK {
		t.Fatal("uninstall failed")
	}
	left := string(m.files["/home/u/.codex/config.toml"])
	if strings.Contains(left, agentsMarkerBegin) || strings.Contains(left, "sandbox_workspace_write") {
		t.Errorf("uninstall left our block behind:\n%s", left)
	}
	if !strings.Contains(left, "model = \"gpt-5\"") {
		t.Errorf("uninstall dropped the user's own setting:\n%s", left)
	}
}

func TestCodexInstallRefusesForeignSandboxTable(t *testing.T) {
	cfgPath := func() string { c, _ := sandboxTestConfig(t); return c }()
	m, ops := newFakeMachine("codex")
	foreign := "[sandbox_workspace_write]\nnetwork_access = false\nwritable_roots = [\"/tmp/mine\"]\n"
	m.files["/home/u/.codex/config.toml"] = []byte(foreign)

	// A refusal is a partial failure: non-zero exit (as with any refused surface).
	code, out, _ := runAgents(t, ops, nil, "install", "-config", cfgPath, "-yes")
	if code != exitTransport {
		t.Fatalf("install exit: %d want exitTransport\n%s", code, out)
	}
	if !strings.Contains(out, "refused") || !strings.Contains(out, "already defines [sandbox_workspace_write]") {
		t.Errorf("expected a refusal with a paste-able snippet, got:\n%s", out)
	}
	// The foreign table is left exactly as it was.
	if string(m.files["/home/u/.codex/config.toml"]) != foreign {
		t.Errorf("the user's own sandbox table was modified:\n%s", m.files["/home/u/.codex/config.toml"])
	}
}

// When there is no readable config or mining is off, the sandbox does not
// matter (nothing records) — the installer writes no block and just notes it.
func TestCodexInstallWithoutMiningWritesNoSandboxBlock(t *testing.T) {
	m, ops := newFakeMachine("codex")
	if code, out, _ := runAgents(t, ops, nil, "install", "-config", testCfg, "-yes"); code != exitOK {
		t.Fatalf("install: %d\n%s", code, out)
	}
	if b, ok := m.files["/home/u/.codex/config.toml"]; ok {
		t.Errorf("wrote a config.toml with no mining configured:\n%s", b)
	}
}
