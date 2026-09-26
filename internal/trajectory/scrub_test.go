package trajectory

import (
	"encoding/json"
	"strings"
	"testing"
)

// The scrubber's own cases. testScrubber (emit_test.go) is the machine they
// all run against: home /Users/zebrauser, hostname zebra-host.local, and one
// environment secret.

func TestTheAccountNameIsRewrittenInsideAPathAndOmittedOutsideOne(t *testing.T) {
	s := testScrubber()

	// Inside a path the name is known exactly, so it is replaced and the
	// text around it survives.
	got, res := s.Text("ZEBRA-TEXT wrote /Users/zebrauser/notes/today.md")
	if want := "ZEBRA-TEXT wrote ~/notes/today.md"; got != want || res.Omit != "" {
		t.Errorf("Text = %q, omit %q; want %q rewritten", got, res.Omit, want)
	}
	if res.Rewrites[ScrubHomePath] != 1 {
		t.Errorf("rewrites = %v, want one home_path", res.Rewrites)
	}

	// Outside one it is a bare mention with no shape around it to bound what
	// else the sentence says, and the event goes whole.
	for _, in := range []string{
		"ZEBRA-TEXT the account is zebrauser on this machine",
		"ZEBRA-TEXT ssh zebrauser@192.0.2.10",
		"ZEBRA-TEXT uid=501(zebrauser) gid=20(staff)",
	} {
		got, res := s.Text(in)
		if res.Omit != ScrubAccountName || got != "" {
			t.Errorf("Text(%q) = %q, omit %q; want the event omitted as account_name", in, got, res.Omit)
		}
	}

	// USER and LOGNAME name the account too, where the home directory does
	// not say it — a transcript read on a machine other than its own.
	for _, environ := range [][]string{{"USER=zebraworker"}, {"LOGNAME=zebraworker"}} {
		other := NewScrubber(environ, "", "")
		if _, res := other.Text("ZEBRA-TEXT signed in as zebraworker"); res.Omit != ScrubAccountName {
			t.Errorf("with %v the account name was not omitted: %q", environ, res.Omit)
		}
	}
}

// TestWhatIsDetectedIsReadInTheTextAsItArrived is T3b's fix, and its first
// case is the defect: at c19f854 the home directory was rewritten to a tilde
// before detection ran, so an environment secret whose value began with the
// home directory no longer matched its own value, and the event was kept.
// A rewrite can only hide such a value from its rule, never reveal one, so
// every detect-only rule now reads the original text. The account name is the
// exception and keeps reading the rewritten text, because inside a path it is
// meant to be replaced and the event kept.
func TestWhatIsDetectedIsReadInTheTextAsItArrived(t *testing.T) {
	// A secret whose value IS a path under the home directory. Synthetic, and
	// a path rather than a credential: what is being tested is that the
	// scrubber still recognizes its own environment's value after the home
	// directory in it has been rewritten.
	const plantedValue = "/Users/zebrauser/.stores/zebra-deploy-0001" // #nosec G101 -- a synthetic fixture path, not a credential
	s := NewScrubber(
		[]string{"ZEBRA_DEPLOY_TOKEN=" + plantedValue},
		"zebra-host.local", "/Users/zebrauser")

	got, res := s.Text("ZEBRA-TEXT before " + plantedValue + " after")
	if res.Omit != ScrubEnvSecret || got != "" {
		t.Errorf("Text = %q, omit %q; want the event omitted as env_secret — the value is the secret whether or not its home directory has been rewritten", got, res.Omit)
	}

	// The same scrubber, on a home path that is nobody's secret: still
	// rewritten, and the event still kept.
	got, res = s.Text("ZEBRA-TEXT wrote /Users/zebrauser/notes/today.md")
	if want := "ZEBRA-TEXT wrote ~/notes/today.md"; got != want || res.Omit != "" {
		t.Errorf("Text = %q, omit %q; want %q rewritten and kept", got, res.Omit, want)
	}
	if res.Rewrites[ScrubHomePath] != 1 {
		t.Errorf("rewrites = %v, want one home_path", res.Rewrites)
	}

	// And a bare mention of the account name: still omitted, which is the
	// rule that has to keep reading the rewritten text.
	if got, res := s.Text("ZEBRA-TEXT the account is zebrauser here"); res.Omit != ScrubAccountName || got != "" {
		t.Errorf("Text = %q, omit %q; want the event omitted as account_name", got, res.Omit)
	}
}

// TestASecretNameIsMatchedBySegmentNotBySubstring. The substring rule read
// every TOKENDROP_* variable this client asks a participant to set as a
// secret, because TOKEN is inside TOKENDROP — so the participant's own config
// and wallet paths counted as secret values. The product's name is not a
// defect in the participant's environment.
func TestASecretNameIsMatchedBySegmentNotBySubstring(t *testing.T) {
	const value = "zebra-secret-value-12345"
	for name, want := range map[string]bool{
		// A segment that is one of the words.
		"CLAUDE_CODE_MESSAGING_TOKEN": true,
		"ZEBRA_DEPLOY_TOKEN":          true,
		"API_KEY":                     true,
		"AWS_SECRET_ACCESS_KEY":       true,
		"KEY":                         true,
		// AUTH is a whole segment here, so this still matches. It is still a
		// false positive — a socket path is not a secret — and it is not one
		// a rule about segments can fix.
		"SSH_AUTH_SOCK": true,
		// The word only as part of a longer segment.
		"TOKENDROP_CONFIG":     false,
		"TOKENDROP_WALLET_DIR": false,
		"TOKENDROP_HOME":       false,
		"MONKEYS":              false,
		"KEYBOARD_LAYOUT":      false,
	} {
		t.Run(name, func(t *testing.T) {
			s := NewScrubber([]string{name + "=" + value}, "", "")
			_, res := s.Text("ZEBRA-TEXT before " + value + " after")
			if got := res.Omit == ScrubEnvSecret; got != want {
				t.Errorf("%s: treated as a secret name = %v, want %v (omit %q)", name, got, want, res.Omit)
			}
		})
	}
}

func TestTheHostnamesFirstLabelIsThisMachineToo(t *testing.T) {
	s := testScrubber()
	for _, in := range []string{
		"ZEBRA-COMMAND-OUTPUT this is zebra-host.local answering",
		"ZEBRA-COMMAND-OUTPUT this is zebra-host answering",
		"ZEBRA-TEXT scp file zebra-host:/tmp/x",
	} {
		got, res := s.Text(in)
		if res.Omit != ScrubHostname || got != "" {
			t.Errorf("Text(%q) = %q, omit %q; want the event omitted as hostname", in, got, res.Omit)
		}
	}
	// A longer name that merely starts with the label is another machine.
	if _, res := s.Text("ZEBRA-TEXT deployed to zebra-hostinger.example"); res.Omit != "" {
		t.Errorf("omit = %q: zebra-hostinger is not this machine", res.Omit)
	}
}

func TestTheHomePathIsFoundInEverySpellingAShellWrites(t *testing.T) {
	s := testScrubber()
	for in, want := range map[string]string{
		"/Users/zebrauser/notes": "~/notes",
		// Both filesystems this runs on are case-insensitive, so a tool
		// result prints whichever spelling it was handed.
		"/users/zebrauser/notes": "~/notes",
		"/USERS/ZEBRAUSER/notes": "~/notes",
		// The shell's own spelling of the same directory.
		"~zebrauser/notes": "~/notes",
	} {
		got, res := s.Text("ZEBRA-TEXT " + in + " end")
		if want := "ZEBRA-TEXT " + want + " end"; got != want || res.Omit != "" {
			t.Errorf("Text(%q) = %q, omit %q; want %q", in, got, res.Omit, want)
		}
	}

	// A different account whose name merely begins with this one's is not
	// this home directory, and keeps its own spelling through the generic
	// rewrite that follows.
	got, res := s.Text("ZEBRA-TEXT /Users/ZebraUserTwo/x")
	if want := "ZEBRA-TEXT /Users/<user>/x"; got != want || res.Omit != "" {
		t.Errorf("Text = %q, omit %q; want %q", got, res.Omit, want)
	}
}

func TestAStringNamingTheTraceBridgeIsOmittedWhereverItAppears(t *testing.T) {
	s := testScrubber()
	for _, in := range []string{
		"TOKENDROP_TRACE_BRIDGE=WkVCUkEtQlJJREdF dropin-miner search --stdin",
		"$env:TOKENDROP_TRACE_BRIDGE='WkVCUkEtQlJJREdF'; & dropin-miner search --stdin",
		"ZEBRA-TEXT the hook had exported TOKENDROP_TRACE_BRIDGE= before it ran",
	} {
		got, res := s.Text(in)
		if res.Omit != ScrubTraceBridge || got != "" {
			t.Errorf("Text(%q) = %q, omit %q; want the event omitted as trace_bridge", in, got, res.Omit)
		}
	}
	// The name alone, with no value after it, is somebody writing about the
	// variable rather than carrying one.
	if _, res := s.Text("ZEBRA-TEXT the variable is called TOKENDROP_TRACE_BRIDGE"); res.Omit != "" {
		t.Errorf("omit = %q: naming the variable is not carrying an envelope", res.Omit)
	}
}

func TestFiveEnvironmentLinesOmitTheEventAndFourDoNot(t *testing.T) {
	s := testScrubber()
	lines := []string{"ZEBRA_ONE=1", "ZEBRA_TWO=2", "ZEBRA_THREE=3", "ZEBRA_FOUR=4", "ZEBRA_FIVE=5"}

	four := strings.Join(lines[:4], "\n")
	if got, res := s.Text(four); res.Omit != "" || got != four {
		t.Errorf("four lines: omit %q, text %q; want them kept", res.Omit, got)
	}
	five := strings.Join(lines, "\n")
	if got, res := s.Text(five); res.Omit != ScrubEnvDump || got != "" {
		t.Errorf("five lines: omit %q, text %q; want the event omitted as env_dump", res.Omit, got)
	}
	exported := "export " + strings.Join(lines, "\nexport ")
	if got, res := s.Text(exported); res.Omit != ScrubEnvDump || got != "" {
		t.Errorf("exported lines: omit %q, text %q; want the event omitted as env_dump", res.Omit, got)
	}
	// Prose that happens to hold an equals sign is not an environment.
	prose := "ZEBRA-TEXT a = 1\nZEBRA-TEXT b = 2\nZEBRA-TEXT c = 3\nZEBRA-TEXT d = 4\nZEBRA-TEXT e = 5"
	if _, res := s.Text(prose); res.Omit != "" {
		t.Errorf("omit = %q: spaced assignments in prose are not an environment dump", res.Omit)
	}
}

// TestASecretUsedAsAJSONKeyOmitsTheWholeValue: a tool call's parameters are
// one item, and a key is as much of it as a value. An object keyed by what it
// holds — a map from token to account, an environment as an object — puts the
// secret exactly where a value-only scan would not look.
func TestASecretUsedAsAJSONKeyOmitsTheWholeValue(t *testing.T) {
	for _, c := range []struct {
		name string
		raw  string
		want ScrubClass
	}{
		{"credential as a top-level key", `{"sr-zebra0000zebra0000zebra00":"ZEBRA-HARMLESS"}`, ScrubCredential},
		{"email as a nested key", `{"env":{"owners":{"zebra.person@example.test":"ZEBRA-NAME"}}}`, ScrubEmail},
		{"env secret as a key inside an array", `[{"ZEBRA-K":"v"},{"zebra-secret-value-12345":"v"}]`, ScrubEnvSecret},
		{"hostname as a key", `{"hosts":{"zebra-host.local":["ZEBRA-ROLE"]}}`, ScrubHostname},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, res := testScrubber().JSON(json.RawMessage(c.raw))
			if res.Omit != c.want || out != nil {
				t.Fatalf("JSON(%s) = %s, omit %q; want the whole value omitted as %q", c.raw, out, res.Omit, c.want)
			}
		})
	}
}

// TestAStringPastTheLimitIsOmittedWholeBeforeAnythingScansIt: scanning the
// first piece of a long result and keeping that is how a credential at its
// end survives its own scrubbing. The limit is checked before any pattern
// runs, and what comes back is nothing at all rather than a prefix.
func TestAStringPastTheLimitIsOmittedWholeBeforeAnythingScansIt(t *testing.T) {
	s := testScrubber()
	const tail = "sr-zebra0000zebra0000zebra00"
	big := strings.Repeat("ZEBRA-FILLER ", (scrubLimit/13)+1) + tail
	if len(big) <= scrubLimit {
		t.Fatalf("the test's own string is %d bytes, not past the %d-byte limit it is about", len(big), scrubLimit)
	}
	if !strings.HasSuffix(big, tail) {
		t.Fatal("the test's own string does not end in the credential it is about")
	}

	got, res := s.Text(big)
	if res.Omit != ScrubTooLarge {
		t.Errorf("omit = %q, want too_large: the limit is checked before any pattern runs", res.Omit)
	}
	if got != "" {
		t.Errorf("Text returned %d bytes; an item too large to scrub whole is omitted whole, never cut", len(got))
	}
	if strings.Contains(got, tail) {
		t.Error("the credential at the end of the string was returned")
	}

	raw, err := json.Marshal(big)
	if err != nil {
		t.Fatal(err)
	}
	out, res := s.JSON(raw)
	if res.Omit != ScrubTooLarge || out != nil {
		t.Errorf("JSON = %d bytes, omit %q; want it omitted whole as too_large", len(out), res.Omit)
	}
}
