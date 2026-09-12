package main

// Corrections 4 and 5: what `action: connect` means, and the one thing
// `connect -json` must refuse to do.

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/twilight-project/dropin-miner/pkg/auth"
)

// ── 4. the connect action names a workflow, not an absence ──────────────

// All three of these are the registration/claim workflow needing
// attention. Narrowing the action to "there is no registration" would
// leave the other two with nothing honest to say.
func TestTheConnectActionCoversTheWholeRegistrationWorkflow(t *testing.T) {
	for _, status := range []string{"unclaimed", "expired"} {
		code, retryable, action := connectOutcome(exitOK, status)
		if action != actionConnect {
			t.Errorf("connectOutcome(ok, %q) action = %q, want %q", status, action, actionConnect)
		}
		if retryable {
			t.Errorf("connectOutcome(ok, %q) is retryable; a human step is not a retry", status)
		}
		if code == "" {
			t.Errorf("connectOutcome(ok, %q) has no code", status)
		}
	}
	// And a search with no local credential is the same workflow.
	if c := classifySearch(searchOutcome{Fault: faultNoCredential}); c.Action != actionConnect {
		t.Errorf("a missing search credential gives action %q, want %q", c.Action, actionConnect)
	}
}

// The guidance must not tell an agent that action connect proves there is
// no registration — two of the three states it covers have one.
func TestGuidanceDoesNotClaimConnectMeansNoRegistrationExists(t *testing.T) {
	entry := guidanceEntry()
	narrow := regexp.MustCompile(`(?i)connect[^.\n]{0,60}(no registration here at all|is not registered|never registered here)`)
	for _, s := range agentSurfaces {
		text := instructionTextFor(t, s.id, entry)
		if m := narrow.FindString(text); m != "" {
			t.Errorf("%s is told the narrow connect contract (%q):\n%s", s.label, m, text)
		}
	}
}

// And the 401 rule survives the broadening: it still maps to login.
func TestGuidanceStillSendsA401ToLoginNotConnect(t *testing.T) {
	entry := guidanceEntry()
	for _, s := range agentSurfaces {
		if s.id == "opencode" {
			continue // the snippet is short by design; the skill carries the 401 rule
		}
		flat := strings.Join(strings.Fields(instructionTextFor(t, s.id, entry)), " ")
		if !strings.Contains(flat, "401 maps to `login`") {
			t.Errorf("%s is not told that a 401 maps to login:\n%s", s.label, flat)
		}
		if !strings.Contains(flat, "does not mean this installation has never registered") {
			t.Errorf("%s is not told that a 401 does not prove absence of registration", s.label)
		}
	}
	// The classifier agrees.
	if c := classifySearch(routerOutcome(401, "unauthorized", "no")); c.Action != actionLogin {
		t.Errorf("a router 401 gives action %q, want %q", c.Action, actionLogin)
	}
}

// ── 5. -json must not manufacture a participant decision ────────────────

// countingPlatformStub is a local platform that refuses everything and
// counts what it was asked for. Zero calls is then an assertion rather
// than an absence of evidence: a test that merely did not crash proves
// nothing about whether a registration was attempted.
func countingPlatformStub(t *testing.T) (url string, calls *atomic.Int64) {
	t.Helper()
	calls = &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		t.Logf("the platform stub was called: %s %s", r.Method, r.URL.Path)
		http.Error(w, `{"error":"this test must never reach the platform"}`, http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, calls
}

// connectFixture writes a config whose state directory is fresh unless the
// caller seeds it. explicitMining writes mining.enabled into the file,
// which is the scripted answer to the question connect would otherwise ask.
func connectFixture(t *testing.T, explicitMining string) (cfgPath, stateDir string) {
	t.Helper()
	root := t.TempDir()
	stateDir = filepath.Join(root, "state")
	cfgPath = filepath.Join(root, "tokendrop.toml")

	// The platform host is pinned at a local stub that refuses everything.
	//
	// Without this the config falls back to the real platform.nyks.dev,
	// and the gate under test is the only thing standing between this test
	// and a live registration attempt — so the moment a mutation disables
	// the gate, the test dials a real host. A fixture whose safety depends
	// on the code under test being correct is not a fixture. It also makes
	// the mutation's red arrive in milliseconds instead of three minutes.
	platform, _ := countingPlatformStub(t)

	toml := "[mining]\n"
	if explicitMining != "" {
		toml += "enabled = " + explicitMining + "\n"
	}
	toml += `state_dir = "` + filepath.ToSlash(stateDir) + `"
spool_dir = "` + filepath.ToSlash(filepath.Join(root, "spool")) + `"

[platform]
base_url = "` + platform + `"
agents_api_url = "` + platform + `"
`
	if err := os.WriteFile(cfgPath, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, stateDir
}

// The heart of it: selecting an output format must not select an answer.
func TestConnectJSONRefusesToInventAFirstMiningDecision(t *testing.T) {
	cfgPath, stateDir := connectFixture(t, "")
	var out, errOut bytes.Buffer
	code := cmdConnect([]string{"-config", cfgPath, "-json"}, strings.NewReader(""), &out, &errOut, noEnv)

	if code != exitUsage {
		t.Fatalf("exit %d, want %d: %s", code, exitUsage, out.String())
	}
	env := decodeCommandEnvelope(t, out.String(), "connect")
	if env["code"] != "human_decision_required" {
		t.Errorf("code %v, want human_decision_required", env["code"])
	}
	if env["action"] != actionConnect {
		t.Errorf("action %v, want %q", env["action"], actionConnect)
	}
	if env["ok"] != false || env["retryable"] != false {
		t.Errorf("header: %v", env)
	}
	// Nothing was decided, and nothing was written.
	if store, err := auth.OpenStoreExisting(stateDir); err == nil {
		if d := store.ReadMiningDecision(); d.State != auth.MiningUndecided {
			t.Errorf("a mining decision was persisted anyway: %s", d.State)
		}
		if _, ok, _ := store.LoadAgentRegistration(); ok {
			t.Error("a registration was created despite the refusal")
		}
	}
}

// ── B.3 rebuild x -json: the pre-check cannot see this one ──────────────
//
// connectNeedsHumanDecision is a pure disk read; it never calls
// /v1/agents/me. A corrupt-or-absent record beside a stored platform
// credential therefore reaches its "preflightFreshRegistration refuses, so
// nothing to ask" branch — true before B.3, no longer true after it, since
// the rebuild can now succeed instead of refusing. If that rebuild reveals
// an EXPIRED identity, connectRun falls into the existing expired-
// replacement branch, which (interactively) asks the mining question. Under
// -json that would be non-interactive and could persist an invented
// decision and mint a live Register call. The guard lives inside
// connectRun itself (gated on the explicit machine flag cmdConnect passes),
// because the pre-check structurally cannot know this branch is even
// reachable until after the network call it is not allowed to make.
func TestConnectJSONRefusesToDecideAfterRebuildingAnExpiredIdentity(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")
	cfg := mustLoadConfig(t, cfgPath)

	_, key := registerAgent(t, platform)
	platform.setStatus("expired")
	setupLostRegistration(t, cfg, key, true) // corrupt agent.json beside a stored platform credential
	registerCallsBefore, _, _ := platform.counts()

	var out, errOut bytes.Buffer
	code := cmdConnect([]string{"-config", cfgPath, "-json"}, strings.NewReader(""), &out, &errOut, noEnv)

	if code != exitUsage {
		t.Fatalf("exit %d, want %d: out=%s err=%s", code, exitUsage, out.String(), errOut.String())
	}
	env := decodeCommandEnvelope(t, out.String(), "connect")
	if env["code"] != "human_decision_required" {
		t.Errorf("code %v, want human_decision_required", env["code"])
	}
	if env["action"] != actionConnect {
		t.Errorf("action %v, want %q", env["action"], actionConnect)
	}
	if env["ok"] != false || env["retryable"] != false {
		t.Errorf("header: %v", env)
	}
	if registerCalls, _, _ := platform.counts(); registerCalls != registerCallsBefore {
		t.Fatalf("Register calls = %d, want unchanged from %d — zero Register calls", registerCalls, registerCallsBefore)
	}
	store, err := auth.OpenStoreExisting(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if d := store.ReadMiningDecision(); d.State != auth.MiningUndecided {
		t.Errorf("a mining decision was persisted: %s", d.State)
	}
	// The rebuilt (expired) identity is allowed to remain persisted — only
	// the decision must stay unmade.
	reg, ok, rerr := store.LoadAgentRegistration()
	if rerr != nil || !ok || reg.Status != "expired" {
		t.Errorf("the recovered expired registration should remain on file: reg=%+v ok=%v err=%v", reg, ok, rerr)
	}
}

// connectNeedsHumanDecision is the gate, and it is what the wrapper calls
// before anything registers. Driving it directly keeps every case network-
// free while still being the production predicate.
func TestTheHumanDecisionGateMatchesWhereTheQuestionIsActuallyAsked(t *testing.T) {
	seedRegistration := func(t *testing.T, stateDir, status string) {
		t.Helper()
		store, err := auth.OpenStore(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SaveAgentRegistration(auth.AgentRegistration{
			AgentID: "agent-fictional", Status: status, ClaimURL: "https://portal.fictional.test/c",
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name     string
		explicit string
		setup    func(t *testing.T, stateDir string)
		want     bool
	}{
		{
			name: "fresh state, no explicit choice: the question would be asked",
			want: true,
		},
		{
			name:  "an expired registration is re-registered, so it would be asked",
			setup: func(t *testing.T, dir string) { seedRegistration(t, dir, "expired") },
			want:  true,
		},
		{
			// The config answered it. This is the established scripted
			// path and must stay open.
			name:     "explicit mining.enabled = true",
			explicit: "true",
			want:     false,
		},
		{
			name:     "explicit mining.enabled = false",
			explicit: "false",
			want:     false,
		},
		{
			// A persisted decision is authoritative, as everywhere else.
			name: "a persisted decision already exists",
			setup: func(t *testing.T, dir string) {
				store, err := auth.OpenStore(dir)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.SaveMiningEnabled(true); err != nil {
					t.Fatal(err)
				}
			},
			want: false,
		},
		{
			// Nothing is registered afresh, so nothing is asked; this
			// must keep resuming.
			name:  "an unclaimed registration is only polled",
			setup: func(t *testing.T, dir string) { seedRegistration(t, dir, "unclaimed") },
			want:  false,
		},
		{
			name:  "a claimed registration is settled",
			setup: func(t *testing.T, dir string) { seedRegistration(t, dir, "claimed") },
			want:  false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, stateDir := connectFixture(t, tc.explicit)
			if tc.setup != nil {
				tc.setup(t, stateDir)
			}
			if got := connectNeedsHumanDecision(cfgPath, false, noEnv); got != tc.want {
				t.Errorf("connectNeedsHumanDecision = %v, want %v", got, tc.want)
			}
		})
	}
}

// The gate reads. Deciding not to decide must not itself decide.
func TestTheHumanDecisionGateWritesNothing(t *testing.T) {
	cfgPath, stateDir := connectFixture(t, "")
	for i := 0; i < 3; i++ {
		if !connectNeedsHumanDecision(cfgPath, false, noEnv) {
			t.Fatal("the gate stopped asking for a decision after being consulted")
		}
	}
	if _, err := os.Stat(stateDir); err == nil {
		entries, _ := os.ReadDir(stateDir)
		for _, e := range entries {
			if strings.Contains(e.Name(), "mining") || strings.Contains(e.Name(), "agent") {
				t.Errorf("the gate created %s", e.Name())
			}
		}
	}
}

// Selecting -json alone changes nothing about the decision path when the
// decision is already answered: the same run, the same outcome, plus an
// envelope.
func TestSelectingJSONDoesNotChangeAnAnsweredDecision(t *testing.T) {
	for _, explicit := range []string{"true", "false"} {
		t.Run("mining.enabled="+explicit, func(t *testing.T) {
			cfgPath, stateDir := connectFixture(t, explicit)
			if connectNeedsHumanDecision(cfgPath, false, noEnv) {
				t.Fatal("an explicitly configured choice was treated as unanswered")
			}
			// The gate lets it through; the decision the flow would make
			// is the configured one, not one -json invented.
			cfg, _, err := loadConfig(cfgPath, noEnv)
			if err != nil {
				t.Fatal(err)
			}
			if !cfg.MiningEnabledExplicit {
				t.Fatal("the fixture did not set mining.enabled explicitly")
			}
			_ = stateDir
		})
	}
}

// ── the corrupt-registration hole ───────────────────────────────────────
//
// A corrupt agent.json is not "a registration this run will sort out". It
// is the state connectRun's recovery treats as no usable registration at
// all, which drops straight through to the fresh-registration branch —
// preflight, ask, Register. Under -json that ask is non-interactive, so
// the implicit mining default gets persisted and a real agent gets minted.
// The gate has to read "cannot be read" the same way the recovery does.

const corruptRegistrationBytes = "{ this is not valid json"

func writeCorruptRegistration(t *testing.T, stateDir string) {
	t.Helper()
	// Through the store, so the file lands where the store looks for it
	// with the mode the store uses, then overwritten with bytes it cannot
	// decode.
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgentRegistration(auth.AgentRegistration{AgentID: "placeholder", Status: "unclaimed"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(stateDir, "agent.json")
	if err := os.WriteFile(path, []byte(corruptRegistrationBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.LoadAgentRegistration(); !errors.Is(err, auth.ErrAgentRegistrationCorrupt) {
		t.Fatalf("the fixture did not produce a corrupt registration: %v", err)
	}
}

func TestConnectJSONRefusesOnACorruptRegistrationBeforeRegistering(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	platformURL, calls := countingPlatformStub(t)
	cfgPath := filepath.Join(root, "tokendrop.toml")
	// No explicit mining.enabled, and no credential to block a fresh
	// registration: exactly the state that could reach Register.
	toml := `[mining]
state_dir = "` + filepath.ToSlash(stateDir) + `"
spool_dir = "` + filepath.ToSlash(filepath.Join(root, "spool")) + `"

[miner]
router_url = "https://router.fictional.test"
intake_dir = "` + filepath.ToSlash(filepath.Join(root, "miner", "intake")) + `"

[platform]
base_url = "` + platformURL + `"
agents_api_url = "` + platformURL + `"
`
	if err := os.WriteFile(cfgPath, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	writeCorruptRegistration(t, stateDir)

	var out, errOut bytes.Buffer
	code := cmdConnect([]string{"-config", cfgPath, "-json"}, strings.NewReader(""), &out, &errOut, noEnv)

	if code != exitUsage {
		t.Fatalf("exit %d, want %d: %s", code, exitUsage, out.String())
	}
	env := decodeCommandEnvelope(t, out.String(), "connect")
	if env["code"] != "human_decision_required" {
		t.Errorf("code %v, want human_decision_required", env["code"])
	}
	if env["action"] != actionConnect {
		t.Errorf("action %v, want %q", env["action"], actionConnect)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("the platform was called %d time(s); a registration was attempted", n)
	}
	store, err := auth.OpenStoreExisting(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if d := store.ReadMiningDecision(); d.State != auth.MiningUndecided {
		t.Errorf("a mining decision was persisted: %s", d.State)
	}
	// The gate reads. It must not repair, replace or rewrite what it
	// could not decode.
	raw, err := os.ReadFile(filepath.Join(stateDir, "agent.json")) // #nosec G304 -- this test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != corruptRegistrationBytes {
		t.Errorf("the corrupt registration was rewritten:\n got %q\nwant %q", raw, corruptRegistrationBytes)
	}
}

// The nearby safety cases: the guard must be conservative about a corrupt
// registration without becoming a blanket refusal.
func TestTheGateAroundACorruptRegistration(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, stateDir string)
		want  bool
	}{
		{
			name:  "corrupt, undecided, nothing else: the question would be asked",
			setup: func(t *testing.T, dir string) { writeCorruptRegistration(t, dir) },
			want:  true,
		},
		{
			// A decision on file answers the question however unreadable
			// the registration is. The gate must not invent a second one.
			name: "corrupt but a decision is already persisted",
			setup: func(t *testing.T, dir string) {
				writeCorruptRegistration(t, dir)
				store, err := auth.OpenStore(dir)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.SaveMiningEnabled(true); err != nil {
					t.Fatal(err)
				}
			},
			want: false,
		},
		{
			// Register already happened for a pending registration;
			// recovery republishes it and asks nothing. Blocking that
			// would break the journal this guard has no business touching.
			name: "corrupt but a valid pending registration exists",
			setup: func(t *testing.T, dir string) {
				writeCorruptRegistration(t, dir)
				store, err := auth.OpenStore(dir)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.SavePendingRegistration(auth.PendingRegistration{
					AgentID: "agent-pending", Key: "sr-pending", Status: "unclaimed",
					ClaimURL: "https://portal.fictional.test/c",
				}); err != nil {
					t.Fatal(err)
				}
			},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, stateDir := connectFixture(t, "")
			if err := os.MkdirAll(stateDir, 0o700); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, stateDir)
			if got := connectNeedsHumanDecision(cfgPath, false, noEnv); got != tc.want {
				t.Errorf("connectNeedsHumanDecision = %v, want %v", got, tc.want)
			}
		})
	}
}

// Ordinary connect is untouched: a corrupt registration still goes through
// the existing recovery path, which reports the credential conflict rather
// than refusing with the machine code.
func TestOrdinaryConnectKeepsItsCorruptRegistrationRecovery(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	minerDir := filepath.Join(root, "miner")
	platformURL, _ := countingPlatformStub(t)
	cfgPath := filepath.Join(root, "tokendrop.toml")
	toml := `[mining]
state_dir = "` + filepath.ToSlash(stateDir) + `"
spool_dir = "` + filepath.ToSlash(filepath.Join(root, "spool")) + `"

[miner]
router_url = "https://router.fictional.test"
intake_dir = "` + filepath.ToSlash(filepath.Join(minerDir, "intake")) + `"

[platform]
base_url = "` + platformURL + `"
agents_api_url = "` + platformURL + `"
`
	if err := os.WriteFile(cfgPath, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	writeCorruptRegistration(t, stateDir)
	// A platform credential beside it: the pre-existing recovery refuses
	// to mint a second agent, and that refusal is what must survive.
	if err := os.MkdirAll(minerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeCredentials(filepath.Join(minerDir, "credentials.json"),
		credentials{APIKey: "sr-existing-platform-key"}); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := cmdConnect([]string{"-config", cfgPath}, strings.NewReader(""), &out, &errOut, noEnv)

	if code == exitOK {
		t.Fatalf("ordinary connect succeeded despite a credential conflict: %s %s", out.String(), errOut.String())
	}
	if strings.Contains(out.String(), "human_decision_required") {
		t.Error("ordinary connect emitted the machine refusal")
	}
	if !strings.Contains(errOut.String(), "could not be decoded") {
		t.Errorf("the pre-existing corrupt-registration report is gone: %q", errOut.String())
	}
	raw, err := os.ReadFile(filepath.Join(stateDir, "agent.json")) // #nosec G304 -- this test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != corruptRegistrationBytes {
		t.Errorf("ordinary connect rewrote the corrupt registration: %q", raw)
	}
}

// ── an invalid command is not a lifecycle state ─────────────────────────
//
// The human-decision pre-check runs before connectRun parses anything, so
// it has to know whether there is a command to check. With a parse failure
// there is no -config, and the gate would answer about the DEFAULT config
// and state directory — on a fresh machine, "undecided, nothing
// registered", which is exactly the shape that returns true. A mistyped
// flag would come back as human_decision_required.
//
// Every case below is run in an environment where the gate WOULD return
// true for a valid command, so a green result is the pre-parser refusing
// to answer rather than the gate happening to say no.
func TestMalformedMachineFlagsAreAUsageErrorNotALifecycleState(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"unknown flag", []string{"-not-a-real-flag"}},
		{"config with no value", []string{"-config"}},
		{"malformed boolean", []string{"-force=notbool"}},
		{"malformed json boolean", []string{"-json=maybe"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, stateDir := connectFixture(t, "")
			platformURL, calls := countingPlatformStub(t)
			_ = platformURL

			// Sanity: with a well-formed command this environment is
			// exactly the one the gate answers true for.
			if !connectNeedsHumanDecision(cfgPath, false, noEnv) {
				t.Fatal("the fixture is not a state the gate would answer; this case would prove nothing")
			}

			args := append([]string{"-json"}, tc.args...)
			var out, errOut bytes.Buffer
			code := cmdConnect(args, strings.NewReader(""), &out, &errOut, noEnv)

			if code != exitUsage {
				t.Fatalf("exit %d, want %d: %s", code, exitUsage, out.String())
			}
			env := decodeCommandEnvelope(t, out.String(), "connect")
			if env["code"] == "human_decision_required" {
				t.Errorf("an invalid command was reported as a lifecycle state: %s", out.String())
			}
			if env["code"] != "invalid_flags" {
				t.Errorf("code %v, want invalid_flags", env["code"])
			}
			if env["action"] != actionFixInput {
				t.Errorf("action %v, want %q", env["action"], actionFixInput)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("the platform was called %d time(s) for an invalid command", n)
			}
			if store, err := auth.OpenStoreExisting(stateDir); err == nil {
				if d := store.ReadMiningDecision(); d.State != auth.MiningUndecided {
					t.Errorf("an invalid command persisted a mining decision: %s", d.State)
				}
			}
			// An invalid command says nothing about this installation, so
			// nothing about it is reported.
			if _, ok := env["data"]; ok {
				t.Errorf("the envelope carries installation data for an invalid command: %s", out.String())
			}
		})
	}
}

// The other half: a well-formed command still reaches the gate.
func TestValidMachineFlagsStillReachTheHumanDecisionGate(t *testing.T) {
	cfgPath, _ := connectFixture(t, "")
	for _, args := range [][]string{
		{"-config", cfgPath, "-json"},
		{"-json", "-config", cfgPath},
		{"-json", "-config", cfgPath, "-name", "my agent"},
		{"-json", "-config", cfgPath, "-force"},
	} {
		var out, errOut bytes.Buffer
		code := cmdConnect(args, strings.NewReader(""), &out, &errOut, noEnv)
		if code != exitUsage {
			t.Fatalf("%v: exit %d", args, code)
		}
		env := decodeCommandEnvelope(t, out.String(), "connect")
		if env["code"] != "human_decision_required" {
			t.Errorf("%v: code %v, want human_decision_required", args, env["code"])
		}
	}
}

// The pre-parser and connectRun must accept the same grammar, or a command
// one of them rejects is handled by the other's rules.
func TestThePreParserAcceptsExactlyWhatConnectRunDoes(t *testing.T) {
	cfgPath, _ := connectFixture(t, "")
	for _, args := range [][]string{
		{"-config", cfgPath},
		{"-config", cfgPath, "-json"},
		{"-config", cfgPath, "-resume"},
		{"-config", cfgPath, "-force", "-name", "x"},
		{"-not-a-real-flag"},
		{"-force=notbool"},
		{"-config"},
	} {
		_, _, preErr := connectMachineFlags(args)
		// connectRun's own parse, observed through its exit code: it
		// returns exitUsage for exactly the arguments it cannot parse.
		var narration bytes.Buffer
		runCode := connectRun(args, strings.NewReader(""), io.Discard, &narration, noEnv, false)
		runRejected := runCode == exitUsage
		if (preErr != nil) != runRejected {
			t.Errorf("%v: pre-parser err=%v, connectRun rejected=%v", args, preErr, runRejected)
		}
	}
}
