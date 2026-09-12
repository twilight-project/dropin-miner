package main

// The exact-head review's corrections, each with the guard that would
// catch it coming back.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/twilight-project/dropin-miner/pkg/auth"
	"github.com/twilight-project/dropin-miner/pkg/config"
)

// ── 1. a router error body is remote text on the model path too ─────────

func TestModelModeNeverDumpsARouterErrorBodyRaw(t *testing.T) {
	oversized := strings.Repeat("A", renderTotalCap*2)
	for name, payload := range map[string]string{
		"ESC":             "\x1b[2Jwiped",
		"CSI":             "\x1b[31mred\x1b[0m",
		"OSC":             "\x1b]0;retitled\x07",
		"CR":              "real\rFAKE",
		"backspace":       "safe\x08\x08\x08\x08oops",
		"bidi":            "moc.live\u202e/example",
		"malformed UTF-8": "good \xff\xfe bad",
		"oversized":       oversized,
	} {
		t.Run(name, func(t *testing.T) {
			// A valid flat envelope carrying the hostile text, so the
			// code/message path is the one under test.
			body, err := marshalFlatError("refused", payload)
			if err != nil {
				t.Fatal(err)
			}
			_, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(body))
			})
			h := fixedSearchOps(root)
			code, out, _ := runSearch(t, h, map[string]string{"TOKENDROP_API_KEY": "k"},
				"-config", cfg, "-format", "model", "q")
			if code != exitClientErr {
				t.Fatalf("exit %d, want %d", code, exitClientErr)
			}
			assertTerminalSafe(t, out)
			if len(out) > renderTotalCap {
				t.Errorf("model error output is %d bytes, budget is %d", len(out), renderTotalCap)
			}
			if strings.Contains(out, "\x1b") || strings.Contains(out, "\x07") ||
				strings.Contains(out, "\x08") || strings.Contains(out, "\r") {
				t.Errorf("a raw control byte reached model output: %q", out)
			}
			if !utf8.ValidString(out) {
				t.Errorf("model output is not valid UTF-8: %q", out)
			}
		})
	}
}

func marshalFlatError(code, message string) (string, error) {
	raw, err := json.Marshal(map[string]string{"code": code, "error": message})
	return string(raw), err
}

// A malformed error body contributes nothing but its own description:
// there is no safe way to quote bytes that did not parse.
func TestAMalformedRouterErrorBodyIsNotEchoedInModelMode(t *testing.T) {
	const hostile = "\x1b[2J{\"code\":"
	_, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(hostile))
	})
	h := fixedSearchOps(root)
	code, out, _ := runSearch(t, h, map[string]string{"TOKENDROP_API_KEY": "k"},
		"-config", cfg, "-format", "model", "q")
	if code != exitClientErr {
		t.Fatalf("exit %d", code)
	}
	assertTerminalSafe(t, out)
	if strings.Contains(out, "\x1b") {
		t.Errorf("the malformed body reached model output: %q", out)
	}
	if !strings.Contains(out, "HTTP 400") {
		t.Errorf("model output does not carry the HTTP summary: %q", out)
	}
}

// The compatibility path is explicitly documented and stays exactly as it
// was: the router's own bytes, verbatim.
func TestJSONFormatKeepsTheRawRouterCompatibilityOutput(t *testing.T) {
	const body = "\x1b[31m{\"code\":\"refused\",\"error\":\"raw\"}"
	_, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(body))
	})
	h := fixedSearchOps(root)
	code, out, _ := runSearch(t, h, map[string]string{"TOKENDROP_API_KEY": "k"},
		"-config", cfg, "-format", "json", "q")
	if code != exitClientErr || out != body {
		t.Errorf("-format json changed: exit %d out %q", code, out)
	}
}

// ── 2. the detached resume's side-effect boundary ───────────────────────

// resumeHarness counts spawnConnectResume rather than inferring it.
func resumeHarness(root string) (*searchHarness, *int) {
	h := fixedSearchOps(root)
	resumes := 0
	h.ops.spawnConnectResume = func(string) error {
		resumes++
		return nil
	}
	return h, &resumes
}

func TestOnlyACompletedResponseMakesTheDetachedResumeEligible(t *testing.T) {
	for _, tc := range []struct {
		name      string
		transport http.RoundTripper
		handler   http.HandlerFunc
		wantResum bool
	}{
		{
			name: "transport failure before any response",
			transport: &recordingTransport{fn: func(int, *http.Request) (*http.Response, error) {
				return nil, errors.New("dial: connection refused")
			}},
		},
		{
			name: "body stalls until the deadline",
			transport: &recordingTransport{fn: func(_ int, req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK, Header: http.Header{},
					Body: blockingBody{ctx: req.Context()}, ContentLength: -1,
				}, nil
			}},
		},
		{
			name: "body is cut short",
			transport: &recordingTransport{fn: func(int, *http.Request) (*http.Response, error) {
				// The prefix arrives, then the connection dies — which is
				// io.ErrUnexpectedEOF, not a clean EOF. A fake that
				// returned clean EOF would look like a complete body and
				// this case would prove nothing.
				return &http.Response{
					StatusCode: http.StatusOK, Header: http.Header{},
					Body:          io.NopCloser(io.MultiReader(strings.NewReader(`{"request_id":"r"`), errorReader{})),
					ContentLength: 4096,
				}, nil
			}},
		},
		{
			name:      "complete 4xx",
			handler:   func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401); _, _ = w.Write([]byte(`{}`)) },
			wantResum: true,
		},
		{
			name:      "complete 5xx",
			handler:   func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503); _, _ = w.Write([]byte(`{}`)) },
			wantResum: true,
		},
		{
			name:      "complete success",
			handler:   func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(routerBody)) },
			wantResum: true,
		},
		{
			// The body arrived whole; it is simply not a usable answer.
			// That reached shouldResume before PR6 and still does.
			name:      "complete but malformed 2xx",
			handler:   func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"nope":`)) },
			wantResum: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.transport != nil {
				useTransport(t, tc.transport)
			}
			_, cfg, root := newFakeRouter(t, tc.handler)
			h, resumes := resumeHarness(root)
			// shouldResume needs something to resume, or it declines on
			// its own and this test would prove nothing.
			seedResumableRegistration(t, root)
			args := []string{"-config", cfg, "q"}
			if tc.transport != nil {
				args = append([]string{"-timeout", "150ms"}, args...)
			}
			runSearch(t, h, map[string]string{"TOKENDROP_API_KEY": "k"}, args...)
			if got := *resumes > 0; got != tc.wantResum {
				t.Errorf("connect -resume spawned=%v (%d times), want %v", got, *resumes, tc.wantResum)
			}
		})
	}
}

// seedResumableRegistration writes the one state shouldResume says yes to
// with no network and no ambiguity: an unclaimed registration. Without it
// shouldResume declines on its own and the table above would pass whatever
// the gate did.
func seedResumableRegistration(t *testing.T, root string) {
	t.Helper()
	store, err := auth.OpenStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgentRegistration(auth.AgentRegistration{
		AgentID:  "agent-fictional-resume",
		Status:   "unclaimed",
		ClaimURL: "https://portal.fictional.test/claim/abc",
	}); err != nil {
		t.Fatal(err)
	}
	if !shouldResume(&config.Config{Mining: config.Mining{StateDir: filepath.Join(root, "state")}}) {
		t.Fatal("the seeded registration is not resumable; the gate under test would prove nothing")
	}
}

// ── 3. a body deadline outranks the status line ─────────────────────────

func TestABodyThatDiesOnTheDeadlineOutranksItsStatus(t *testing.T) {
	for _, status := range []int{200, 401, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			useTransport(t, &recordingTransport{fn: func(_ int, req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: status, Header: http.Header{},
					Body: blockingBody{ctx: req.Context()}, ContentLength: -1,
				}, nil
			}})
			out := runPerformSearch(t, 150*time.Millisecond, searchCall{
				Endpoint: "https://r.test/v1/search", Key: "k", Query: "q",
			})
			if out.Fault != faultTimeout {
				t.Fatalf("HTTP %d with a stalled body gave fault %q, want %q", status, out.Fault, faultTimeout)
			}
			c := classifySearch(out)
			if c.ExitCode != exitTransport || c.Code != "search_timeout" || !c.Retryable || c.Action != actionRetry {
				t.Errorf("HTTP %d stalled: %+v", status, c)
			}
			if out.ResponseComplete {
				t.Errorf("HTTP %d stalled: the response was marked complete", status)
			}
		})
	}
}

func TestANonSuccessBodyCanceledByTheCallerIsNotRetryable(t *testing.T) {
	useTransport(t, &recordingTransport{fn: func(_ int, req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized, Header: http.Header{},
			Body: blockingBody{ctx: req.Context()}, ContentLength: -1,
		}, nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	defer cancel()
	out := performSearch(ctx, time.Now, searchCall{Endpoint: "https://r.test/v1/search", Key: "k", Query: "q"})
	if out.Fault != faultCanceled {
		t.Fatalf("fault %q, want %q", out.Fault, faultCanceled)
	}
	c := classifySearch(out)
	if c.Retryable || c.Action != actionNone {
		t.Errorf("a canceled 401 was made retryable: %+v", c)
	}
}

// The other half: a response that DID complete still classifies by status.
func TestACompleteNonSuccessStillClassifiesByStatus(t *testing.T) {
	for _, tc := range []struct {
		status     int
		wantAction string
		retryable  bool
	}{
		{401, actionLogin, false},
		{500, actionRetry, true},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			useTransport(t, &recordingTransport{fn: func(int, *http.Request) (*http.Response, error) {
				return fakeResponse(tc.status, `{"code":"c","error":"e"}`, nil), nil
			}})
			out := runPerformSearch(t, 30*time.Second, searchCall{
				Endpoint: "https://r.test/v1/search", Key: "k", Query: "q",
			})
			if out.Fault != faultRouterStatus || !out.ResponseComplete {
				t.Fatalf("fault %q complete %v", out.Fault, out.ResponseComplete)
			}
			c := classifySearch(out)
			if c.Action != tc.wantAction || c.Retryable != tc.retryable {
				t.Errorf("HTTP %d: %+v", tc.status, c)
			}
		})
	}
}

// ── 6. fix_input only for what the caller can fix ───────────────────────

func TestOnlyCallerFixableStatusesSayFixInput(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		// The request the caller composed.
		{400, actionFixInput},
		{413, actionFixInput},
		{422, actionFixInput},
		// Chosen by this client, not by its caller.
		{405, actionReport},
		{406, actionReport},
		{411, actionReport},
		{414, actionReport},
		{415, actionReport},
		{431, actionReport},
		// The ones with their own meaning.
		{401, actionLogin},
		{403, actionCheckAccess},
		{429, actionRetry},
		{404, actionReport},
		{409, actionReport},
	} {
		c := classifySearch(searchOutcome{Fault: faultRouterStatus, HTTPStatus: tc.status})
		if c.Action != tc.want {
			t.Errorf("HTTP %d → action %q, want %q", tc.status, c.Action, tc.want)
		}
		if tc.want == actionFixInput && c.Retryable {
			t.Errorf("HTTP %d is fix_input and retryable", tc.status)
		}
	}
}
