package main

// The search command: the CLI as the tool.
//
//	dropin-miner search [-tier fast] [-format json|model] <query words>
//
// posts the query straight to the search router and prints the answer.
// No daemon, no proxy, no tool server: a skill names this command and any
// agent that can run a command can use it. What makes it a miner:
//
//   - the router meters the request against the participant's own key,
//     which is sent in Authorization exactly as a proxy would forward it;
//   - the served request id (X-Request-Id) is written to the intake
//     directory the moment the answer arrives, and a detached flush is
//     started to join the open epoch and submit it;
//   - the trace envelope rides in the body so the router can group one
//     task's searches. It comes from, in order: the TOKENDROP_TRACE_BRIDGE
//     variable a hook put in front of this command; the workspace lineage
//     file a hook wrote (named by TOKENDROP_LINEAGE, or found by walking
//     up from the working directory); or, with no hook at all, a hashed
//     per-shell session identity. TOKENDROP_TRACE=off sends none.
//
// Two knowing trade-offs, documented rather than hidden: an argv query
// rides in process arguments (visible in `ps` and shell history on the
// user's own machine — it is not a credential; the key comes from the
// environment or the owner-only credentials file, see credentials.go), and
// a search with no hook around it has thinner lineage. The first of those
// is why agents are pointed at `search --stdin` instead.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/auth"
	"github.com/twilight-project/dropin-miner/pkg/config"
)

const (
	searchUserAgent   = "dropin-miner"
	renderTotalCap    = 64 << 10
	renderAnswerCap   = 4000
	renderSnippetCap  = 400
	renderCitationCap = 8
)

// intakeWriteBlocked reports whether an intake write failed because the
// directory is not writable from inside an agent's sandbox — EACCES on
// Linux (Landlock), EROFS on macOS (Seatbelt). The EROFS case is matched by
// string so the check stays correct on every GOOS without a syscall import.
func intakeWriteBlocked(err error) bool {
	return errors.Is(err, fs.ErrPermission) || strings.Contains(err.Error(), "read-only file system")
}

type searchOps struct {
	getppid  func() int
	hostname func() (string, error)
	getwd    func() (string, error)
	// spawnFlush starts the detached flush after a served search; nil
	// means "do not" (tests, or -no-flush).
	spawnFlush func(cfgPath string) error
	// spawnConnectResume starts the detached connect -resume (agent
	// onboarding design §5.5's "next invocation of anything" — in
	// practice, search: the one path an agent invokes routinely). Called
	// after every served search, independent of spawnFlush/-no-flush and
	// of [mining]/[miner] being configured at all — a search-only
	// unclaimed participant has neither. shouldResume gates it on a
	// cheap local disk check first, so this never fires when there is
	// nothing to resume.
	spawnConnectResume func(cfgPath string) error
	now                func() time.Time
	// hook is the filesystem the lineage file is read and bumped through.
	hook hookOps
}

func realSearchOps() searchOps {
	return searchOps{
		getppid:            os.Getppid,
		hostname:           os.Hostname,
		getwd:              os.Getwd,
		spawnFlush:         startFlush,
		spawnConnectResume: startConnectResume,
		now:                time.Now,
		hook:               realHookOps(),
	}
}

func cmdSearch(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	ops := realSearchOps()
	ops.hook.getenv = getenv
	return searchMain(ops, args, stdin, stdout, stderr, getenv)
}

// searchFault names, as a value rather than as prose, why a search did not
// produce a protocol success. The empty value means it did.
//
// Every recovery decision the client or an agent makes is derived from one
// of these plus the HTTP status — never from the text of an error. That is
// the whole reason the type exists: message wording that classification
// depends on is an API nobody agreed to, and one rephrasing silently
// changes what a participant is told to do.
type searchFault string

const (
	faultNone            searchFault = ""
	faultNoCredential    searchFault = "not_connected"
	faultTransport       searchFault = "transport"
	faultTimeout         searchFault = "search_timeout"
	faultCanceled        searchFault = "canceled"
	faultRouterStatus    searchFault = "router_status"
	faultOversized       searchFault = "router_response_too_large"
	faultInvalidResponse searchFault = "invalid_router_response"
	faultMissingID       searchFault = "missing_request_id"
)

// routerAttempt is one POST's answer, already bounded.
type routerAttempt struct {
	Status  int
	Header  http.Header
	Raw     []byte
	BodyErr error
}

// searchCall is everything one search sends.
type searchCall struct {
	Endpoint string
	Key      string
	Query    string
	Tier     string
	Trace    *traceEnvelope
}

// searchOutcome is the structured result of running a search. Both
// renderers — the human one and the machine envelope — are built from it,
// so they cannot disagree, and neither is produced by parsing the other.
type searchOutcome struct {
	Fault searchFault
	// Err is diagnostic only. Nothing branches on its text.
	Err           error
	HTTPStatus    int
	RouterErr     searchHostError
	HasRouterErr  bool
	RetryAfterMS  int64
	HasRetryAfter bool

	Success  routerSuccess
	RawBody  []byte
	Started  time.Time
	Finished time.Time
	Attempts int
	Traced   bool
	Retried  bool
}

func (o searchOutcome) ok() bool { return o.Fault == faultNone }

// searchMachineRequested answers "did the caller ask for --stdin" before
// flag parsing, so a flag error can still be reported in the format the
// caller asked for rather than as prose they are not reading.
func searchMachineRequested(args []string) bool { return hasBoolFlag(args, "stdin") }

func searchMain(ops searchOps, args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	machine := searchMachineRequested(args)
	// In machine mode everything is in the envelope. An expected protocol
	// outcome must not also explain itself on stderr: a caller reading
	// both would get the same failure twice, in two formats, and the one
	// that is not the contract is the one it would be tempted to parse.
	if machine {
		stderr = io.Discard
	}

	fs := newFlagSet("search", stderr)
	cfgPath := fs.String("config", "", "path to TOML config file")
	tier := fs.String("tier", "", "search tier accepted by the router, e.g. fast; empty = the router's default")
	format := fs.String("format", "json", "output: json (the router's bytes, verbatim) or model (compact text for an agent)")
	noFlush := fs.Bool("no-flush", false, "do not start a flush after this search")
	timeout := fs.Duration("timeout", defaultSearchTimeout, "whole-search deadline, covering connect, headers, body and the one trace-compatibility retry")
	fs.Bool("stdin", false, "read one version-1 JSON search request from stdin and answer with the machine envelope")
	if err := fs.Parse(args); err != nil {
		if machine {
			return failSearchInput(stdout, inputErrorf(codeInvalidFlags, "the flags could not be parsed"))
		}
		return exitUsage
	}

	var query string
	if machine {
		// No positional query in machine mode: two sources for the same
		// value is how a caller ends up sending one and escaping the
		// other.
		if len(fs.Args()) > 0 {
			return failSearchInput(stdout, inputErrorf(codeUnexpectedArgument,
				"--stdin takes the query from the request on stdin, not from the command line"))
		}
		req, err := decodeMachineSearchRequest(stdin)
		if err != nil {
			var ierr *inputError
			if errors.As(err, &ierr) {
				return failSearchInput(stdout, ierr)
			}
			return failSearchInput(stdout, inputErrorf(codeInvalidJSON, "the request could not be read"))
		}
		query = req.query
		if req.tier != "" {
			*tier = req.tier
		}
	} else {
		query = strings.TrimSpace(strings.Join(fs.Args(), " "))
		if query == "" {
			fmt.Fprintln(stderr, "dropin-miner search: a query is required: dropin-miner search [-tier fast] [-format model] <query words>, or --stdin")
			return exitUsage
		}
		if *format != "json" && *format != "model" {
			fmt.Fprintf(stderr, "dropin-miner search: -format must be json or model, not %q\n", *format)
			return exitUsage
		}
	}
	// Refused rather than treated as "no limit": a zero or negative budget
	// used to be the only state this command had, and restoring it by
	// accident is the failure the deadline exists to prevent.
	if *timeout <= 0 {
		if machine {
			return failSearchInput(stdout, inputErrorf(codeInvalidFlags, "-timeout must be positive"))
		}
		fmt.Fprintf(stderr, "dropin-miner search: -timeout must be positive, not %s\n", *timeout)
		return exitUsage
	}

	cfg, cfgSource, err := loadConfig(*cfgPath, getenv)
	if err != nil {
		if machine {
			return failSearchSetup(stdout, exitTransport, "config_unreadable", actionFixInput, err)
		}
		fmt.Fprintf(stderr, "dropin-miner: config (%s): %v\n", orDefaults(cfgSource), err)
		return exitTransport
	}
	if cfg.Miner.RouterURL == nil {
		if machine {
			return failSearchSetup(stdout, exitTransport, "no_router_configured", actionFixInput,
				errors.New("no router configured (miner.router_url or a [[provider]] upstream)"))
		}
		fmt.Fprintln(stderr, "dropin-miner: no router configured (miner.router_url or a [[provider]] upstream)")
		return exitTransport
	}
	key, keySrc, err := resolveAPIKey(getenv, cfg.Miner)
	if err != nil {
		if machine {
			return failSearchSetup(stdout, exitClientErr, "credential_unreadable", actionConnect, err)
		}
		fmt.Fprintln(stderr, "dropin-miner:", err)
		return exitClientErr
	}
	if key == "" {
		if machine {
			// The value is never named, only its absence. There is
			// nothing in this envelope to leak.
			return failSearchSetup(stdout, exitClientErr, string(faultNoCredential), actionConnect,
				errors.New("this installation holds no search credential; run dropin-miner connect"))
		}
		fmt.Fprintln(stderr, "dropin-miner: no API key; the router needs your sr- key to meter the search. Store it once with: dropin-miner login   (or export TOKENDROP_API_KEY)")
		return exitClientErr
	}

	ctx, cancel := searchDeadline(*timeout)
	defer cancel()

	out := performSearch(ctx, ops.now, searchCall{
		Endpoint: strings.TrimRight(cfg.Miner.RouterURL.String(), "/") + "/v1/search",
		Key:      key,
		Query:    query,
		Tier:     *tier,
		Trace:    searchTrace(ops, cfg.Miner, getenv),
	})
	if out.Retried {
		fmt.Fprintln(stderr, "dropin-miner search: the router answered "+traceUnsupportedCode+"; retrying once without the trace")
	}

	mining := recordSearchForMining(ops, cfg, out, *cfgPath, *noFlush, stderr)

	// Independent of cfg.Miner.Enabled/-no-flush above: a search-only
	// unclaimed participant has neither [mining] nor [miner] configured
	// at all, and still needs the claim to resolve eventually. shouldResume
	// is a cheap local disk check (agent onboarding design §5.5) — it
	// costs nothing and spawns nothing when there is no stored
	// registration to resume.
	if ops.spawnConnectResume != nil && shouldResume(cfg) {
		_ = ops.spawnConnectResume(*cfgPath) // best effort; the next search resumes it if this one could not even start
	}

	if machine {
		env, code := searchEnvelopeOf(out, &mining)
		emitMachine(stdout, env)
		return code
	}
	return renderSearchForHuman(out, *format, keySrc, stdout, stderr)
}

// renderSearchForHuman is the terminal path: the router's own bytes for
// -format json, compact text for -format model, and a bounded diagnostic
// on stderr. An invalid success is the one case that prints no body at
// all — echoing a malformed or oversized router response back is how
// untrusted bytes reach a terminal unbounded.
func renderSearchForHuman(out searchOutcome, format string, keySrc keySource, stdout, stderr io.Writer) int {
	if out.ok() {
		if format == "model" {
			fmt.Fprint(stdout, renderForModel(out.Success.Response))
		} else {
			_, _ = stdout.Write(out.Success.Raw)
		}
		return exitOK
	}
	switch out.Fault {
	case faultTimeout:
		fmt.Fprintln(stderr, "dropin-miner: the search did not finish within its deadline; retry, or raise -timeout")
		return exitTransport
	case faultCanceled:
		fmt.Fprintln(stderr, "dropin-miner: the search was canceled")
		return exitTransport
	case faultTransport:
		fmt.Fprintln(stderr, "dropin-miner: router:", out.Err)
		return exitTransport
	case faultOversized, faultInvalidResponse, faultMissingID:
		fmt.Fprintf(stderr, "dropin-miner: the router answered HTTP %d, but the answer is not a usable search response: %v\n",
			out.HTTPStatus, out.Err)
		return exitServerErr
	}
	// A router status. The body is the router's own, echoed as it always
	// was for the compatibility path.
	_, _ = stdout.Write(out.RawBody)
	switch {
	case out.HTTPStatus >= 500:
		fmt.Fprintf(stderr, "\ndropin-miner: HTTP %d\n", out.HTTPStatus)
		return exitServerErr
	case out.HTTPStatus == http.StatusUnauthorized:
		fmt.Fprintf(stderr, "\ndropin-miner: HTTP %d — the router refused the key (from %s); store a valid one with: dropin-miner login\n", out.HTTPStatus, keySrc)
		return exitClientErr
	default:
		fmt.Fprintf(stderr, "\ndropin-miner: HTTP %d\n", out.HTTPStatus)
		return exitClientErr
	}
}

// recordSearchForMining is the mining side of a served search, and it is
// deliberately the only thing between the router's answer and the exit
// code. Nothing in here can change what the search returned: AGENTS.md
// invariant 1 — a mining-side write, spawn or state failure must never
// turn a successful search into a failed one.
// It returns the mining state as a value so the machine envelope reports
// the same thing the human stderr lines say, rather than a second reading
// of the same store — and so a caller can tell a search that earned
// nothing because mining is off from one that earned nothing because
// capture broke.
func recordSearchForMining(ops searchOps, cfg *config.Config, out searchOutcome, cfgPath string, noFlush bool, stderr io.Writer) machineMining {
	// The persisted decision is read either way: it is a local disk read,
	// it changes nothing, and a failed search still has a mining state
	// worth reporting.
	decision, mstore := inspectMiningState(cfg.Mining.StateDir)
	snap := machineMining{State: string(decision.State), Configured: cfg.Miner.Enabled}
	if decision.Err != nil {
		snap.Detail = boundMessage(decision.Err.Error())
	}
	finish := func() machineMining {
		if mstore != nil {
			records, _ := mstore.HealthRecords()
			snap.Health = healthOf(records)
		}
		return snap
	}
	if !out.ok() || !cfg.Miner.Enabled {
		return finish()
	}
	// [miner] enabled says intake is configured; the persisted decision is
	// the only runtime authority. An undecided participant is silent. An
	// unreadable state is fail-closed for mining but visible as a concise
	// diagnostic; neither state-store nor health failures may replace a
	// successful router answer.
	if decision.State == auth.MiningDegraded {
		detail := miningDecisionDetail(decision)
		if mstore != nil {
			_ = mstore.MarkHealth(auth.HealthDecision, auth.HealthDecisionUnreadable, detail)
		}
		fmt.Fprintf(stderr, "dropin-miner: the search succeeded, but mining capture is unavailable because local mining state cannot be safely trusted%s\n",
			detail)
	}
	if decision.State != auth.MiningEnabled {
		return finish()
	}
	rec := intakeRecord{
		RequestID:  out.Success.RequestID,
		Host:       cfg.Miner.RouterURL.Host,
		StatusCode: out.HTTPStatus,
		StartedAt:  out.Started,
		FinishedAt: out.Finished,
	}
	if c := out.Success.Response.chosen(); c != nil {
		rec.ChosenProvider = c.Provider
	}
	if _, err := writeIntake(cfg.Miner.IntakeDir, rec); err != nil {
		reason := auth.HealthIntakeUnwritable
		if intakeWriteBlocked(err) {
			reason = auth.HealthSandboxRestricted
			fmt.Fprintf(stderr, "dropin-miner: the search worked, but its mining observation could NOT be\n"+
				"  recorded — %s is not writable from inside this agent's sandbox, so\n"+
				"  searches run here earn nothing. Let the agent write to that directory.\n"+
				"  For Codex, re-run `dropin-miner agents install`, which now configures it.\n",
				minerRoot(cfg.Miner))
		} else {
			fmt.Fprintln(stderr, "dropin-miner search: could not record the request for mining:", err)
		}
		if mstore != nil {
			_ = mstore.MarkHealth(auth.HealthCapture, reason, err.Error())
		}
		return finish()
	}
	snap.Recorded = true
	if mstore != nil {
		_ = mstore.ClearHealth(auth.HealthCapture)
	}
	if !noFlush && ops.spawnFlush != nil {
		if err := ops.spawnFlush(cfgPath); err != nil {
			if mstore != nil {
				_ = mstore.MarkHealth(auth.HealthFlush, auth.HealthFlushSpawnFailed, err.Error())
			}
			fmt.Fprintln(stderr, "dropin-miner search: could not start mining flush:", err)
		}
	}
	return finish()
}

// performSearch runs the whole protocol operation under one context: at
// most two POSTs, the second only on an explicit trace_unsupported, and
// both sharing ctx's single absolute deadline.
func performSearch(ctx context.Context, now func() time.Time, call searchCall) searchOutcome {
	body := map[string]any{"query": call.Query}
	if call.Tier != "" {
		body["tier"] = call.Tier
	}
	out := searchOutcome{Traced: call.Trace != nil}
	if out.Traced {
		body["trace"] = call.Trace
	}

	// CheckRedirect: this request carries the participant's sr- key in
	// Authorization. net/http's default follows up to ten redirects and
	// replays both the header and the body on a 307/308 — a compromised or
	// misconfigured router redirecting this request would hand the key to
	// whatever host it named. Same-origin bounded, not refused outright: a
	// router legitimately redirecting within its own origin must not break
	// every search. Timeout stays 0: the deadline is ctx's, so it covers
	// the body read too, which a client Timeout would also do but could
	// not share across the two attempts.
	client := &http.Client{Timeout: 0, CheckRedirect: auth.SameOriginRedirects, Transport: searchTransport}

	out.Started = now()
	attempt, err := postSearch(ctx, client, call, body)
	out.Attempts++
	if err == nil && out.Traced && searchTraceUnsupported(attempt.Status, attempt.Raw, attempt.BodyErr) {
		// The router said, in its own machine code, that it does not
		// accept the field. One retry without it, on the SAME ctx, so the
		// fallback gets only what is left of the original budget. Never
		// recursive: this is the only place a second POST is issued.
		delete(body, "trace")
		out.Retried = true
		attempt, err = postSearch(ctx, client, call, body)
		out.Attempts++
	}
	out.Finished = now()

	if err != nil {
		out.Err = err
		out.Fault = contextFault(ctx, err, faultTransport)
		return out
	}
	out.HTTPStatus = attempt.Status
	out.RawBody = attempt.Raw
	if ms, ok := retryAfterMS(attempt.Header, now()); ok {
		out.RetryAfterMS, out.HasRetryAfter = ms, true
	}

	if attempt.Status < 200 || attempt.Status > 299 {
		out.Fault = faultRouterStatus
		if attempt.BodyErr == nil {
			if e, ok := decodeSearchHostError(attempt.Raw); ok {
				out.RouterErr, out.HasRouterErr = e, true
			}
		}
		return out
	}
	if attempt.BodyErr != nil {
		out.Err = attempt.BodyErr
		switch {
		case errors.Is(attempt.BodyErr, errBodyOversized):
			out.Fault = faultOversized
		default:
			// A body read that died on the deadline is a timeout, not a
			// malformed router: §19's "the body read is covered by it".
			out.Fault = contextFault(ctx, attempt.BodyErr, faultInvalidResponse)
		}
		return out
	}
	success, derr := decodeRouterSuccess(attempt.Raw, attempt.Header.Get("X-Request-Id"))
	if derr != nil {
		out.Err = derr
		if errors.Is(derr, errNoRequestID) {
			out.Fault = faultMissingID
		} else {
			out.Fault = faultInvalidResponse
		}
		return out
	}
	out.Success = success
	return out
}

// contextFault separates a deliberate cancellation from an expired
// deadline, falling back to otherwise. An agent may retry a timeout; it
// must not automatically retry something a person interrupted.
func contextFault(ctx context.Context, err error, otherwise searchFault) searchFault {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded):
		return faultTimeout
	case errors.Is(err, context.Canceled), errors.Is(ctx.Err(), context.Canceled):
		return faultCanceled
	default:
		return otherwise
	}
}

func postSearch(ctx context.Context, client *http.Client, call searchCall, body map[string]any) (routerAttempt, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return routerAttempt{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, call.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return routerAttempt{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", searchUserAgent+"/"+strings.TrimPrefix(buildVersion(), "v"))
	req.Header.Set("Authorization", "Bearer "+call.Key)
	resp, err := client.Do(req)
	if err != nil {
		return routerAttempt{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	attempt := routerAttempt{Status: resp.StatusCode, Header: resp.Header}
	if resp.ContentLength > searchMaxBody {
		// Declared oversized: refuse without draining it. The streaming
		// max+1 check below stays authoritative — Content-Length is
		// optional and can lie — but when it is present and honest there
		// is no reason to read the ceiling to learn what it already said.
		attempt.BodyErr = errBodyOversized
		return attempt, nil
	}
	attempt.Raw, attempt.BodyErr = readBoundedBody(resp.Body, searchMaxBody)
	return attempt, nil
}

// searchTrace picks the envelope for this search: bridge, lineage file,
// or the per-shell fallback. nil means send none.
func searchTrace(ops searchOps, m config.Miner, getenv func(string) string) *traceEnvelope {
	switch strings.ToLower(getenv("TOKENDROP_TRACE")) {
	case "off", "0", "false":
		return nil
	}
	harness := getenv("TOKENDROP_HARNESS")

	if bridge := getenv(bridgeEnv); bridge != "" {
		if env := decodeTraceBridge(bridge); env != nil {
			if harness != "" {
				env.Harness = harness
			}
			return capTrace(env)
		}
	}

	now := ops.now()
	var (
		lf   *lineageFile
		path string
	)
	if p := getenv(lineageEnv); p != "" {
		if l, ok := loadLineage(ops.hook, p); ok && now.Sub(l.UpdatedAt) <= lineageMaxAge {
			lf, path = l, p
		}
	}
	if lf == nil && m.SessionsDir != "" {
		if cwd, err := ops.getwd(); err == nil {
			lf, path = lineageForCwd(ops.hook, m.SessionsDir, cwd, now)
		}
	}
	if lf != nil {
		lf.Seq++
		env := lf.envelope()
		if env != nil {
			if harness != "" {
				env.Harness = harness
			}
			_ = saveLineage(ops.hook, path, lf, now)
			return capTrace(env)
		}
	}

	// No hook anywhere: the parent shell stands in for the session. One
	// agent session keeps one shell, so its pid is stable across calls.
	// Hashed like every other identifier — the raw pid/host never travel.
	host, _ := ops.hostname()
	return capTrace(&traceEnvelope{
		V:         traceVersion,
		Harness:   orString(harness, "cli"),
		SessionID: traceHash(host + "|" + strconv.Itoa(ops.getppid())),
		CallID:    traceRandomID(),
	})
}

// ── the router's answer, as much of it as the miner reads ───────────────

type routerResponse struct {
	RequestID  string            `json:"request_id"`
	Query      string            `json:"query"`
	Chosen     int               `json:"chosen"`
	Candidates []routerCandidate `json:"candidates"`
	Session    *struct {
		ID string `json:"id"`
	} `json:"session,omitempty"`
	Usage struct {
		LatencyMS int64 `json:"latency_ms"`
	} `json:"usage"`
}

type routerCandidate struct {
	Provider  string           `json:"provider"`
	Kind      string           `json:"kind"`
	Status    string           `json:"status"`
	Answer    string           `json:"answer,omitempty"`
	Error     string           `json:"error,omitempty"`
	Citations []routerCitation `json:"citations,omitempty"`
}

type routerCitation struct {
	URL     string `json:"url"`
	Title   string `json:"title,omitempty"`
	Snippet string `json:"snippet,omitempty"`
}

func (r routerResponse) chosen() *routerCandidate {
	if r.Chosen < 0 || r.Chosen >= len(r.Candidates) {
		return nil
	}
	return &r.Candidates[r.Chosen]
}
