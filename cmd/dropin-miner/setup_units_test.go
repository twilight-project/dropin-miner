package main

// The pieces of setup that decide something on their own, tested directly:
// TOML quoting, the profile block editor, the Windows delta journal, the
// identity bundle's conflict rule, and -with's resolver.

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

func TestTOMLStringRoundTripsEveryCharacterClass(t *testing.T) {
	for _, in := range []string{
		"plain",
		`C:\Users\me\.tokendrop`,
		`a "quoted" path`,
		`back\slash and \"both\"`,
		"tab\tnew\nline\rcarriage\bbell\fform",
		"control \x01 and \x1f and del \x7f",
		"unicode ünïcødé — 日本語",
		`$HOME; rm -rf / & echo '|' ` + "`x`",
		"",
	} {
		q, err := tomlString(in)
		if err != nil {
			t.Fatalf("tomlString(%q): %v", in, err)
		}
		var doc struct{ V string }
		if _, err := toml.Decode("V = "+q+"\n", &doc); err != nil {
			t.Fatalf("tomlString(%q) = %s does not parse: %v", in, q, err)
		}
		if doc.V != in {
			t.Errorf("round trip of %q gave %q (via %s)", in, doc.V, q)
		}
	}
	if _, err := tomlString("bad \xff utf-8"); err == nil {
		t.Error("invalid UTF-8 was rendered instead of refused")
	}
}

// Every path in the fresh config is quoted: a home that holds a quote and a
// backslash still produces a config pkg/config loads, with the path intact.
func TestFreshConfigQuotesEveryPath(t *testing.T) {
	dir := t.TempDir()
	home := `/tmp/we ird"q\b$;&'x/.tokendrop`
	v, err := resolveSetupValues(home, func(string) string { return "" }, false)
	if err != nil {
		t.Fatal(err)
	}
	data, err := renderFreshConfig(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateConfigFile(dir)(data); err != nil {
		t.Fatalf("fresh config for %q does not load: %v\n%s", home, err, data)
	}
	var doc struct {
		Mining struct {
			StateDir string `toml:"state_dir"`
		} `toml:"mining"`
	}
	if _, err := toml.Decode(string(data), &doc); err != nil || doc.Mining.StateDir != filepath.Join(home, "state") {
		t.Fatalf("state_dir = %q (%v), want %q", doc.Mining.StateDir, err, filepath.Join(home, "state"))
	}
}

func TestMigrationPolicyUnits(t *testing.T) {
	dir := t.TempDir()
	validate := validateConfigFile(dir)
	v, _ := resolveSetupValues(filepath.Join(dir, "home"), func(string) string { return "" }, false)
	path := filepath.Join(dir, "tokendrop.toml")
	mining := "[mining]\nas_url = \"https://as.example\"\nchain_id = \"c\"\nslot_id = 1\nstate_dir = \"/s\"\n"

	// [miner] present, even as a dotted key at the root: left.
	plan, err := planSetupConfig(path, []byte(mining+"\n[miner]\nenabled = false\n"), v, validate)
	if err != nil || plan.outcome != configLeft || plan.data != nil {
		t.Fatalf("[miner] table: %+v %v", plan, err)
	}
	// The text "[miner]" inside a comment is not a table.
	plan, err = planSetupConfig(path, []byte(mining+"# [miner] later\n"), v, validate)
	if err != nil || plan.outcome != configMigrated || strings.Join(plan.added, ",") != "[platform],[miner]" {
		t.Fatalf("commented [miner]: %+v %v", plan, err)
	}
	// Invalid, whatever it says.
	_, err = planSetupConfig(path, []byte("[miner]\nenabled = true\n[miner]\n"), v, validate)
	if !errors.Is(err, errConfigInvalid) || !strings.Contains(err.Error(), path) {
		t.Fatalf("invalid config: %v", err)
	}
	// A migration whose result would not load is refused, not published: no
	// [mining] AS, so [miner] enabled cannot be valid.
	_, err = planSetupConfig(path, []byte("[log]\nlevel = \"info\"\n"), v, validate)
	if err == nil || !strings.Contains(err.Error(), "left as it is") {
		t.Fatalf("unloadable migration: %v", err)
	}
}

func TestRewriteProfileEditsExactlyOneWellFormedBlock(t *testing.T) {
	block := profileBlock([]string{"export TOKENDROP_CONFIG='/x'"})
	old := profileBlock([]string{"export TOKENDROP_CONFIG='/old'"})

	got, err := rewriteProfile(nil, block)
	if err != nil || string(got) != block {
		t.Fatalf("empty profile: %q %v", got, err)
	}
	got, err = rewriteProfile([]byte("export A=1"), block)
	if err != nil || string(got) != "export A=1\n"+block {
		t.Fatalf("append without a final newline: %q %v", got, err)
	}
	in := "before\n" + old + "after\n"
	got, err = rewriteProfile([]byte(in), block)
	if err != nil || string(got) != "before\n"+block+"after\n" {
		t.Fatalf("replace in place: %q %v", got, err)
	}
	if again, _ := rewriteProfile(got, block); !bytes.Equal(again, got) {
		t.Fatalf("rewrite is not idempotent: %q", again)
	}

	for name, body := range map[string]string{
		"start without end": "a\n" + profileMarkerStart + "\nexport X=1\nb\n",
		"end without start": "a\n" + profileMarkerEnd + "\n",
		"two starts":        profileMarkerStart + "\n" + old,
		"two blocks":        old + old,
		"end before start":  profileMarkerEnd + "\n" + profileMarkerStart + "\n",
	} {
		got, err := rewriteProfile([]byte(body), block)
		if !errors.Is(err, errProfileMalformed) || got != nil {
			t.Errorf("%s: got %q, %v; want a refusal and no bytes", name, got, err)
		}
	}
}

// The quoting survives a real shell: every metacharacter a path can hold is
// read back literally.
func TestShellQuoteRoundTripsThroughSh(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX shell profile on Windows")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal("sh is required")
	}
	for _, in := range []string{`plain`, `sp ace`, `it's`, `"dq"`, `back\slash`, `$HOME;&|<>(){}*?[]~!#` + "`x`", "new\nline"} {
		out, err := exec.Command(sh, "-c", "printf '%s' "+shellQuote(in)).Output() // #nosec G204 -- sh from PATH, argument built by the function under test
		if err != nil || string(out) != in {
			t.Errorf("shellQuote(%q) read back as %q (%v)", in, out, err)
		}
	}
}

func TestUserEnvironmentJournalRecordsDeltas(t *testing.T) {
	const bin = `C:\Users\me\.tokendrop\bin`
	const cfg = `C:\Users\me\.tokendrop\tokendrop.toml`
	dir := t.TempDir()
	journal := filepath.Join(dir, setupEnvJournalFile)

	// Absent PATH entry, absent TOKENDROP_CONFIG: setup adds both and says so.
	env := newFakeUserEnv()
	env.values["Path"] = `C:\Windows`
	c, err := planUserEnvironment(env, nil, bin, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !c.journal.Path.AddedBySetup || c.journal.TokendropConfig.PreviousPresent || !c.addPath || !c.setConfig {
		t.Fatalf("fresh plan: %+v", c)
	}
	if err := applyUserEnvironment(env, journal, c); err != nil {
		t.Fatal(err)
	}
	if env.values["Path"] != `C:\Windows;`+bin || env.values["TOKENDROP_CONFIG"] != cfg || env.broadcasts != 1 {
		t.Fatalf("environment after apply: %v broadcasts=%d", env.values, env.broadcasts)
	}
	written, _ := os.ReadFile(journal) // #nosec G304 -- this test's own temp file

	// Rerun: nothing to change, and the journal is not rewritten.
	prior, err := readEnvJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	c, _ = planUserEnvironment(env, prior, bin, cfg)
	if c.writeJournal || c.addPath || c.setConfig {
		t.Fatalf("rerun plans a change: %+v", c)
	}
	if !c.journal.Path.AddedBySetup {
		t.Error("rerun forgot setup added the PATH entry")
	}
	if err := applyUserEnvironment(env, journal, c); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(journal); !bytes.Equal(again, written) { // #nosec G304 -- as above
		t.Error("journal rewritten on a no-op rerun")
	}

	// A PATH entry already present — case and a trailing separator aside —
	// and a TOKENDROP_CONFIG of the participant's own.
	env = newFakeUserEnv()
	env.values["Path"] = `C:\Windows;c:\users\ME\.tokendrop\bin\`
	env.values["TOKENDROP_CONFIG"] = `D:\mine.toml`
	c, _ = planUserEnvironment(env, nil, bin, cfg)
	if c.journal.Path.AddedBySetup || c.addPath {
		t.Errorf("pre-existing PATH entry recorded as setup's: %+v", c.journal.Path)
	}
	if !c.journal.TokendropConfig.PreviousPresent || c.journal.TokendropConfig.PreviousValue != `D:\mine.toml` {
		t.Errorf("previous TOKENDROP_CONFIG not recorded: %+v", c.journal.TokendropConfig)
	}
	journal2 := filepath.Join(dir, "second.json")
	if err := applyUserEnvironment(env, journal2, c); err != nil {
		t.Fatal(err)
	}
	// On the rerun TOKENDROP_CONFIG holds setup's own value; the journal must
	// still name the participant's.
	prior, _ = readEnvJournal(journal2)
	c, _ = planUserEnvironment(env, prior, bin, cfg)
	if c.journal.TokendropConfig.PreviousValue != `D:\mine.toml` || !c.journal.TokendropConfig.PreviousPresent {
		t.Errorf("rerun replaced the original with setup's own value: %+v", c.journal.TokendropConfig)
	}
	if c.journal.Path.AddedBySetup {
		t.Error("rerun turned a pre-existing PATH entry into setup's")
	}

	// A journal that cannot be read is an error, never a fresh start.
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readEnvJournal(bad); err == nil {
		t.Error("an unreadable journal was accepted")
	}
}

func TestIdentityBundleConflictsWithADestinationAgentJSON(t *testing.T) {
	for _, existing := range []string{"agent.json", "refresh.token", "registration_pending.json", "participation.secret", credentialsFile} {
		t.Run(existing, func(t *testing.T) {
			root := t.TempDir()
			src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
			writeInstallation(t, src, "identity")
			rel := filepath.Join("state", existing)
			if existing == credentialsFile {
				rel = existing
			}
			p := filepath.Join(dst, rel)
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("destination's own"), 0o600); err != nil {
				t.Fatal(err)
			}
			before := snapshotTree(t, src)
			a := &adoption{src: src, dst: dst, now: time.Now(), out: io.Discard, restrict: restrictToOwner}
			var conflict *identityConflict
			if err := a.run(); !errors.As(err, &conflict) || conflict.Destination != dst || conflict.Source != src || conflict.Evidence != p {
				t.Fatalf("typed conflict = %+v, want destination %s, source %s, evidence %s", conflict, dst, src, p)
			}
			if len(a.moved) != 0 {
				t.Errorf("moved %v despite the conflict", a.moved)
			}
			if after := snapshotTree(t, src); len(after) != len(before) {
				t.Error("the source changed despite the conflict")
			}
			if got, _ := os.ReadFile(p); string(got) != "destination's own" { // #nosec G304 -- this test's own temp file
				t.Error("the destination's identity file changed")
			}
		})
	}
}

func TestIdentityBundleMovesStateAndKeyTogether(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	writeInstallation(t, src, "identity")
	if err := os.MkdirAll(filepath.Join(dst, "state"), 0o700); err != nil { // setup's own empty state/
		t.Fatal(err)
	}
	a := &adoption{src: src, dst: dst, now: time.Now(), out: io.Discard, restrict: restrictToOwner}
	if err := a.run(); err != nil {
		t.Fatalf("unexpected adoption error: %v", err)
	}
	for _, rel := range []string{filepath.Join("state", "refresh.token"), filepath.Join("state", "dpop.key"), credentialsFile} {
		if !lexists(filepath.Join(dst, rel)) || lexists(filepath.Join(src, rel)) {
			t.Errorf("%s did not move with the bundle", rel)
		}
	}
}

// fakeIntegration is a targetIntegration, registered only for the test that
// proves setup -with reaches a kind agents -client never can.
type fakeIntegration struct{}

func (fakeIntegration) ID() string       { return "fake-integration" }
func (fakeIntegration) Label() string    { return "Fake integration" }
func (fakeIntegration) Kind() targetKind { return targetIntegration }
func (fakeIntegration) Detect(agentOps, agentPaths, func(string) string) string {
	return "fake-integration on PATH"
}
func (fakeIntegration) PlanInstall(ops agentOps, _ agentPaths, _ binEntry, _ func(string) string, p *agentPlan) {
	planWrite(ops, "Fake integration", filepath.Join(ops.home, ".fake-integration"), []byte("x"), 0o600, "marker", p)
}
func (fakeIntegration) PlanUninstall(agentOps, agentPaths, binEntry, func(string) string, *agentPlan) {
}
func (fakeIntegration) Status(agentOps, agentPaths, binEntry) targetStatus { return targetStatus{} }

func TestSetupWithReachesEveryTargetKindAndDeduplicates(t *testing.T) {
	orig := installTargets
	installTargets = append(append([]installTarget(nil), orig...), fakeIntegration{})
	t.Cleanup(func() { installTargets = orig })

	ops := realAgentOps()
	ops.home = t.TempDir()
	ops.lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	paths := ops.paths(func(string) string { return "" })
	none := func(string) string { return "" }

	got, _, explicit, err := setupTargets(ops, paths, none, []string{"fake-integration", "codex", " Codex ", "fake-integration"}, true)
	if err != nil || !explicit {
		t.Fatalf("-with an integration: %v explicit=%v", err, explicit)
	}
	var ids []string
	for _, t := range got {
		ids = append(ids, t.ID())
	}
	if strings.Join(ids, ",") != "fake-integration,codex" {
		t.Fatalf("selection = %v, want [fake-integration codex] in first-occurrence order", ids)
	}

	// Detection is host-only: an integration never arrives uninvited.
	detected, _, explicit, err := setupTargets(ops, paths, none, nil, false)
	if err != nil || explicit || len(detected) != 0 {
		t.Fatalf("default selection with nothing on PATH: %v %v %v", detected, explicit, err)
	}

	if _, _, _, err := setupTargets(ops, paths, none, []string{"nope"}, false); err == nil || !strings.Contains(err.Error(), "fake-integration") {
		t.Fatalf("unknown id error does not list every kind: %v", err)
	}
}
