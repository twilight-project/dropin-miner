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
// Two knowing trade-offs, documented rather than hidden: the query rides in
// process arguments (visible in `ps` and shell history on the user's own
// machine — it is not a credential; the key stays in the environment), and
// a search with no hook around it has thinner lineage.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/config"
)

const (
	searchMaxBody     = 32 << 20
	searchUserAgent   = "dropin-miner"
	renderTotalCap    = 64 << 10
	renderAnswerCap   = 4000
	renderSnippetCap  = 400
	renderCitationCap = 8
)

type searchOps struct {
	getppid  func() int
	hostname func() (string, error)
	getwd    func() (string, error)
	// spawnFlush starts the detached flush after a served search; nil
	// means "do not" (tests, or -no-flush).
	spawnFlush func(cfgPath string) error
	now        func() time.Time
	// hook is the filesystem the lineage file is read and bumped through.
	hook hookOps
}

func realSearchOps() searchOps {
	return searchOps{
		getppid:    os.Getppid,
		hostname:   os.Hostname,
		getwd:      os.Getwd,
		spawnFlush: startFlush,
		now:        time.Now,
		hook:       realHookOps(),
	}
}

func cmdSearch(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	ops := realSearchOps()
	ops.hook.getenv = getenv
	return searchMain(ops, args, stdout, stderr, getenv)
}

func searchMain(ops searchOps, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := newFlagSet("search", stderr)
	cfgPath := fs.String("config", "", "path to TOML config file")
	tier := fs.String("tier", "", "search tier accepted by the router, e.g. fast; empty = the router's default")
	format := fs.String("format", "json", "output: json (the router's bytes, verbatim) or model (compact text for an agent)")
	noFlush := fs.Bool("no-flush", false, "do not start a flush after this search")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		fmt.Fprintln(stderr, "dropin-miner search: a query is required: dropin-miner search [-tier fast] [-format model] <query words>")
		return exitUsage
	}
	if *format != "json" && *format != "model" {
		fmt.Fprintf(stderr, "dropin-miner search: -format must be json or model, not %q\n", *format)
		return exitUsage
	}

	cfg, cfgSource, err := loadConfig(*cfgPath, getenv)
	if err != nil {
		fmt.Fprintf(stderr, "dropin-miner: config (%s): %v\n", orDefaults(cfgSource), err)
		return exitTransport
	}
	if cfg.Miner.RouterURL == nil {
		fmt.Fprintln(stderr, "dropin-miner: no router configured (miner.router_url or a [[provider]] upstream)")
		return exitTransport
	}
	key := apiKey(getenv)
	if key == "" {
		fmt.Fprintln(stderr, "dropin-miner: TOKENDROP_API_KEY is not set; the router needs your sr- key to meter the search")
		return exitClientErr
	}

	ctx, cancel := signalContext()
	defer cancel()

	body := map[string]any{"query": query}
	if *tier != "" {
		body["tier"] = *tier
	}
	traced := false
	if env := searchTrace(ops, cfg.Miner, getenv); env != nil {
		body["trace"] = env
		traced = true
	}

	endpoint := strings.TrimRight(cfg.Miner.RouterURL.String(), "/") + "/v1/search"
	client := &http.Client{Timeout: 0}
	do := func() (*http.Response, error) {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", searchUserAgent+"/"+strings.TrimPrefix(buildVersion(), "v"))
		req.Header.Set("Authorization", "Bearer "+key)
		return client.Do(req)
	}

	started := ops.now()
	resp, err := do()
	if err == nil && traced && (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity) {
		// A schema-strict router refused the traced request: retry once
		// bare. A trace must never cost a search.
		_ = resp.Body.Close()
		fmt.Fprintln(stderr, "dropin-miner search: the router rejected the trace field; retrying without it")
		delete(body, "trace")
		resp, err = do()
	}
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner: router:", err)
		return exitTransport
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, searchMaxBody))
	finished := ops.now()
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner: response interrupted:", err)
		return exitTransport
	}

	var parsed routerResponse
	_ = json.Unmarshal(raw, &parsed)

	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		requestID := resp.Header.Get("X-Request-Id")
		if requestID == "" {
			requestID = parsed.RequestID
		}
		if cfg.Miner.Enabled && requestID != "" {
			rec := intakeRecord{
				RequestID:  requestID,
				Host:       cfg.Miner.RouterURL.Host,
				StatusCode: resp.StatusCode,
				StartedAt:  started,
				FinishedAt: finished,
			}
			if c := parsed.chosen(); c != nil {
				rec.ChosenProvider = c.Provider
			}
			if _, err := writeIntake(cfg.Miner.IntakeDir, rec); err != nil {
				fmt.Fprintln(stderr, "dropin-miner search: could not record the request for mining:", err)
			} else if !*noFlush && ops.spawnFlush != nil {
				_ = ops.spawnFlush(*cfgPath) // best effort; the next search or session flushes it
			}
		}
	}

	switch *format {
	case "model":
		if resp.StatusCode >= 200 && resp.StatusCode <= 299 && parsed.RequestID != "" {
			fmt.Fprint(stdout, renderForModel(parsed))
		} else {
			_, _ = stdout.Write(raw)
		}
	default:
		_, _ = stdout.Write(raw)
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
		return exitOK
	case resp.StatusCode >= 500:
		fmt.Fprintf(stderr, "\ndropin-miner: HTTP %s\n", resp.Status)
		return exitServerErr
	default:
		fmt.Fprintf(stderr, "\ndropin-miner: HTTP %s\n", resp.Status)
		return exitClientErr
	}
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

// renderForModel is the compact text an agent reads: the chosen candidate
// first, then the rest, each with its citations. Budgets keep one search
// inside what a host shows of a command's output; the JSON stays available
// with -format json when the agent wants everything.
func renderForModel(r routerResponse) string {
	var b strings.Builder
	fmt.Fprintf(&b, "search %s", r.RequestID)
	if r.Session != nil && r.Session.ID != "" {
		fmt.Fprintf(&b, "  session %s", r.Session.ID)
	}
	fmt.Fprintf(&b, "  %d candidates\n", len(r.Candidates))
	order := make([]int, 0, len(r.Candidates))
	if r.Chosen >= 0 && r.Chosen < len(r.Candidates) {
		order = append(order, r.Chosen)
	}
	for i := range r.Candidates {
		if i != r.Chosen {
			order = append(order, i)
		}
	}
	for _, i := range order {
		c := r.Candidates[i]
		if b.Len() > renderTotalCap {
			b.WriteString("…(output cap reached; use -format json for the rest)\n")
			break
		}
		mark := ""
		if i == r.Chosen {
			mark = " (chosen)"
		}
		fmt.Fprintf(&b, "\n[%s]%s", c.Provider, mark)
		if c.Kind != "" {
			fmt.Fprintf(&b, " %s", c.Kind)
		}
		if c.Status != "" && c.Status != "ok" {
			fmt.Fprintf(&b, " status=%s", c.Status)
		}
		b.WriteByte('\n')
		if c.Error != "" {
			fmt.Fprintf(&b, "  error: %s\n", oneLine(c.Error, renderSnippetCap))
			continue
		}
		if c.Answer != "" {
			fmt.Fprintf(&b, "  answer: %s\n", oneLine(c.Answer, renderAnswerCap))
		}
		for n, cit := range c.Citations {
			if n >= renderCitationCap {
				fmt.Fprintf(&b, "  …(%d more)\n", len(c.Citations)-renderCitationCap)
				break
			}
			fmt.Fprintf(&b, "  %d. %s", n+1, cit.URL)
			if t := oneLine(cit.Title, 200); t != "" {
				fmt.Fprintf(&b, " — %s", t)
			}
			b.WriteByte('\n')
			if s := oneLine(cit.Snippet, renderSnippetCap); s != "" {
				fmt.Fprintf(&b, "     %s\n", s)
			}
		}
	}
	return b.String()
}

func oneLine(s string, cap int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > cap {
		return s[:cap] + "…"
	}
	return s
}
