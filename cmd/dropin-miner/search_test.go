package main

// The search command against a fake router: the query and key go out,
// the router's bytes come back, the served request is recorded for
// mining, and the trace comes from the right place. All identifiers are
// synthetic.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeRouter struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []*http.Request
	body []byte
}

func newFakeRouter(t *testing.T, handler http.HandlerFunc) (*fakeRouter, string, string) {
	t.Helper()
	f := &fakeRouter{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Clone(r.Context()))
		f.body = body
		f.mu.Unlock()
		if handler != nil {
			handler(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	root := t.TempDir()
	cfg := filepath.Join(root, "tokendrop.toml")
	toml := `[[provider]]
name = "search-router"
upstream = "https://router.fictional.test"

[mining]
enabled = true
as_url = "https://as.fictional.test"
chain_id = "fictional-1"
slot_id = 3
state_dir = "` + filepath.Join(root, "state") + `"
spool_dir = "` + filepath.Join(root, "spool") + `"

[miner]
enabled = true
router_url = "` + f.srv.URL + `"
`
	if err := os.WriteFile(cfg, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	return f, cfg, root
}

func (f *fakeRouter) last(t *testing.T) (*http.Request, []byte) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		t.Fatal("the fake router received no requests")
	}
	return f.reqs[len(f.reqs)-1], f.body
}

type searchHarness struct {
	ops     searchOps
	fs      *fakeHookFS
	flushes []string
}

func fixedSearchOps(cwd string) *searchHarness {
	h := &searchHarness{}
	fs, hops := newFakeHookOps(nil)
	// The lineage file store is in-memory, but intake goes to the real
	// temp dir the config names — that is what flush reads.
	h.fs = fs
	h.ops = searchOps{
		getppid:  func() int { return 4242 },
		hostname: func() (string, error) { return "fictional-host", nil },
		getwd:    func() (string, error) { return cwd, nil },
		spawnFlush: func(cfg string) error {
			h.flushes = append(h.flushes, cfg)
			return nil
		},
		now:  func() time.Time { return time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC) },
		hook: hops,
	}
	return h
}

func runSearch(t *testing.T, h *searchHarness, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	getenv := envOf(env)
	h.ops.hook.getenv = getenv
	code := searchMain(h.ops, args, &out, &errOut, getenv)
	return code, out.String(), errOut.String()
}

const routerBody = `{"request_id":"01a03e86-fictional","query":"how do ports work","chosen":1,"candidates":[` +
	`{"provider":"slow","kind":"retrieval","status":"ok","citations":[{"url":"https://a.test/1","title":"One","snippet":"first"}]},` +
	`{"provider":"fictional","kind":"answer","status":"ok","answer":"Ports number the endpoints.","citations":[{"url":"https://b.test/2","title":"Two","snippet":"second snippet"}]}],` +
	`"session":{"id":"sess-9"},"usage":{"latency_ms":12}}`

func TestSearchPostsToTheRouterPrintsVerbatimAndRecordsIntake(t *testing.T) {
	fr, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "01a03e86-fictional")
		_, _ = w.Write([]byte(routerBody))
	})
	h := fixedSearchOps(root)
	code, out, errOut := runSearch(t, h, map[string]string{"TOKENDROP_API_KEY": "sr-fictional"},
		"-config", cfg, "-tier", "fast", "how", "do", "ports", "work")
	if code != exitOK || out != routerBody {
		t.Fatalf("exit %d out %q err %q", code, out, errOut)
	}
	req, sent := fr.last(t)
	if req.URL.Path != "/v1/search" || req.Header.Get("Authorization") != "Bearer sr-fictional" || !strings.HasPrefix(req.Header.Get("User-Agent"), "dropin-miner/") {
		t.Errorf("request: %s auth=%q ua=%q", req.URL.Path, req.Header.Get("Authorization"), req.Header.Get("User-Agent"))
	}
	var m map[string]any
	if err := json.Unmarshal(sent, &m); err != nil || m["query"] != "how do ports work" || m["tier"] != "fast" {
		t.Errorf("body: %s", sent)
	}
	tr, _ := m["trace"].(map[string]any)
	if tr == nil || tr["harness"] != "cli" {
		t.Fatalf("trace: %v", tr)
	}
	sid := tr["session_id"].(string)
	if sid == "" || strings.Contains(sid, "4242") || strings.Contains(sid, "fictional-host") {
		t.Errorf("session id leaks raw parts: %q", sid)
	}

	recs, _, err := readIntake(filepath.Join(root, "intake"))
	if err != nil || len(recs) != 1 {
		t.Fatalf("intake: %v %v", recs, err)
	}
	rec := recs[0].rec
	if rec.RequestID != "01a03e86-fictional" || rec.StatusCode != 200 || rec.ChosenProvider != "fictional" || rec.StartedAt.IsZero() {
		t.Errorf("intake record: %+v", rec)
	}
	if len(h.flushes) != 1 || h.flushes[0] != cfg {
		t.Errorf("a flush was not started after the search: %v", h.flushes)
	}
}

func TestSearchNeedsAKeyAndDoesNotCallTheRouterWithoutOne(t *testing.T) {
	fr, cfg, root := newFakeRouter(t, nil)
	h := fixedSearchOps(root)
	code, _, errOut := runSearch(t, h, nil, "-config", cfg, "q")
	if code != exitClientErr || !strings.Contains(errOut, "TOKENDROP_API_KEY") {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if len(fr.reqs) != 0 {
		t.Error("the router was called without a key")
	}
}

func TestSearchUsesTheBridgeFromTheEnvironmentFirst(t *testing.T) {
	fr, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(routerBody)) })
	h := fixedSearchOps(root)
	bridge, _ := encodeTraceBridge(&traceEnvelope{V: traceVersion, Harness: "claude-code", SessionID: "abc", TurnID: "t", CallID: "c",
		History: []traceHistory{{Role: "assistant", Text: "looking"}}})
	runSearch(t, h, map[string]string{"TOKENDROP_API_KEY": "k", bridgeEnv: bridge}, "-config", cfg, "q")
	_, sent := fr.last(t)
	var m map[string]any
	_ = json.Unmarshal(sent, &m)
	tr := m["trace"].(map[string]any)
	if tr["session_id"] != "abc" || tr["turn_id"] != "t" || tr["harness"] != "claude-code" {
		t.Errorf("bridge not used: %v", tr)
	}
	if hist, _ := tr["history"].([]any); len(hist) != 1 {
		t.Errorf("history dropped: %v", tr)
	}
}

func TestSearchFallsBackToTheWorkspaceLineageFileAndBumpsSeq(t *testing.T) {
	fr, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(routerBody)) })
	h := fixedSearchOps(filepath.Join(root, "src"))
	sessions := filepath.Join(root, "sessions")
	path := lineagePath(sessions, root)
	_ = saveLineage(h.ops.hook, path, &lineageFile{Harness: "cursor", SessionID: "conv", TurnID: "gen", Window: "none", Seq: 3,
		History: []traceHistory{{Role: "assistant", Text: "Let me check."}}}, h.ops.now())

	runSearch(t, h, map[string]string{"TOKENDROP_API_KEY": "k"}, "-config", cfg, "q")
	_, sent := fr.last(t)
	var m map[string]any
	_ = json.Unmarshal(sent, &m)
	tr := m["trace"].(map[string]any)
	if tr["harness"] != "cursor" || tr["session_id"] != "conv" || tr["turn_id"] != "gen" || tr["seq"] != float64(4) {
		t.Errorf("lineage file not used from a subdirectory: %v", tr)
	}
	if l, _ := loadLineage(h.ops.hook, path); l.Seq != 4 {
		t.Errorf("seq not persisted: %+v", l)
	}

	// TOKENDROP_LINEAGE names the file directly, wherever the cwd is.
	other := lineagePath(sessions, "/elsewhere")
	_ = saveLineage(h.ops.hook, other, &lineageFile{Harness: "cursor", SessionID: "named", Window: "2"}, h.ops.now())
	runSearch(t, h, map[string]string{"TOKENDROP_API_KEY": "k", lineageEnv: other}, "-config", cfg, "q")
	_, sent = fr.last(t)
	_ = json.Unmarshal(sent, &m)
	if tr := m["trace"].(map[string]any); tr["session_id"] != "named" || tr["window"] != "2" {
		t.Errorf("named lineage not used: %v", tr)
	}
}

func TestSearchKillSwitchSendsNoTraceAndStillMines(t *testing.T) {
	fr, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "01a03e86-quiet")
		_, _ = w.Write([]byte(routerBody))
	})
	h := fixedSearchOps(root)
	runSearch(t, h, map[string]string{"TOKENDROP_API_KEY": "k", "TOKENDROP_TRACE": "off"}, "-config", cfg, "q")
	_, sent := fr.last(t)
	if bytes.Contains(sent, []byte(`"trace"`)) {
		t.Errorf("trace sent despite the kill switch: %s", sent)
	}
	if recs, _, _ := readIntake(filepath.Join(root, "intake")); len(recs) != 1 {
		t.Error("the search was not recorded for mining")
	}
}

func TestSearchRetriesOnceBareWhenTheRouterRejectsTheTrace(t *testing.T) {
	calls := 0
	fr, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		_ = body
		if calls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"unknown field trace"}`))
			return
		}
		_, _ = w.Write([]byte(routerBody))
	})
	h := fixedSearchOps(root)
	code, out, errOut := runSearch(t, h, map[string]string{"TOKENDROP_API_KEY": "k"}, "-config", cfg, "q")
	if code != exitOK || out != routerBody || !strings.Contains(errOut, "retrying without it") {
		t.Fatalf("exit %d out %q err %q", code, out, errOut)
	}
	if calls != 2 {
		t.Errorf("calls: %d", calls)
	}
	_, sent := fr.last(t)
	if bytes.Contains(sent, []byte(`"trace"`)) {
		t.Error("the retry still carried the trace")
	}
}

func TestSearchModelFormatIsCompactAndChosenFirst(t *testing.T) {
	_, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(routerBody)) })
	h := fixedSearchOps(root)
	code, out, _ := runSearch(t, h, map[string]string{"TOKENDROP_API_KEY": "k"}, "-config", cfg, "-format", "model", "q")
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	want := []string{"search 01a03e86-fictional", "session sess-9", "[fictional] (chosen) answer", "answer: Ports number the endpoints.", "1. https://b.test/2 — Two", "second snippet", "[slow] retrieval"}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
	if strings.Index(out, "[fictional]") > strings.Index(out, "[slow]") {
		t.Error("the chosen candidate is not first")
	}
	if strings.Contains(out, `"candidates"`) {
		t.Error("model format leaked raw JSON")
	}
}

func TestSearchMapsRouterErrorsToExitCodesAndRecordsNothing(t *testing.T) {
	for _, tc := range []struct {
		status int
		exit   int
	}{{401, exitClientErr}, {429, exitClientErr}, {503, exitServerErr}} {
		_, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"error":"fictional"}`))
		})
		h := fixedSearchOps(root)
		code, out, _ := runSearch(t, h, map[string]string{"TOKENDROP_API_KEY": "k"}, "-config", cfg, "-format", "model", "q")
		if code != tc.exit || out != `{"error":"fictional"}` {
			t.Errorf("HTTP %d: exit %d out %q", tc.status, code, out)
		}
		if recs, _, _ := readIntake(filepath.Join(root, "intake")); len(recs) != 0 {
			t.Errorf("HTTP %d: a failed search was recorded for mining", tc.status)
		}
		if len(h.flushes) != 0 {
			t.Errorf("HTTP %d: a flush was started for nothing", tc.status)
		}
	}
}

func TestSearchUsageErrors(t *testing.T) {
	_, cfg, root := newFakeRouter(t, nil)
	h := fixedSearchOps(root)
	if code, _, _ := runSearch(t, h, nil, "-config", cfg); code != exitUsage {
		t.Errorf("no query: exit %d", code)
	}
	if code, _, _ := runSearch(t, h, nil, "-config", cfg, "-format", "xml", "q"); code != exitUsage {
		t.Errorf("bad format: exit %d", code)
	}
}
