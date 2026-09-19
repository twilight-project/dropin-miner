package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/internal/selfupdate"
)

// ── a local release, served through the real HTTPSource ─────────────────

// localRelease publishes releases at an httptest server; the client's
// transport sends the compiled-in GitHub URLs there, so discovery, bounds,
// checksum verification and archive inspection all run for real.
type localRelease struct {
	t        *testing.T
	latest   string
	bodies   map[string]string // version -> the executable's content
	mu       sync.Mutex
	requests []string
	deadline []time.Time
	srv      *httptest.Server
}

func newLocalRelease(t *testing.T, latest string, bodies map[string]string) *localRelease {
	t.Helper()
	lr := &localRelease{t: t, latest: latest, bodies: bodies}
	lr.srv = httptest.NewServer(http.HandlerFunc(lr.serve))
	t.Cleanup(lr.srv.Close)
	return lr
}

func (lr *localRelease) archive(version string) ([]byte, string) {
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	body := lr.bodies[version]
	_ = tw.WriteHeader(&tar.Header{Name: "dropin-miner", Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte(body))
	_ = tw.Close()
	_ = gz.Close()
	return out.Bytes(), fmt.Sprintf("dropin-miner_%s_linux_amd64.tar.gz", version)
}

func (lr *localRelease) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/releases/latest"), strings.Contains(path, "/releases/tags/v"):
		version := lr.latest
		if i := strings.Index(path, "/tags/v"); i >= 0 {
			version = path[i+len("/tags/v"):]
		}
		if _, ok := lr.bodies[version]; !ok {
			http.NotFound(w, r)
			return
		}
		archive, name := lr.archive(version)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v" + version,
			"assets": []map[string]any{
				{"name": name, "size": len(archive)},
				{"name": "checksums.txt", "size": 200},
			},
		})
	case strings.Contains(path, "/releases/download/v"):
		rest := path[strings.Index(path, "/releases/download/v")+len("/releases/download/v"):]
		version, asset, _ := strings.Cut(rest, "/")
		archive, name := lr.archive(version)
		switch asset {
		case name:
			_, _ = w.Write(archive)
		case "checksums.txt":
			fmt.Fprintf(w, "%x  %s\n", sha256.Sum256(archive), name)
		default:
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

func (lr *localRelease) source() selfupdate.ReleaseSource {
	target, _ := url.Parse(lr.srv.URL)
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
			lr.mu.Lock()
			lr.requests = append(lr.requests, r.URL.String())
			if dl, ok := r.Context().Deadline(); ok {
				lr.deadline = append(lr.deadline, dl)
			}
			lr.mu.Unlock()
			if r.URL.Scheme != "https" || (r.URL.Host != "api.github.com" && r.URL.Host != "github.com") {
				lr.t.Errorf("the updater requested %s, outside the compiled-in origin", r.URL)
			}
			out := r.Clone(r.Context())
			out.URL.Scheme, out.URL.Host, out.Host = target.Scheme, target.Host, target.Host
			return http.DefaultTransport.RoundTrip(out)
		}),
	}
	return selfupdate.NewHTTPSource(client)
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// contentRunner reports as its version the text of the file it runs, and
// remembers each run's deadline.
type contentRunner struct {
	mu       sync.Mutex
	deadline []time.Time
	failFor  string // a path whose run fails
}

func (c *contentRunner) Run(ctx context.Context, path string, _, _ []string) ([]byte, []byte, error) {
	c.mu.Lock()
	if dl, ok := ctx.Deadline(); ok {
		c.deadline = append(c.deadline, dl)
	}
	c.mu.Unlock()
	if c.failFor != "" && path == c.failFor {
		return nil, nil, errors.New("injected: the installed binary does not run")
	}
	b, err := os.ReadFile(path) // #nosec G304 -- test fixture
	if err != nil {
		return nil, nil, err
	}
	return []byte("dropin-miner " + strings.TrimSpace(string(b)) + "\n"), nil, nil
}

type upgradeFixture struct {
	t           *testing.T
	home        string
	exe         string
	lr          *localRelease
	runner      *contentRunner
	env         map[string]string
	build       string
	sourcesMade int
	timeout     time.Duration
	// runnerOverride, when set, replaces runner as what the command runs
	// processes with: the re-render cases need one that tells a version
	// check from an `agents install`.
	runnerOverride selfupdate.CommandRunner
	// agents is where the re-render looks for host files: a user home inside
	// this fixture's own root, with nothing on PATH, so no case can read or
	// write the machine's real agent configuration.
	agents agentOps
}

func newUpgradeFixture(t *testing.T, lr *localRelease) *upgradeFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &upgradeFixture{t: t, home: filepath.Join(root, "custom-home"), lr: lr, runner: &contentRunner{}, env: map[string]string{}, build: "0.3.0", timeout: selfupdate.OperationTimeout}
	f.exe = filepath.Join(f.home, "bin", "dropin-miner")
	f.agents = realAgentOps()
	f.agents.home = root
	f.agents.lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	f.agents.executable = func() (string, error) { return f.exe, nil }
	f.agents.isTerminal = func() bool { return false }
	writeFileT(t, filepath.Join(f.home, setupConfigFile), "")
	f.write(f.exe, "0.3.0")
	return f
}

func (f *upgradeFixture) write(path, content string) {
	f.t.Helper()
	writeFileT(f.t, path, content+"\n")
	if err := os.Chmod(path, 0o700); err != nil { // #nosec G302 -- an executable fixture
		f.t.Fatal(err)
	}
}

func (f *upgradeFixture) content(path string) string {
	b, err := os.ReadFile(path) // #nosec G304 -- test fixture
	if err != nil {
		return "<absent>"
	}
	return strings.TrimSpace(string(b))
}

func (f *upgradeFixture) run(args ...string) (int, string, string) {
	f.t.Helper()
	var out, errOut bytes.Buffer
	var runner selfupdate.CommandRunner = f.runner
	if f.runnerOverride != nil {
		runner = f.runnerOverride
	}
	d := upgradeDeps{
		stdout: &out, stderr: &errOut, getenv: envOf(f.env), userHome: filepath.Dir(f.home),
		executable: func() (string, error) { return f.exe, nil },
		build:      func() string { return f.build },
		source: func() selfupdate.ReleaseSource {
			f.sourcesMade++
			return f.lr.source()
		},
		runner: runner, goos: "linux", goarch: "amd64", operationTimeout: f.timeout,
		agents: f.agents, environ: func() []string { return nil },
	}
	code := upgradeMain(d, append([]string{"-home", f.home}, args...))
	return code, out.String(), errOut.String()
}

func (f *upgradeFixture) noStagingLeftovers() {
	f.t.Helper()
	left, err := selfupdate.StagingLeftovers(filepath.Dir(f.exe))
	if err != nil || len(left) != 0 {
		f.t.Errorf("staging material left beside the binary: %v %v", left, err)
	}
}

// ── the command ─────────────────────────────────────────────────────────

func TestUpgradeCommandInstallsAVerifiedRelease(t *testing.T) {
	f := newUpgradeFixture(t, newLocalRelease(t, "0.3.1", map[string]string{"0.3.1": "0.3.1\n"}))
	code, out, errOut := f.run()
	if code != exitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if f.content(f.exe) != "0.3.1" || f.content(selfupdate.PreviousPath(f.exe)) != "0.3.0" {
		t.Errorf("installed %s, previous %s", f.content(f.exe), f.content(selfupdate.PreviousPath(f.exe)))
	}
	if !strings.Contains(out, "upgraded "+f.exe+" from 0.3.0 to 0.3.1") || !strings.Contains(out, "upgrade -rollback") {
		t.Errorf("success output:\n%s", out)
	}
	f.noStagingLeftovers()
}

// One operation deadline, created before discovery, reaches every stage:
// release discovery and downloads carry it exactly, and the candidate's own
// five-second bound is taken inside it — equal to it when it is the shorter.
func TestUpgradeCarriesOneOperationDeadlineThroughEveryStage(t *testing.T) {
	// The operation is shorter than the candidate bound: every stage must
	// see exactly the operation's deadline.
	f := newUpgradeFixture(t, newLocalRelease(t, "0.3.1", map[string]string{"0.3.1": "0.3.1\n"}))
	f.timeout = 3 * time.Second
	if code, out, errOut := f.run(); code != exitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if len(f.lr.deadline) < 3 || len(f.runner.deadline) < 2 {
		t.Fatalf("stages seen: %d requests, %d runs; want discovery, both downloads, and both validations", len(f.lr.deadline), len(f.runner.deadline))
	}
	op := f.lr.deadline[0]
	for i, dl := range append(append([]time.Time{}, f.lr.deadline...), f.runner.deadline...) {
		if !dl.Equal(op) {
			t.Errorf("stage %d ran under deadline %s, not the operation's %s", i, dl, op)
		}
	}

	// The operation is the frozen three minutes: requests carry it, and each
	// validation is bounded by its own five seconds inside it.
	g := newUpgradeFixture(t, newLocalRelease(t, "0.3.1", map[string]string{"0.3.1": "0.3.1\n"}))
	start := time.Now()
	if code, out, errOut := g.run(); code != exitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	op = g.lr.deadline[0]
	if op.Sub(start) < selfupdate.OperationTimeout || op.Sub(start) > selfupdate.OperationTimeout+time.Minute {
		t.Errorf("the operation deadline is %s after start, want %s", op.Sub(start), selfupdate.OperationTimeout)
	}
	for _, dl := range g.lr.deadline {
		if !dl.Equal(op) {
			t.Errorf("a request ran under %s, not the operation's %s", dl, op)
		}
	}
	for _, dl := range g.runner.deadline {
		if !dl.Before(op) || dl.Sub(start) > selfupdate.CandidateTimeout+time.Second {
			t.Errorf("a validation ran under %s; want its own %s bound inside the operation", dl.Sub(start), selfupdate.CandidateTimeout)
		}
	}

	// Rollback runs under the same single deadline.
	h := newUpgradeFixture(t, newLocalRelease(t, "0.3.1", nil))
	h.write(selfupdate.PreviousPath(h.exe), "0.2.9")
	h.timeout = 3 * time.Second
	if code, out, errOut := h.run("-rollback"); code != exitOK {
		t.Fatalf("rollback exit %d\n%s\n%s", code, out, errOut)
	}
	if len(h.runner.deadline) < 2 {
		t.Fatalf("rollback validations seen: %d", len(h.runner.deadline))
	}
	for _, dl := range h.runner.deadline[1:] {
		if !dl.Equal(h.runner.deadline[0]) {
			t.Errorf("a rollback stage ran under %s, not the operation's %s", dl, h.runner.deadline[0])
		}
	}
}

func TestUpgradeCommandRollbackNeedsNoNetwork(t *testing.T) {
	f := newUpgradeFixture(t, newLocalRelease(t, "0.3.1", map[string]string{"0.3.1": "0.3.1\n"}))
	f.write(selfupdate.PreviousPath(f.exe), "0.2.9")
	code, out, errOut := f.run("-rollback")
	if code != exitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if f.content(f.exe) != "0.2.9" || f.content(selfupdate.PreviousPath(f.exe)) != "0.3.0" {
		t.Errorf("rollback installed %s, previous %s", f.content(f.exe), f.content(selfupdate.PreviousPath(f.exe)))
	}
	if f.sourcesMade != 0 || len(f.lr.requests) != 0 {
		t.Errorf("rollback built %d release sources and made %d requests; it needs none", f.sourcesMade, len(f.lr.requests))
	}
	if !strings.Contains(out, "rolled back "+f.exe+" from 0.3.0 to 0.2.9") {
		t.Errorf("rollback output:\n%s", out)
	}
	f.noStagingLeftovers()
}

func TestUpgradeCommandExactVersionAndNoOp(t *testing.T) {
	lr := newLocalRelease(t, "0.3.0", map[string]string{"0.3.0": "0.3.0\n", "0.3.2": "0.3.2\n"})
	f := newUpgradeFixture(t, lr)
	code, out, _ := f.run()
	if code != exitOK || !strings.Contains(out, "already the latest release") || f.content(f.exe) != "0.3.0" || lexists(selfupdate.PreviousPath(f.exe)) {
		t.Errorf("latest equals current is a no-op: exit %d\n%s", code, out)
	}
	for _, r := range lr.requests {
		if strings.Contains(r, "/download/") {
			t.Errorf("a no-op downloaded %s", r)
		}
	}
	code, out, errOut := f.run("-version", "0.3.2")
	if code != exitOK || f.content(f.exe) != "0.3.2" {
		t.Errorf("-version selects exactly that release: exit %d\n%s\n%s", code, out, errOut)
	}
}

func TestUpgradeCommandRefusesADowngrade(t *testing.T) {
	lr := newLocalRelease(t, "0.3.0", map[string]string{"0.2.9": "0.2.9\n"})
	f := newUpgradeFixture(t, lr)
	code, out, errOut := f.run("-version", "0.2.9")
	if code != upgradeRefused.exit() || !strings.HasPrefix(errOut, "dropin-miner upgrade: refused: ") || f.content(f.exe) != "0.3.0" {
		t.Errorf("a lower -version is refused: exit %d\n%s\n%s", code, out, errOut)
	}
	for _, r := range lr.requests {
		if strings.Contains(r, "/download/") {
			t.Errorf("a refused downgrade downloaded %s", r)
		}
	}
}

func TestUpgradeCommandSendsNpmCopiesToNpmBeforeAnythingElse(t *testing.T) {
	f := newUpgradeFixture(t, newLocalRelease(t, "0.3.1", map[string]string{"0.3.1": "0.3.1\n"}))
	f.env["DROPIN_MINER_LAUNCH"] = "npm:global"
	code, out, errOut := f.run()
	if code != exitUsage || !strings.HasPrefix(errOut, "dropin-miner upgrade: ownership: ") || !strings.Contains(errOut, "npm install -g dropin-miner@latest") {
		t.Errorf("an npm copy gets npm's command: exit %d\n%s\n%s", code, out, errOut)
	}
	if len(f.lr.requests) != 0 || f.content(f.exe) != "0.3.0" || lexists(lifecycleGatePath(f.home)) {
		t.Error("an npm copy is refused before any request, replacement or lock")
	}
}

func TestUpgradeCommandRefusesADevBuildBeforeAnything(t *testing.T) {
	f := newUpgradeFixture(t, newLocalRelease(t, "0.3.1", map[string]string{"0.3.1": "0.3.1\n"}))
	f.build = "dev (0123456789ab+dirty)"
	code, _, errOut := f.run()
	if code != exitUsage || !strings.HasPrefix(errOut, "dropin-miner upgrade: ownership: ") {
		t.Errorf("a dev build: exit %d, %s", code, errOut)
	}
	if len(f.lr.requests) != 0 || lexists(lifecycleGatePath(f.home)) {
		t.Error("a dev build is refused before any request and before any lock file")
	}
}

func TestUpgradeCommandRefusesUnderAHeldLifecycleGateOfItsCustomHome(t *testing.T) {
	f := newUpgradeFixture(t, newLocalRelease(t, "0.3.1", map[string]string{"0.3.1": "0.3.1\n"}))
	mustAcquireGate(t, lifecycleGatePath(f.home))
	setForegroundWait(t, 50*time.Millisecond)
	code, _, errOut := f.run()
	if code != exitTransport || !strings.HasPrefix(errOut, "dropin-miner upgrade: lifecycle_busy: ") {
		t.Errorf("a held gate: exit %d, %s", code, errOut)
	}
	if len(f.lr.requests) != 0 || f.content(f.exe) != "0.3.0" {
		t.Error("lifecycle contention is refused before any request or replacement")
	}
}

func TestUpgradeCommandFlagCombinations(t *testing.T) {
	f := newUpgradeFixture(t, newLocalRelease(t, "0.3.1", map[string]string{"0.3.1": "0.3.1\n"}))
	for name, args := range map[string][]string{
		"both":         {"-version", "0.3.1", "-rollback"},
		"bad version":  {"-version", "v0.3"},
		"prerelease":   {"-version", "0.3.1-rc.1"},
		"extra":        {"now"},
		"unknown flag": {"-force"},
	} {
		if code, _, errOut := f.run(args...); code != exitUsage || !strings.Contains(errOut, "usage: dropin-miner upgrade") {
			t.Errorf("%s: want usage exit, got %d: %s", name, code, errOut)
		}
	}
	if len(f.lr.requests) != 0 || f.content(f.exe) != "0.3.0" {
		t.Error("an invalid command changes and requests nothing")
	}
	var out bytes.Buffer
	if code := upgradeMain(upgradeDeps{stdout: &out, stderr: &out}, []string{"-h"}); code != exitOK || !strings.Contains(out.String(), "-rollback") {
		t.Errorf("-h prints the usage: %d %s", code, out.String())
	}
}

// A replacement that fails prints no success: the failure's class, the old
// binary restored, and nothing staged left behind.
func TestUpgradeCommandPrintsSuccessOnlyAfterTheCanonicalPathValidates(t *testing.T) {
	f := newUpgradeFixture(t, newLocalRelease(t, "0.3.1", map[string]string{"0.3.1": "0.3.1\n"}))
	f.runner.failFor = f.exe // the candidate validates; the installed canonical path does not
	code, out, errOut := f.run()
	if strings.Contains(out, "upgraded") {
		t.Errorf("success was printed for a replacement that failed:\n%s", out)
	}
	if code != exitTransport || !strings.HasPrefix(errOut, "dropin-miner upgrade: retry: ") {
		t.Errorf("a restored replacement failure is a safe retry: exit %d\n%s", code, errOut)
	}
	if f.content(f.exe) != "0.3.0" || lexists(selfupdate.PreviousPath(f.exe)) {
		t.Errorf("the old binary is restored and no .previous committed: installed %s", f.content(f.exe))
	}
	f.noStagingLeftovers()
}

func TestUpgradeFailureClassesAreDistinctAndTyped(t *testing.T) {
	cases := map[upgradeClass][]error{
		upgradeRetry: {
			&selfupdate.Error{Kind: selfupdate.KindUnavailable, Err: errors.New("x")},
			&selfupdate.Error{Kind: selfupdate.KindReplacementFailed, Err: errors.New("x")},
			&selfupdate.Error{Kind: selfupdate.KindPreviousInUse, Err: errors.New("x")},
		},
		upgradeReleaseInvalid: {
			&selfupdate.Error{Kind: selfupdate.KindReleaseInvalid, Err: errors.New("x")},
			&selfupdate.Error{Kind: selfupdate.KindCandidateInvalid, Err: errors.New("x")},
			&selfupdate.Error{Kind: selfupdate.KindUnsupported, Err: errors.New("x")},
		},
		upgradeOwnership: {
			fmt.Errorf("%w: npm", errUpgradeOwnership),
			&selfupdate.Error{Kind: selfupdate.KindNotRelease, Err: errors.New("x")},
		},
		upgradeFilesystem: {
			&selfupdate.Error{Kind: selfupdate.KindStaging, Err: errors.New("x")},
			&selfupdate.Error{Kind: selfupdate.KindNoPrevious, Err: errors.New("x")},
			&selfupdate.Error{Kind: selfupdate.KindPreviousInvalid, Err: errors.New("x")},
		},
		upgradeLifecycleBusy: {
			errLifecycleBusy,
			&lifecycleActiveError{Operation: "setup", Lock: "x"},
		},
		upgradeRefused: {&selfupdate.Error{Kind: selfupdate.KindDowngrade, Err: errors.New("x")}},
		upgradeManual: {
			&selfupdate.Error{Kind: selfupdate.KindManualIntervention, Err: errors.New("x")},
			&selfupdate.Error{Kind: selfupdate.KindIncomplete, Err: errors.New("x")},
		},
	}
	tokens := map[upgradeClass]bool{}
	for class, errs := range cases {
		tokens[class] = true
		for _, err := range errs {
			// Wrapped, and with a message that names another class: only the
			// type decides.
			wrapped := fmt.Errorf("retry release_invalid ownership: %w", err)
			if got := classifyUpgrade(wrapped); got != class {
				t.Errorf("%v: class %s, want %s", err, got, class)
			}
		}
	}
	if len(tokens) != 7 {
		t.Errorf("want seven distinct classes, have %d", len(tokens))
	}
	if upgradeManual.exit() == upgradeRetry.exit() || upgradeRetry.exit() == exitOK || upgradeManual.exit() != exitOutcomeUnknown {
		t.Error("manual intervention must not share retry's exit code")
	}
}

// Cleanup after an attempted Install goes only through the package's
// preservation rule.
func TestUpgradeCommandCleansUpOnlyThroughDiscardAfterInstall(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "upgrade.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	afterInstall := 0
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			switch sel.Sel.Name {
			case "DiscardAfterInstall":
				afterInstall++
			case "Discard", "Remove", "RemoveAll":
				t.Errorf("upgrade.go calls %s: staging material after Install is cleaned only by Prepared.DiscardAfterInstall", sel.Sel.Name)
			}
		}
		return true
	})
	if afterInstall != 1 {
		t.Errorf("upgrade.go must call Prepared.DiscardAfterInstall exactly once, after Install; found %d", afterInstall)
	}
}

// ── the top-level surfaces ──────────────────────────────────────────────

func TestTopLevelHelpNamesTheLifecycleCommands(t *testing.T) {
	for _, command := range []string{"\n  setup ", "\n  uninstall ", "\n  upgrade "} {
		if !strings.Contains(usageText, command) {
			t.Errorf("top-level help must describe%s", command)
		}
	}
	for _, want := range []string{"-purge-state", "-yes never answers it", "-rollback", "npm install -g dropin-miner@latest"} {
		if !strings.Contains(usageText, want) {
			t.Errorf("top-level help must say %q", want)
		}
	}
}

// The purge confirmation cannot be reached through the real command wiring
// without a terminal, even with the right text on stdin and -yes.
func TestPurgeThroughTheCommandWiringNeedsATerminal(t *testing.T) {
	s := installed(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(walletFixtureAddress(t) + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	defer r.Close()
	var out, errOut bytes.Buffer
	code := cmdUninstall([]string{"-purge-state", "-yes", "-home", s.home}, r, &out, &errOut, s.getenv)
	if code != exitUsage || !lexists(filepath.Join(s.home, "wallet")) || !lexists(filepath.Join(s.home, "state")) {
		t.Errorf("a purge from a pipe must refuse and destroy nothing: exit %d\n%s\n%s", code, out.String(), errOut.String())
	}
}
