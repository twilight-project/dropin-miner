package trajectory

import "testing"

func TestSearchIsRecognizedByParsingTheCommandLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want []SearchInvocation
	}{
		{"bare stdin", `dropin-miner search --stdin`, []SearchInvocation{{Stdin: true}}},
		{"quoted path, config after the subcommand, here-document",
			"TOKENDROP_TRACE_BRIDGE=abc '/opt/x y/bin/dropin-miner' search -config '/opt/x y/t.toml' --stdin <<'JSON'\n{\"query\":\"q\"}\nJSON",
			[]SearchInvocation{{Stdin: true}}},
		{"config before the subcommand", `dropin-miner -config /t.toml search -format json q`, []SearchInvocation{{Format: "json"}}},
		{"human format", `cd /w && dropin-miner search -format text "two words"`, []SearchInvocation{{Format: "text"}}},
		{"format with equals", `dropin-miner search --format=JSON q`, []SearchInvocation{{Format: "json"}}},
		{"windows path under bash", `"C:\Users\p\bin\dropin-miner.exe" search --stdin`, []SearchInvocation{{Stdin: true}}},
		{"powershell call operator and here-string",
			"$OutputEncoding = [System.Text.UTF8Encoding]::new($false)\n@'\n{\"query\":\"dropin-miner search --stdin\"}\n'@ | & 'C:\\Users\\p\\bin\\dropin-miner.exe' search -config 'C:\\t.toml' --stdin",
			[]SearchInvocation{{Stdin: true}}},
		{"piped onward", `dropin-miner search --stdin <<'JSON' | jq .result` + "\n{}\nJSON", []SearchInvocation{{Stdin: true, OutputElsewhere: true}}},
		{"or is not a pipe", `dropin-miner search -format json q || echo failed`, []SearchInvocation{{Format: "json"}}},
		{"redirected to a file", `dropin-miner search -format json q > /tmp/out.json`, []SearchInvocation{{Format: "json", OutputElsewhere: true}}},
		{"stderr redirect is not stdout", `dropin-miner search -format json q 2>/dev/null`, []SearchInvocation{{Format: "json"}}},
		{"two in one line", `dropin-miner search --stdin < a.json; dropin-miner search --stdin < b.json`, []SearchInvocation{{Stdin: true}, {Stdin: true}}},
		{"behind env", `env TOKENDROP_TRACE=off dropin-miner search --stdin`, []SearchInvocation{{Stdin: true}}},

		// Everything below mentions the search and runs none.
		{"echoed", `echo "dropin-miner search --stdin is the command"`, nil},
		{"echoed unquoted", `echo dropin-miner search --stdin`, nil},
		{"grepped for", `grep -rn 'dropin-miner search' docs`, nil},
		{"another subcommand", `dropin-miner status -json`, nil},
		{"another binary", `/opt/not-dropin-miner search --stdin`, nil},
		{"a longer name", `dropin-miner-worktrees search --stdin`, nil},
		{"the command as here-document data", "cat > notes.md <<'EOF'\ndropin-miner search --stdin\nEOF", nil},
		{"the command inside a query", "curl -d '{\"q\":\"dropin-miner search --stdin\"}' https://example.test", nil},
		{"help text", `dropin-miner help search`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseSearchInvocations(c.line)
			if len(got) != len(c.want) {
				t.Fatalf("parseSearchInvocations = %+v, want %+v", got, c.want)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Errorf("invocation %d = %+v, want %+v", i, got[i], c.want[i])
				}
			}
		})
	}
}

func TestLossReasonIsDecidedFromStructure(t *testing.T) {
	envelope := func(extra string) string { return `{"version":1,"command":"search",` + extra + `}` }
	cases := []struct {
		name    string
		inv     SearchInvocation
		text    string
		isError bool
		denied  bool
		wantIDs int
		want    LossReason
	}{
		{"machine envelope", SearchInvocation{Stdin: true}, envelope(`"ok":true,"request_id":"req_1"`), false, false, 1, ""},
		{"id only under result", SearchInvocation{Stdin: true}, envelope(`"ok":true,"result":{"request_id":"req_2"}`), false, false, 1, ""},
		{"router json", SearchInvocation{Format: "json"}, `{"request_id":"req_3","candidates":[]}`, false, false, 1, ""},
		{"byte-order mark in front", SearchInvocation{Stdin: true}, "\uFEFF" + envelope(`"ok":true,"request_id":"req_4"`), false, false, 1, ""},
		{"a failure that still reached the router", SearchInvocation{Stdin: true}, envelope(`"ok":false,"request_id":"req_5"`), true, false, 1, ""},
		{"denied by the host", SearchInvocation{Stdin: true}, "Permission to use Bash has been denied.", true, true, 0, LossDeniedByHost},
		{"failed, envelope without an id", SearchInvocation{Stdin: true}, envelope(`"ok":false,"code":"usage"`), false, false, 0, LossSearchFailed},
		{"failed, tool error", SearchInvocation{Stdin: true}, "Exit code 1", true, false, 0, LossSearchFailed},
		{"piped elsewhere", SearchInvocation{Stdin: true, OutputElsewhere: true}, "3", false, false, 0, LossOutputElsewhere},
		{"human rendering", SearchInvocation{Format: "text"}, "an answer", false, false, 0, LossHumanFormat},
		{"cut by the host", SearchInvocation{Stdin: true}, `{"version":1,"command":"search","ok":true,"result":{"candi`, false, false, 0, LossResultTruncated},
		{"json without an id", SearchInvocation{Stdin: true}, envelope(`"ok":true`), false, false, 0, LossNoRequestID},
		{"not json at all", SearchInvocation{Stdin: true}, "something else", false, false, 0, LossResultNotJSON},
		{"an id that is not shaped like one", SearchInvocation{Stdin: true}, envelope(`"ok":true,"request_id":"not an id; rm -rf"`), false, false, 0, LossNoRequestID},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Search{Invocations: []SearchInvocation{c.inv}, Loss: LossNoResult}
			s.settle(c.text, c.isError, c.denied, 7)
			if len(s.RequestIDs) != c.wantIDs || s.Loss != c.want {
				t.Fatalf("RequestIDs = %v, Loss = %q; want %d id(s), %q", s.RequestIDs, s.Loss, c.wantIDs, c.want)
			}
			if s.Anchored() != (s.Loss == "") {
				t.Fatalf("Anchored = %v with Loss %q: exactly one must hold", s.Anchored(), s.Loss)
			}
		})
	}
}
