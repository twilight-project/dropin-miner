package main

// #88 item 3: every byte this client writes into a configuration file is
// ASCII.
//
// Windows PowerShell 5.1 reads a file with no byte-order mark in the
// system's ANSI code page, so a UTF-8 em dash arrives as three cp1252
// characters and `Get-Content ~\.tokendrop\tokendrop.toml` showed the
// participant `# target_epoch deliberately unset â€” flush asks…`. Nothing
// behaved differently; it just looked broken to the one person who had
// gone looking at the file.
//
// The guard is the rule, not the character. Fixing the one dash leaves the
// next piece of non-ASCII punctuation to read exactly as badly, so these
// cases render every configuration artifact the client produces and refuse
// any byte above 0x7F, naming the offset and what it decodes to.
//
// installer_bridge_test.go already holds the same rule for scripts/install.ps1,
// where the consequence is worse than cosmetic: a mis-decoded quotation mark
// ends a string early and the file stops parsing. This file is the config
// side of that one rule.
//
// Every fixture below is built from ASCII inputs on purpose. A participant
// whose home directory is C:\Users\José would make the rendered output
// non-ASCII through no fault of this client, and the claim under test is
// about the bytes the client contributes, not the ones it is handed.

import (
	"path/filepath"
	"strings"
	"testing"
)

// firstNonASCII is the offset of the first byte above 0x7F, or -1. Split
// out from the reporting below so the scan itself can be tested against a
// byte that is not ASCII, rather than only against bytes that are: a check
// that never looks passes every case in this file.
func firstNonASCII(b []byte) int {
	for i, c := range b {
		if c >= 0x80 {
			return i
		}
	}
	return -1
}

// asciiOrFail names the first byte above 0x7F: its offset, what it is, and
// the line it sits on. "the output is not ASCII" is not a finding anyone
// can act on.
func asciiOrFail(t *testing.T, what string, b []byte) {
	t.Helper()
	i := firstNonASCII(b)
	if i < 0 {
		return
	}
	line := 1 + strings.Count(string(b[:i]), "\n")
	start := strings.LastIndexByte(string(b[:i]), '\n') + 1
	end := len(b)
	if nl := strings.IndexByte(string(b[i:]), '\n'); nl >= 0 {
		end = i + nl
	}
	r := []rune(string(b[i:]))
	t.Fatalf("%s: byte %d (line %d) is 0x%02x, part of %q, in:\n  %s\n"+
		"every byte this client writes into a config must be ASCII: Windows PowerShell 5.1 "+
		"decodes the file in the ANSI code page and shows a participant mojibake",
		what, i, line, b[i], string(r[0]), string(b[start:end]))
}

// asciiSetupValues is a complete set of ASCII inputs, so anything
// non-ASCII in the output came from the client.
func asciiSetupValues(home string) setupValues {
	return setupValues{
		home:         home,
		router:       "https://router-api.nyks.dev",
		platformURL:  "https://platform.nyks.dev",
		agentsAPIURL: "https://agents-v1.nyks.dev",
		asURL:        "https://rewards.nyks.dev",
		chainID:      "twilight-testnet-1",
		slotID:       3,
	}
}

func TestEveryGeneratedConfigIsASCII(t *testing.T) {
	home := filepath.Join(t.TempDir(), "tokendrop")
	v := asciiSetupValues(home)

	t.Run("tokendrop.toml, fresh", func(t *testing.T) {
		b, err := renderFreshConfig(v)
		if err != nil {
			t.Fatal(err)
		}
		// The line #88 was filed about, still present and still saying what
		// it said: the case would pass just as well on a file that had lost
		// the comment altogether.
		if !strings.Contains(string(b), "# target_epoch deliberately unset") {
			t.Fatalf("the target_epoch comment is gone; this case would then prove nothing:\n%s", b)
		}
		asciiOrFail(t, "renderFreshConfig", b)
	})

	t.Run("tokendrop.toml, fresh with the scripted mining answer", func(t *testing.T) {
		sv := v
		sv.scriptedMine = true
		sv.payoutAddress = "twilight1k5stzqa2sgvfgx9u04cv93pek3gcmm9h5t9hkn"
		b, err := renderFreshConfig(sv)
		if err != nil {
			t.Fatal(err)
		}
		asciiOrFail(t, "renderFreshConfig (scripted)", b)
	})

	t.Run("tokendrop.toml, migrated", func(t *testing.T) {
		// A proxy-era config with neither table: setup appends both, with a
		// comment of its own.
		existing := []byte("[[provider]]\nname = \"search-router\"\nupstream = \"https://router-api.nyks.dev\"\n")
		plan, err := planSetupConfig(filepath.Join(home, setupConfigFile), existing, v, func([]byte) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if plan.outcome != configMigrated {
			t.Fatalf("outcome %v, want configMigrated", plan.outcome)
		}
		asciiOrFail(t, "planSetupConfig (migrated)", plan.data)
	})

	t.Run("~/.codex/config.toml block", func(t *testing.T) {
		asciiOrFail(t, "codexSandboxBlock", codexSandboxBlock([]string{
			filepath.Join(home, "state"), filepath.Join(home, "intake"),
		}))
	})

	t.Run("Hermes config.yaml block", func(t *testing.T) {
		body := "hooks:\n  pre_tool_call:\n    - command: " + hermesYAMLSingleQuoted(filepath.Join(home, "bin", "dropin-miner")) + "\n"
		asciiOrFail(t, "hermesHookBlock", hermesHookBlock(body, true))
		asciiOrFail(t, "hermesHookBlock (no trailing-newline note)", hermesHookBlock(body, false))
	})
}

// The scan, against a byte that is not ASCII and against one that is.
// Every case above passes on generated output that is already clean, so
// without this a scan that never looked would look exactly the same.
func TestTheASCIIScanFindsTheByteItIsLookingFor(t *testing.T) {
	if i := firstNonASCII([]byte("# target_epoch deliberately unset - flush asks the AS.\n")); i != -1 {
		t.Fatalf("firstNonASCII = %d on an ASCII line, want -1", i)
	}
	// The exact line #88 was filed about, before the fix.
	before := []byte("# target_epoch deliberately unset \u2014 flush asks the AS.\n")
	i := firstNonASCII(before)
	if i < 0 {
		t.Fatal("firstNonASCII missed the em dash that #88 was filed about")
	}
	if want := strings.Index(string(before), "\u2014"); i != want {
		t.Fatalf("firstNonASCII = %d, want %d (the em dash's first byte)", i, want)
	}
}
