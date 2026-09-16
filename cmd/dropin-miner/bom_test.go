package main

// One leading byte-order mark is tolerated, and nothing else about the
// machine protocol moves.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

const bomPrefix = "\xef\xbb\xbf"

func TestSearchStdinAcceptsOneLeadingByteOrderMark(t *testing.T) {
	request := `{"version":1,"query":"café 東京"}`
	got, err := decodeMachineSearchRequest(strings.NewReader(bomPrefix + request))
	if err != nil {
		t.Fatalf("a request behind one mark was refused: %v", err)
	}
	if got.query != "café 東京" {
		t.Fatalf("query %q", got.query)
	}
	// The same request without the mark is unchanged, and the mark does not
	// permit anything else in front of or after the object.
	if _, err := decodeMachineSearchRequest(strings.NewReader(request)); err != nil {
		t.Fatalf("the plain request was refused: %v", err)
	}
	for name, raw := range map[string]string{
		"two marks":            bomPrefix + bomPrefix + request,
		"a mark after the end": request + bomPrefix,
		"a mark inside":        `{"version":1,` + bomPrefix + `"query":"q"}`,
		"leading text":         "x" + request,
		"two objects":          bomPrefix + request + request,
	} {
		if _, err := decodeMachineSearchRequest(strings.NewReader(raw)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// A hook's payload arrives from whatever shell the host put in front of it,
// and a mark there would otherwise cost the whole envelope: the hook would
// parse nothing, emit nothing, and the search would lose its lineage.
func TestHookStdinAcceptsOneLeadingByteOrderMark(t *testing.T) {
	_, ops := newFakeHookOps(nil)
	payload, _ := json.Marshal(map[string]any{
		"session_id": "sess-1", "prompt_id": "p-1", "tool_use_id": "call-1", "tool_name": "Bash",
		"tool_input": map[string]any{"command": "dropin-miner search -format model \"q\""},
	})
	var out, errOut bytes.Buffer
	if code := hookMain(ops, []string{"lineage"}, strings.NewReader(bomPrefix+string(payload)), &out, &errOut); code != exitOK {
		t.Fatalf("hook exited %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), bridgeEnv+"=") {
		t.Fatalf("a payload behind one mark produced no bridge: %q", out.String())
	}
}

func TestTrimUTF8BOMTakesExactlyOne(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"one mark":     {bomPrefix + "x", "x"},
		"two marks":    {bomPrefix + bomPrefix + "x", bomPrefix + "x"},
		"no mark":      {"x", "x"},
		"mark inside":  {"x" + bomPrefix, "x" + bomPrefix},
		"only a mark":  {bomPrefix, ""},
		"empty input":  {"", ""},
		"partial mark": {"\xef\xbb", "\xef\xbb"},
	} {
		if got := string(trimUTF8BOM([]byte(tc.in))); got != tc.want {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
}
