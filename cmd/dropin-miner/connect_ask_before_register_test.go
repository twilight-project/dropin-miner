package main

// Issue: connect used to register before asking the mining question, so
// the hint register sends the platform (requested_scopes, which the
// claim page pre-ticks its mining grant from) could only ever come from
// the removed -mining flag — never from what was actually decided.
// connect.go now asks first (see cmdConnect's `!existed` branch) and
// builds the hint from the outcome via registrationHint. These tests
// cover the reordered first-run path directly: what register actually
// received, not just what askMiningQuestion returned in isolation
// (already covered exhaustively by TestAskMiningQuestion and friends).
import (
	"bufio"
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/twilight-project/dropin-miner/pkg/auth"
)

func TestRegistrationHint(t *testing.T) {
	cases := []struct {
		name    string
		outcome miningEnableOutcome
		want    []string
	}{
		{"enabled with an address", miningEnableOutcome{enabled: true, payoutAddress: "twilight1x"}, []string{"mining"}},
		{"enabled with no address yet", miningEnableOutcome{enabled: true}, []string{"mining"}},
		{"declined", miningEnableOutcome{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := registrationHint(tc.outcome)
			if (got == nil) != (tc.want == nil) || (got != nil && (len(got) != len(tc.want) || got[0] != tc.want[0])) {
				t.Fatalf("registrationHint(%+v) = %v, want %v", tc.outcome, got, tc.want)
			}
		})
	}
}

// A genuinely interactive terminal cannot be faked with a plain
// *bytes.Buffer in this codebase (isInteractive requires a real *os.File
// character device — see TestTerminalYesIsFollowedThroughToAnActualEnrollment's
// doc comment), so — matching that test's own approach — this drives
// askMiningQuestion directly with interactive forced true and checks
// what connect.go's own registrationHint does with its outcome, rather
// than trying to fake a terminal through cmdConnect itself.
func TestRegistrationHintFromAnInteractiveAnswer(t *testing.T) {
	platform := newStubPlatform(t)

	t.Run("yes with a typed address hints mining", func(t *testing.T) {
		cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")
		cfg, _, err := loadConfig(cfgPath, noEnv)
		if err != nil {
			t.Fatal(err)
		}
		store, err := auth.OpenStore(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		stdin := bytes.NewBufferString("y\ntwilight1k5stzqa2sgvfgx9u04cv93pek3gcmm9h5t9hkn\n")
		br := bufio.NewReader(stdin)
		outcome, code := askMiningQuestion(stdin, br, &bytes.Buffer{}, &bytes.Buffer{}, noEnv, cfg, store, true, false)
		if code != exitOK {
			t.Fatalf("askMiningQuestion: code=%d", code)
		}
		if got := registrationHint(outcome); len(got) != 1 || got[0] != "mining" {
			t.Fatalf("registrationHint(%+v) = %v, want [mining]", outcome, got)
		}
	})

	t.Run("no hints nothing", func(t *testing.T) {
		cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")
		cfg, _, err := loadConfig(cfgPath, noEnv)
		if err != nil {
			t.Fatal(err)
		}
		store, err := auth.OpenStore(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		stdin := bytes.NewBufferString("n\n")
		br := bufio.NewReader(stdin)
		outcome, code := askMiningQuestion(stdin, br, &bytes.Buffer{}, &bytes.Buffer{}, noEnv, cfg, store, true, false)
		if code != exitOK {
			t.Fatalf("askMiningQuestion: code=%d", code)
		}
		if got := registrationHint(outcome); got != nil {
			t.Fatalf("registrationHint(%+v) = %v, want nil", outcome, got)
		}
	})
}

// A "yes" already on the store with no address yet — a crash between
// the two writes on a first run, or an adopted state dir — must NOT be
// read back whole: that would register with the mining hint and never
// ask for an address at all. decideRegistrationOutcome must ask, but
// only the missing half (finishMiningEnabled, not askMiningQuestion):
// the terminal here would answer "n" to an enable question it should
// never see, so a fresh "no" would prove the bug, not fix it.
func TestDecideRegistrationOutcomeFillsInAMissingAddressWithoutReaskingEnable(t *testing.T) {
	platform := newStubPlatform(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, "")
	cfg, _, err := loadConfig(cfgPath, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMiningEnabled(true); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := store.LoadPayoutAddress(); ok {
		t.Fatal("test fixture bug: an address is already on file")
	}

	const addr = "twilight1k5stzqa2sgvfgx9u04cv93pek3gcmm9h5t9hkn"
	stdin := bytes.NewBufferString(addr + "\n") // ONE line: the address question, and nothing else
	br := bufio.NewReader(stdin)
	outcome, code := decideRegistrationOutcome(stdin, br, &bytes.Buffer{}, &bytes.Buffer{}, noEnv, cfg, store, true)
	if code != exitOK || !outcome.enabled || outcome.payoutAddress != addr {
		t.Fatalf("decideRegistrationOutcome: got %+v code=%d, want enabled with %q", outcome, code, addr)
	}
	if got, ok, err := store.LoadPayoutAddress(); err != nil || !ok || got != addr {
		t.Fatalf("address not persisted: %q ok=%v err=%v", got, ok, err)
	}
	if got := registrationHint(outcome); len(got) != 1 || got[0] != "mining" {
		t.Fatalf("registrationHint(%+v) = %v, want [mining]", outcome, got)
	}
}

// The scripted (non-interactive) shape of the same reorder, driven
// through cmdConnect itself: [mining] enabled = true means the answer is
// "yes" before register ever runs, and register must actually receive
// the hint — not just have askMiningQuestion return the right outcome.
func TestConnectScriptedOptInHintsMiningAtRegistration(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	as := newStubAS(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, as.srv.URL) // enabled = true, no address configured

	if code, out, errOut := runConnect(t, cfgPath, nil); code != exitOK {
		t.Fatalf("connect exited %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}

	scopes, seen := platform.requestedScopes()
	if !seen || len(scopes) != 1 || scopes[0] != "mining" {
		t.Fatalf("register's requested_scopes = %v (seen=%v), want [mining]", scopes, seen)
	}
	if registerCalls, _, _ := platform.counts(); registerCalls != 1 {
		t.Fatalf("register calls = %d, want 1", registerCalls)
	}
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	enabled, ok, err := store.LoadMiningEnabled()
	if err != nil || !ok || !enabled {
		t.Fatalf("decision on file = enabled=%v ok=%v err=%v, want true", enabled, ok, err)
	}
}

// The opt-out mirror: no [mining] block at all (the default scripted
// shape when TOKENDROP_MINING is unset) means the answer is "no" before
// register ever runs, and register must receive no hint at all — not an
// explicit empty list, which §5.1 treats as a field the client had an
// opinion about (see the "empty requested_scopes was sent as a field
// instead of omitted" check in pkg/platform's own client test).
func TestConnectScriptedOptOutSendsNoHintAtRegistration(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, "") // no [mining].enabled at all

	if code, out, errOut := runConnect(t, cfgPath, nil); code != exitOK {
		t.Fatalf("connect exited %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}

	scopes, seen := platform.requestedScopes()
	if !seen {
		t.Fatal("register was never called")
	}
	if len(scopes) != 0 {
		t.Fatalf("register's requested_scopes = %v, want none", scopes)
	}
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	enabled, ok, err := store.LoadMiningEnabled()
	if err != nil || !ok || enabled {
		t.Fatalf("decision on file = enabled=%v ok=%v err=%v, want false", enabled, ok, err)
	}
}

// Design safety claim: "a crash or a register failure between the
// question and the registration is safe... the next run asks nothing it
// has answered." A register failure leaves the decision (and, if
// enabled, the address) on disk with no agent registration saved
// (existed still false), so the retry takes the same `!existed` branch
// again. Proving "asks nothing it has answered" needs the retry to
// actually diverge from a fresh, config-driven answer — so this changes
// the config's own [mining].enabled between the two runs: if the retry
// re-derived from config (the old, pre-fix behavior), the second
// register call would hint nothing; because it reads the already-decided
// outcome back from the store instead, it still hints mining.
func TestConnectRegisterFailureRetriesWithTheSameHintNotTheConfig(t *testing.T) {
	withShortConnectTimings(t)
	platform := newStubPlatform(t)
	as := newStubAS(t)
	cfgPath, stateDir := connectConfig(t, platform.srv.URL, as.srv.URL) // enabled = true

	platform.failNextRegister(1)
	code, out, errOut := runConnect(t, cfgPath, nil)
	if code != exitTransport {
		t.Fatalf("first connect (register failing) exited %d, want exitTransport\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}

	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	enabled, ok, err := store.LoadMiningEnabled()
	if err != nil || !ok || !enabled {
		t.Fatalf("decision not persisted despite the failed register: enabled=%v ok=%v err=%v", enabled, ok, err)
	}
	if _, existed, err := store.LoadAgentRegistration(); err != nil || existed {
		t.Fatalf("agent registration recorded despite the failed register: existed=%v err=%v", existed, err)
	}
	if calls, _, _ := platform.counts(); calls != 1 {
		t.Fatalf("register calls after the failed attempt = %d, want 1", calls)
	}

	// The config now says the opposite of what was already decided — a
	// config-driven re-ask (the old order's only option) would flip the
	// hint; reading the stored decision back must not.
	flipped, err := os.ReadFile(cfgPath) // #nosec G304 -- this test's own t.TempDir()-rooted config file
	if err != nil {
		t.Fatal(err)
	}
	flippedBody := strings.Replace(string(flipped), "enabled = true", "enabled = false", 1)
	if flippedBody == string(flipped) {
		t.Fatal("test fixture bug: connectConfig's shape no longer contains \"enabled = true\"")
	}
	if err := os.WriteFile(cfgPath, []byte(flippedBody), 0o600); err != nil { // #nosec G703 -- fixed test path
		t.Fatal(err)
	}

	platform.claim("credits", "mining")
	code, out, errOut = runConnect(t, cfgPath, nil)
	if code != exitOK {
		t.Fatalf("retry exited %d, want exitOK\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}

	scopes, seen := platform.requestedScopes()
	if !seen || len(scopes) != 1 || scopes[0] != "mining" {
		t.Fatalf("retry's requested_scopes = %v (seen=%v), want [mining] — the flipped config was consulted instead of the stored decision", scopes, seen)
	}
	if calls, _, _ := platform.counts(); calls != 2 {
		t.Fatalf("register calls across both runs = %d, want 2 (one failed, one that succeeded)", calls)
	}
	if _, existed, err := store.LoadAgentRegistration(); err != nil || !existed {
		t.Fatalf("agent registration not recorded after the retry: existed=%v err=%v", existed, err)
	}
	if !strings.Contains(out, "claim this agent:") && !strings.Contains(out, "enrolled for mining") {
		t.Fatalf("retry did not proceed past registration: %q", out)
	}
}
