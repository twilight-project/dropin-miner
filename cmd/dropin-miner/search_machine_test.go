package main

// `search --stdin`: the machine protocol.
//
// Two things are being proved here. The query survives the round trip
// exactly — no shell ever sees it, so no amount of metacharacters in it
// can mean anything — and the answer is one JSON object whose header an
// agent can branch on, with the process exit code it claims to have.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/twilight-project/dropin-miner/pkg/auth"
)

func runSearchStdin(t *testing.T, h *searchHarness, env map[string]string, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut strings.Builder
	getenv := envOf(env)
	h.ops.hook.getenv = getenv
	code := searchMain(h.ops, append([]string{"--stdin"}, args...), strings.NewReader(stdin), &out, &errOut, getenv)
	return code, out.String(), errOut.String()
}

// mandatoryEnvelopeFields is §9's list. Every envelope carries all of
// them, whatever happened.
var mandatoryEnvelopeFields = []string{
	"version", "command", "ok", "exit_code", "status", "code", "retryable", "action",
}

// decodeEnvelope asserts stdout is exactly one JSON object with one
// trailing newline and nothing else, then returns it.
func decodeEnvelope(t *testing.T, stdout string) map[string]any {
	t.Helper()
	if !strings.HasSuffix(stdout, "\n") {
		t.Fatalf("stdout does not end in a newline: %q", stdout)
	}
	if strings.Count(stdout, "\n") != 1 {
		t.Fatalf("stdout is not exactly one line of JSON: %q", stdout)
	}
	if stdout[0] != '{' {
		t.Fatalf("stdout begins with something other than the envelope: %q", stdout)
	}
	dec := json.NewDecoder(strings.NewReader(stdout))
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("stdout is not one JSON object: %v in %q", err, stdout)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		t.Fatalf("stdout carries more than one JSON value: %q", stdout)
	}
	for _, k := range mandatoryEnvelopeFields {
		if _, ok := m[k]; !ok {
			t.Errorf("the envelope is missing the mandatory field %q: %s", k, stdout)
		}
	}
	return m
}

func envField(t *testing.T, env map[string]any, key string) any {
	t.Helper()
	v, ok := env[key]
	if !ok {
		t.Fatalf("envelope has no %q: %v", key, env)
	}
	return v
}

// ── §18 the input contract ──────────────────────────────────────────────

// Every one of these is a query a shell would mangle, and none of them is
// ever handed to a shell. The assertion is byte equality between the JSON
// string the caller wrote and the JSON string the router received.
func TestMachineQueriesSurviveExactlyWhateverIsInThem(t *testing.T) {
	canary := filepath.Join(t.TempDir(), "command-was-executed")
	for _, tc := range []struct{ name, query string }{
		{"plain", "how do ports work"},
		{"unicode", "¿cómo funcionan los puertos? 端口 🌐 é́"},
		{"double quotes", `he said "quote" to me`},
		{"single quotes", `it's a 'quoted' thing`},
		{"dollar", "$HOME and $PATH and ${BRACED}"},
		{"backticks", "`whoami` and ``nested``"},
		{"semicolon", "first; second; third"},
		{"pipes", "a | b || c"},
		{"redirects", "echo x > out.txt 2>&1 < in.txt"},
		{"backslashes", `C:\Users\someone\path and \\server\share and \n\t`},
		{"parens", "(a) and $(id) and $((1+2))"},
		{"embedded newline", "first line\nsecond line\r\nthird"},
		{"command substitution canary", "$(touch " + canary + ") `touch " + canary + "` ; touch " + canary},
		{"ampersands", "a && b & c"},
		{"glob and tilde", "~/*.go ?? [a-z]"},
		{"leading and trailing space kept", "  spaced out  "},
		{"null-ish escapes", `\u0000 \x00 %00`},
		{"json in the query", `{"nested":"object"} [1,2,3]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(routerBody))
			})
			h := fixedSearchOps(root)
			request, err := json.Marshal(map[string]any{"version": 1, "query": tc.query})
			if err != nil {
				t.Fatal(err)
			}
			code, out, errOut := runSearchStdin(t, h, map[string]string{"TOKENDROP_API_KEY": "k"},
				string(request), "-config", cfg)
			if code != exitOK {
				t.Fatalf("exit %d: %s %s", code, out, errOut)
			}
			_, sent := fr.last(t)
			var body map[string]any
			if err := json.Unmarshal(sent, &body); err != nil {
				t.Fatalf("router body: %v", err)
			}
			// The forwarded query is the caller's string, not a trimmed,
			// re-quoted or re-escaped version of it.
			if got := body["query"]; got != tc.query {
				t.Errorf("query changed in transit:\n sent %q\n want %q", got, tc.query)
			}
			if _, err := os.Stat(canary); err == nil {
				t.Fatal("a query was evaluated as a command: the canary file exists")
			}
		})
	}
}

func TestMachineInputContractRefusals(t *testing.T) {
	big := `{"version":1,"query":"` + strings.Repeat("x", machineStdinMax) + `"}`
	for _, tc := range []struct {
		name, stdin, wantCode string
	}{
		{"unsupported version", `{"version":2,"query":"q"}`, codeUnsupportedVersion},
		{"version zero", `{"version":0,"query":"q"}`, codeUnsupportedVersion},
		{"missing version", `{"query":"q"}`, codeUnsupportedVersion},
		{"version as a string", `{"version":"1","query":"q"}`, codeUnsupportedVersion},
		{"missing query", `{"version":1}`, codeMissingQuery},
		{"empty query", `{"version":1,"query":""}`, codeEmptyQuery},
		{"whitespace-only query", `{"version":1,"query":"   \t\n  "}`, codeEmptyQuery},
		{"malformed JSON", `{"version":1,"query":`, codeInvalidJSON},
		{"trailing JSON value", `{"version":1,"query":"a"}{"version":1,"query":"b"}`, codeInvalidJSON},
		{"trailing non-whitespace", `{"version":1,"query":"a"} oops`, codeInvalidJSON},
		{"not an object", `["version",1]`, codeInvalidJSON},
		{"empty stdin", ``, codeInvalidJSON},
		{"oversized stdin", big, codeInputTooLarge},
		// Trace and lineage are local host state. A request that could set
		// them would let the caller choose the trajectory its search is
		// attributed to.
		{"trace field", `{"version":1,"query":"q","trace":{"v":1}}`, codeUnknownField},
		{"lineage field", `{"version":1,"query":"q","lineage":"/tmp/x"}`, codeUnknownField},
		{"unknown field", `{"version":1,"query":"q","surprise":true}`, codeUnknownField},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(routerBody))
			})
			h := fixedSearchOps(root)
			code, out, errOut := runSearchStdin(t, h, map[string]string{"TOKENDROP_API_KEY": "k"}, tc.stdin, "-config", cfg)
			if code != exitUsage {
				t.Fatalf("exit %d, want %d (%s)", code, exitUsage, out)
			}
			env := decodeEnvelope(t, out)
			if got := envField(t, env, "code"); got != tc.wantCode {
				t.Errorf("code %q, want %q", got, tc.wantCode)
			}
			if got := envField(t, env, "action"); got != actionFixInput {
				t.Errorf("action %q, want %q", got, actionFixInput)
			}
			if errOut != "" {
				t.Errorf("an expected protocol error also explained itself on stderr: %q", errOut)
			}
			fr.mu.Lock()
			defer fr.mu.Unlock()
			if len(fr.reqs) != 0 {
				t.Error("a refused request was sent to the router anyway")
			}
		})
	}
}

// Two sources for one value is how a caller ends up sending one and
// escaping the other.
func TestMachineModeRefusesAPositionalQuery(t *testing.T) {
	fr, cfg, root := newFakeRouter(t, nil)
	h := fixedSearchOps(root)
	code, out, _ := runSearchStdin(t, h, map[string]string{"TOKENDROP_API_KEY": "k"},
		`{"version":1,"query":"from stdin"}`, "-config", cfg, "from", "argv")
	if code != exitUsage {
		t.Fatalf("exit %d, want %d", code, exitUsage)
	}
	env := decodeEnvelope(t, out)
	if got := envField(t, env, "code"); got != codeUnexpectedArgument {
		t.Errorf("code %q, want %q", got, codeUnexpectedArgument)
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if len(fr.reqs) != 0 {
		t.Error("the router was called despite the refusal")
	}
}

// -format model is a human presentation choice. It must not turn the
// machine contract back into prose.
func TestMachineModeIgnoresFormatModel(t *testing.T) {
	_, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(routerBody))
	})
	h := fixedSearchOps(root)
	code, out, _ := runSearchStdin(t, h, map[string]string{"TOKENDROP_API_KEY": "k"},
		`{"version":1,"query":"q"}`, "-config", cfg, "-format", "model")
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	env := decodeEnvelope(t, out)
	if env["command"] != "search" {
		t.Errorf("not the machine envelope: %s", out)
	}
}

func TestMachineTierComesFromTheRequest(t *testing.T) {
	fr, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(routerBody))
	})
	h := fixedSearchOps(root)
	code, _, _ := runSearchStdin(t, h, map[string]string{"TOKENDROP_API_KEY": "k"},
		`{"version":1,"query":"q","tier":"fast"}`, "-config", cfg)
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	_, sent := fr.last(t)
	var body map[string]any
	if err := json.Unmarshal(sent, &body); err != nil {
		t.Fatal(err)
	}
	if body["tier"] != "fast" {
		t.Errorf("tier: %v", body)
	}
}

// ── §18 the machine envelope, for every process class ───────────────────

func TestEveryProcessClassEmitsACompleteEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name      string
		wantExit  int
		want      string
		handler   http.HandlerFunc
		transport http.RoundTripper
		stdin     string
	}{
		{
			name: "0 success", wantExit: exitOK, want: statusOK,
			handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(routerBody)) },
		},
		{
			name: "1 transport", wantExit: exitTransport, want: statusTransport,
			transport: &recordingTransport{fn: func(int, *http.Request) (*http.Response, error) {
				return nil, errors.New("dial fictional.test: connection refused")
			}},
		},
		{
			name: "2 usage", wantExit: exitUsage, want: statusUsage,
			stdin: `{"version":9,"query":"q"}`,
		},
		{
			name: "3 client", wantExit: exitClientErr, want: statusClient,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"code":"unauthorized","error":"key not accepted"}`))
			},
		},
		{
			name: "4 server", wantExit: exitServerErr, want: statusServer,
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"code":"internal","error":"boom"}`))
			},
		},
		{
			name: "4 invalid success", wantExit: exitServerErr, want: statusServer,
			handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"nope":`)) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.transport != nil {
				useTransport(t, tc.transport)
			}
			_, cfg, root := newFakeRouter(t, tc.handler)
			h := fixedSearchOps(root)
			stdin := tc.stdin
			if stdin == "" {
				stdin = `{"version":1,"query":"q"}`
			}
			code, out, errOut := runSearchStdin(t, h, map[string]string{"TOKENDROP_API_KEY": "k"}, stdin, "-config", cfg)
			env := decodeEnvelope(t, out)
			if code != tc.wantExit {
				t.Fatalf("exit %d, want %d: %s", code, tc.wantExit, out)
			}
			// The envelope must not claim an exit code the process did
			// not return.
			if got := envField(t, env, "exit_code"); got != float64(tc.wantExit) {
				t.Errorf("envelope exit_code %v, process returned %d", got, tc.wantExit)
			}
			if got := envField(t, env, "status"); got != tc.want {
				t.Errorf("status %q, want %q", got, tc.want)
			}
			if got := envField(t, env, "ok"); got != (tc.wantExit == exitOK) {
				t.Errorf("ok %v at exit %d", got, tc.wantExit)
			}
			if env["version"] != float64(machineVersion) || env["command"] != "search" {
				t.Errorf("header: %s", out)
			}
			if errOut != "" {
				t.Errorf("machine mode wrote prose to stderr: %q", errOut)
			}
		})
	}
}

func TestMachineSuccessCarriesTheClientOwnedResult(t *testing.T) {
	_, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "header-id")
		_, _ = w.Write([]byte(routerBody))
	})
	h := fixedSearchOps(root)
	code, out, _ := runSearchStdin(t, h, map[string]string{"TOKENDROP_API_KEY": "k"},
		`{"version":1,"query":"how do ports work"}`, "-config", cfg)
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	env := decodeEnvelope(t, out)
	// The normalized identity, not a re-read of the raw body.
	if env["request_id"] != "01a03e86-fictional" {
		t.Errorf("request_id %v", env["request_id"])
	}
	result, ok := env["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %s", out)
	}
	if result["request_id"] != "01a03e86-fictional" || result["chosen"] != float64(1) ||
		result["session_id"] != "sess-9" || result["latency_ms"] != float64(12) {
		t.Errorf("result: %v", result)
	}
	cands, _ := result["candidates"].([]any)
	if len(cands) != 2 {
		t.Fatalf("candidates: %v", result["candidates"])
	}
	chosen, _ := cands[1].(map[string]any)
	if chosen["provider"] != "fictional" || chosen["chosen"] != true ||
		chosen["answer"] != "Ports number the endpoints." {
		t.Errorf("chosen candidate: %v", chosen)
	}
	if _, echoed := result["query"]; echoed {
		t.Error("the machine result echoes the submitted query back")
	}
}

// ── §22 the classifier ──────────────────────────────────────────────────

func TestRecoveryClassificationComesFromTypesNotText(t *testing.T) {
	for _, tc := range []struct {
		name          string
		out           searchOutcome
		wantExit      int
		wantCode      string
		wantRetryable bool
		wantAction    string
	}{
		{"success", searchOutcome{}, exitOK, "ok", false, actionNone},
		{
			"transport error", searchOutcome{Fault: faultTransport, Err: errors.New("connection refused")},
			exitTransport, "transport_failed", true, actionRetry,
		},
		{
			"context deadline", searchOutcome{Fault: faultTimeout},
			exitTransport, "search_timeout", true, actionRetry,
		},
		{
			"caller cancellation", searchOutcome{Fault: faultCanceled},
			exitTransport, "canceled", false, actionNone,
		},
		{
			"no local credential", searchOutcome{Fault: faultNoCredential},
			exitClientErr, "not_connected", false, actionConnect,
		},
		{
			"router 400", routerOutcome(400, "invalid_query", "the query is empty"),
			exitClientErr, "invalid_query", false, actionFixInput,
		},
		{
			"router 401", routerOutcome(401, "unauthorized", "key not accepted"),
			exitClientErr, "unauthorized", false, actionLogin,
		},
		{
			"router 403", routerOutcome(403, "forbidden", "no access to this tier"),
			exitClientErr, "forbidden", false, actionCheckAccess,
		},
		{
			"router 404", routerOutcome(404, "not_found", "no such route"),
			exitClientErr, "not_found", false, actionReport,
		},
		{
			"router 409", routerOutcome(409, "conflict", "in use"),
			exitClientErr, "conflict", false, actionReport,
		},
		{
			"router 422", routerOutcome(422, "invalid_tier", "unknown tier"),
			exitClientErr, "invalid_tier", false, actionFixInput,
		},
		{
			"router 429", routerOutcome(429, "rate_limited", "slow down"),
			exitClientErr, "rate_limited", true, actionRetry,
		},
		{
			"router 500", routerOutcome(500, "internal", "boom"),
			exitServerErr, "internal", true, actionRetry,
		},
		{
			"router 503", routerOutcome(503, "unavailable", "maintenance"),
			exitServerErr, "unavailable", true, actionRetry,
		},
		{
			"invalid 2xx body", searchOutcome{Fault: faultInvalidResponse, HTTPStatus: 200, Err: errBodyNotJSON},
			exitServerErr, "invalid_router_response", false, actionReport,
		},
		{
			"oversized 2xx", searchOutcome{Fault: faultOversized, HTTPStatus: 200, Err: errBodyOversized},
			exitServerErr, "router_response_too_large", false, actionReport,
		},
		{
			"missing request id", searchOutcome{Fault: faultMissingID, HTTPStatus: 200, Err: errNoRequestID},
			exitServerErr, "missing_request_id", false, actionReport,
		},
		// A router error with no usable code falls back to the client's
		// own, rather than promoting a sentence into a machine identifier.
		{
			"router code is a sentence", routerOutcome(403, "you are not allowed to do that, sorry", "prose"),
			exitClientErr, "forbidden", false, actionCheckAccess,
		},
		{
			"router supplies no code", routerOutcome(500, "", "boom"),
			exitServerErr, "router_error", true, actionRetry,
		},

		// The critical proof. Each of these has text that says one thing
		// and a type or status that means another. Classification follows
		// the type.
		{
			"transport error whose text says unauthorized",
			searchOutcome{Fault: faultTransport, Err: errors.New("401 unauthorized: please log in again")},
			exitTransport, "transport_failed", true, actionRetry,
		},
		{
			"400 whose prose says unauthorized, retry later",
			routerOutcome(400, "invalid_query", "unauthorized — retry later"),
			exitClientErr, "invalid_query", false, actionFixInput,
		},
		{
			"400 whose prose says trace_unsupported",
			routerOutcome(400, "invalid_query", "trace_unsupported is not the reason"),
			exitClientErr, "invalid_query", false, actionFixInput,
		},
		{
			"invalid body whose text says retry later",
			searchOutcome{Fault: faultInvalidResponse, HTTPStatus: 200, Err: errors.New("retry later, unauthorized")},
			exitServerErr, "invalid_router_response", false, actionReport,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifySearch(tc.out)
			if got.ExitCode != tc.wantExit || got.Code != tc.wantCode ||
				got.Retryable != tc.wantRetryable || got.Action != tc.wantAction {
				t.Errorf("classification = %+v, want exit %d code %q retryable %v action %q",
					got, tc.wantExit, tc.wantCode, tc.wantRetryable, tc.wantAction)
			}
		})
	}
}

func routerOutcome(status int, code, message string) searchOutcome {
	return searchOutcome{
		Fault:        faultRouterStatus,
		HTTPStatus:   status,
		RouterErr:    searchHostError{Code: code, Error: message},
		HasRouterErr: true,
	}
}

func TestRetryAfterReachesTheEnvelopeWhenTheRouterSuppliesIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		status int
		want   any
	}{
		{"429 with seconds", "30", 429, float64(30000)},
		{"503 with seconds", "5", 503, float64(5000)},
		{"429 with nonsense", "soon", 429, nil},
		{"429 with none", "", 429, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, _ *http.Request) {
				if tc.header != "" {
					w.Header().Set("Retry-After", tc.header)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"code":"rate_limited","error":"slow down"}`))
			})
			h := fixedSearchOps(root)
			_, out, _ := runSearchStdin(t, h, map[string]string{"TOKENDROP_API_KEY": "k"},
				`{"version":1,"query":"q"}`, "-config", cfg)
			env := decodeEnvelope(t, out)
			if got := env["retry_after_ms"]; got != tc.want {
				t.Errorf("retry_after_ms %v, want %v", got, tc.want)
			}
		})
	}
}

// ── §12 mining state, separate from search success ──────────────────────

func TestMiningStateIsReportedWithoutChangingSearchSuccess(t *testing.T) {
	for _, tc := range []struct {
		name      string
		enabled   bool
		wantState string
		wantRec   bool
	}{
		{"mining on", true, string(auth.MiningEnabled), true},
		{"mining off", false, string(auth.MiningDisabled), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(routerBody))
			})
			store, err := auth.OpenStore(filepath.Join(root, "state"))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SaveMiningEnabled(tc.enabled); err != nil {
				t.Fatal(err)
			}
			h := fixedSearchOps(root)
			code, out, _ := runSearchStdin(t, h, map[string]string{"TOKENDROP_API_KEY": "k"},
				`{"version":1,"query":"q"}`, "-config", cfg)
			// A successful search is successful whatever mining is doing.
			if code != exitOK {
				t.Fatalf("exit %d: %s", code, out)
			}
			env := decodeEnvelope(t, out)
			if env["ok"] != true {
				t.Errorf("ok %v", env["ok"])
			}
			mining, ok := env["mining"].(map[string]any)
			if !ok {
				t.Fatalf("no mining object: %s", out)
			}
			if mining["state"] != tc.wantState {
				t.Errorf("mining state %v, want %q", mining["state"], tc.wantState)
			}
			if mining["recorded"] != tc.wantRec {
				t.Errorf("mining recorded %v, want %v", mining["recorded"], tc.wantRec)
			}
			// Nothing anywhere calls this earned.
			if strings.Contains(strings.ToLower(out), "earn") {
				t.Errorf("the envelope claims earnings: %s", out)
			}
		})
	}
}

// A capture failure degrades mining and says so, and the search still
// succeeds (AGENTS.md invariant 1).
func TestACaptureFailureDegradesMiningButNotTheSearch(t *testing.T) {
	_, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(routerBody))
	})
	// Make the intake directory unusable by putting a file where it goes.
	if err := os.WriteFile(filepath.Join(root, "intake"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := fixedSearchOps(root)
	code, out, _ := runSearchStdin(t, h, map[string]string{"TOKENDROP_API_KEY": "k"},
		`{"version":1,"query":"q"}`, "-config", cfg)
	if code != exitOK {
		t.Fatalf("a mining capture failure changed the search exit: %d %s", code, out)
	}
	env := decodeEnvelope(t, out)
	if env["ok"] != true {
		t.Fatalf("ok %v", env["ok"])
	}
	mining, _ := env["mining"].(map[string]any)
	if mining == nil || mining["recorded"] != false {
		t.Fatalf("mining: %v", mining)
	}
	health, _ := mining["health"].([]any)
	if len(health) == 0 {
		t.Errorf("a capture failure left no open health reason: %v", mining)
	}
}

// ── §18 secret absence ──────────────────────────────────────────────────

func TestNoSeededSecretReachesTheEnvelopeOrTheHumanError(t *testing.T) {
	const (
		routerKey    = "sr-canary-9d41ffb0routerkey"
		refreshShape = "rt_canary_0f11ee22refreshtoken"
		dpopCanary   = "-----BEGIN PRIVATE KEY----- canarydpopmaterial"
		otherCanary  = "unrelated-canary-3b7c1d9e"
	)
	_, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"unauthorized","error":"key not accepted"}`))
	})
	h := fixedSearchOps(root)
	env := map[string]string{
		"TOKENDROP_API_KEY":   routerKey,
		"TOKENDROP_REFRESH":   refreshShape,
		"TOKENDROP_DPOP":      dpopCanary,
		"TOKENDROP_UNRELATED": otherCanary,
	}
	_, machineOut, machineErr := runSearchStdin(t, h, env, `{"version":1,"query":"q"}`, "-config", cfg)
	_, humanOut, humanErr := runSearch(t, h, env, "-config", cfg, "q")

	for _, secret := range []string{routerKey, refreshShape, dpopCanary, otherCanary} {
		for name, text := range map[string]string{
			"machine stdout": machineOut, "machine stderr": machineErr,
			"human stdout": humanOut, "human stderr": humanErr,
		} {
			if strings.Contains(text, secret) {
				t.Errorf("%s echoed a seeded secret %q:\n%s", name, secret, text)
			}
		}
	}
	// Sanity: the failure was actually reported, so the absence above is
	// not the absence of any output at all.
	if got := envField(t, decodeEnvelope(t, machineOut), "action"); got != actionLogin {
		t.Errorf("action %v", got)
	}
}
