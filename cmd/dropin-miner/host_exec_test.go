package main

// The real-shell execution harness: a rendered string is not proven by
// reading it, only by running it in the shell that runs it.
//
// Every test here builds this tree's binary, installs it under a temporary
// home, renders a host's strings for that installation with the install's
// own renderers, and runs them through a real shell on the runner — bash and
// sh on Linux and macOS; on Windows, Windows PowerShell 5.1 and pwsh (always
// through -EncodedCommand, never as a -Command argument: 5.1 strips embedded
// double quotes from a native argument and would run something other than
// the rendered string), cmd.exe as a Node or Win32 host starts it, Git Bash at
// its standard path, and Hermes' own argument splitter. What decides a case
// is what arrives: the request an httptest router receives, the answer a hook
// prints, the file a hook writes. Nothing reaches a real host: the router and
// the platform are loopback stubs, a closed loopback proxy catches anything
// that would dial out, and the platform stub fails the test if it is called.
//
// H1 recorded v0.2.9's results here, failures included, under names that said
// so. H2 and H3 flipped those rows as they fixed them, and renamed them: a
// test that asserts the fix is not characterization any more. What is still
// named for a defect names the issue it guards (#66, #67, #68, #69).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"
)

// ── shells ───────────────────────────────────────────────────────────────

// execShell is one real way a host runs a string on this runner.
type execShell struct {
	name string    // bash, sh, git-bash, powershell, pwsh, cmd, hermes-split
	kind shellKind // the grammar it speaks
}

var (
	shellBash       = execShell{"bash", shellPOSIX}
	shellSh         = execShell{"sh", shellPOSIX}
	shellGitBash    = execShell{"git-bash", shellPOSIX}
	shellWinPS      = execShell{"powershell", shellPowerShell}
	shellPwsh       = execShell{"pwsh", shellPowerShell}
	shellCmdExe     = execShell{"cmd", shellCmd}
	shellHermesArgv = execShell{"hermes-split", shellArgv}
)

// gitBashPath is Git for Windows' standard location. A bash found on PATH is
// not used: on Windows that is often System32's WSL launcher, which is the
// very shell the soak's Codex fell into.
const gitBashPath = `C:\Program Files\Git\bin\bash.exe`

// execShellsFor are the real shells that speak kind on this runner's OS. A
// POSIX string runs under bash (tool calls) or sh (Claude Code's hooks) on
// Linux and macOS, and under Git Bash on Windows; a PowerShell string under
// both editions.
func execShellsFor(kind shellKind, hookRunner bool) []execShell {
	switch kind {
	case shellPOSIX:
		if runtime.GOOS == "windows" {
			return []execShell{shellGitBash}
		}
		if hookRunner {
			return []execShell{shellSh}
		}
		return []execShell{shellBash}
	case shellPowerShell:
		if runtime.GOOS == "windows" {
			return []execShell{shellWinPS, shellPwsh}
		}
	case shellCmd:
		if runtime.GOOS == "windows" {
			return []execShell{shellCmdExe}
		}
	case shellArgv:
		return []execShell{shellHermesArgv}
	}
	return nil
}

// requireExecTool resolves one program a shell needs. Locally a missing one
// skips with the reason; under CI=true it fails, because CI runs go test
// without -v and a skip there reads as a pass.
func requireExecTool(t *testing.T, sh execShell) string {
	t.Helper()
	var candidates []string
	switch sh {
	case shellBash:
		candidates = []string{"bash"}
	case shellSh:
		candidates = []string{"sh"}
	case shellGitBash:
		if _, err := os.Stat(gitBashPath); err == nil {
			return gitBashPath
		}
	case shellWinPS:
		candidates = []string{"powershell.exe"}
	case shellPwsh:
		candidates = []string{"pwsh.exe", "pwsh"}
	case shellCmdExe:
		candidates = []string{"cmd.exe"}
	case shellHermesArgv:
		candidates = []string{"python3", "python"}
		if runtime.GOOS == "windows" {
			// python3.exe on a Windows PATH is often the Store's installer
			// stub, which exits without running anything.
			candidates = []string{"python", "python3"}
		}
	}
	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	reason := fmt.Sprintf("the %s shell is not available on this runner", sh.name)
	if os.Getenv("CI") == "true" {
		t.Fatalf("%s, and CI does not let an execution test skip", reason)
	}
	t.Skip(reason)
	return ""
}

// execOutcome is what running one string produced.
type execOutcome struct {
	exit           int
	stdout, stderr string
}

func (o execOutcome) String() string {
	return fmt.Sprintf("exit %d\nstdout:\n%s\nstderr:\n%s", o.exit, o.stdout, o.stderr)
}

// hermesSplitScript is Hermes' own hook spawn, reduced to what decides
// whether a hook command runs: split_command_line from
// hermes_cli/_subprocess_compat.py and the Popen from agent/shell_hooks.py
// _spawn (NousResearch/hermes-agent@5d59366010640c1d6b8f170d8a4ee109db2bbdef),
// with IS_WINDOWS taken from the interpreter's own platform.
const hermesSplitScript = `
import os, shlex, subprocess, sys
IS_WINDOWS = os.name == "nt"
def split_command_line(line):
    if not IS_WINDOWS:
        return shlex.split(line)
    out = []
    for tok in shlex.split(line, posix=False):
        if len(tok) >= 2 and tok[0] == tok[-1] and tok[0] in ("'", '"'):
            tok = tok[1:-1]
        out.append(tok)
    return out
command = open(sys.argv[1], encoding="utf-8").read()
stdin_json = sys.stdin.buffer.read().decode("utf-8")
argv = split_command_line(os.path.expanduser(command))
proc = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                        text=True, encoding="utf-8", errors="replace", shell=False)
out, err = proc.communicate(stdin_json, timeout=60)
sys.stdout.write(out)
sys.stderr.write(err)
sys.exit(proc.returncode)
`

// runInShell runs script through sh, exactly as that shell would receive it
// from a host, with stdin and env, and reports what happened.
func runInShell(t *testing.T, sh execShell, script string, stdin []byte, env []string) execOutcome {
	t.Helper()
	program := requireExecTool(t, sh)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	switch sh {
	case shellBash, shellSh:
		// POSIX argv is exact: -c receives the rendered bytes as they are.
		cmd = exec.CommandContext(ctx, program, "-c", script) // #nosec G204 -- a test shell running a string this test rendered
	case shellGitBash:
		// A Windows command line is re-parsed by the MSYS runtime with its own
		// quoting rules, so the script is handed over as a file instead: bash
		// reads the rendered bytes from it unchanged.
		file := filepath.Join(t.TempDir(), "rendered.sh")
		if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd = exec.CommandContext(ctx, program, filepath.ToSlash(file)) // #nosec G204 -- Git Bash running a file this test wrote
	case shellWinPS, shellPwsh:
		cmd = exec.CommandContext(ctx, program, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-EncodedCommand", encodePowerShellCommand(script)) // #nosec G204 -- PowerShell running a string this test rendered
	case shellCmdExe:
		cmd = cmdShellCommand(ctx, program, script)
	case shellHermesArgv:
		file := filepath.Join(t.TempDir(), "hook-command.txt")
		if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd = exec.CommandContext(ctx, program, "-c", hermesSplitScript, file) // #nosec G204 -- a fixed test script splitting a string this test rendered
	default:
		t.Fatalf("no runner for %s", sh.name)
	}
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	out := execOutcome{stdout: stdout.String(), stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		out.exit = exitErr.ExitCode()
	default:
		t.Fatalf("%s did not run: %v\n%s", sh.name, err, out)
	}
	return out
}

// encodePowerShellCommand is -EncodedCommand's argument: the script as
// UTF-16LE, base64. It is the one way to hand either PowerShell edition a
// string it will parse exactly as written.
func encodePowerShellCommand(script string) string {
	units := utf16.Encode([]rune(script))
	b := make([]byte, 2*len(units))
	for i, u := range units {
		binary.LittleEndian.PutUint16(b[2*i:], u)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func TestEncodePowerShellCommandIsUTF16LEBase64(t *testing.T) {
	got, _ := base64.StdEncoding.DecodeString(encodePowerShellCommand("a'é😀"))
	want := []byte{'a', 0, '\'', 0, 0xe9, 0, 0x3d, 0xd8, 0x00, 0xde}
	if !bytes.Equal(got, want) {
		t.Fatalf("encoded % x, want % x", got, want)
	}
}

// ── the installation ─────────────────────────────────────────────────────

// execBinary is this tree's binary, built once per test process.
var execBinary struct {
	once sync.Once
	path string
	err  error
}

// execInstallation is one temporary installation: the binary under
// <home>/.tokendrop/bin, a config naming loopback stubs only, and the
// directories it writes to.
type execInstallation struct {
	root, bin, cfg, sessions string
	router                   *execRouter
	entry                    binEntry
	env                      []string
}

// execRouter records every search request the binary sends.
type execRouter struct {
	mu       sync.Mutex
	requests []execRouterRequest
}

type execRouterRequest struct {
	Query string          `json:"query"`
	Trace *traceEnvelope  `json:"trace"`
	Raw   json.RawMessage `json:"-"`
}

func (r *execRouter) received() []execRouterRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]execRouterRequest(nil), r.requests...)
}

func newExecInstallation(t *testing.T) *execInstallation {
	t.Helper()
	return newExecInstallationIn(t, "")
}

// newExecInstallationIn puts the installation under a directory of the given
// name, so a rendered command can be run against a path a participant might
// actually have rather than only against a tame one.
func newExecInstallationIn(t *testing.T, dirName string) *execInstallation {
	t.Helper()
	execBinary.once.Do(func() {
		dir, err := os.MkdirTemp("", "dropin-miner-exec-bin")
		if err != nil {
			execBinary.err = err
			return
		}
		execBinary.path = filepath.Join(dir, exeName("dropin-miner"))
		if out, err := exec.Command("go", "build", "-o", execBinary.path, ".").CombinedOutput(); err != nil { // #nosec G204 -- this test's own temp path
			execBinary.err = fmt.Errorf("build: %v\n%s", err, out)
		}
	})
	if execBinary.err != nil {
		t.Fatal(execBinary.err)
	}

	root := t.TempDir()
	if dirName != "" {
		root = filepath.Join(root, dirName)
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Skipf("this filesystem will not hold a directory named %q: %v", dirName, err)
		}
	}
	home := filepath.Join(root, installMarker)
	in := &execInstallation{
		root:     root,
		bin:      filepath.Join(home, "bin", exeName("dropin-miner")),
		cfg:      filepath.Join(home, "tokendrop.toml"),
		sessions: filepath.Join(home, "sessions"),
		router:   &execRouter{},
	}
	for _, d := range []string{filepath.Dir(in.bin), in.sessions, filepath.Join(home, "state"), filepath.Join(home, "intake"), filepath.Join(home, "spool")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	src, err := os.ReadFile(execBinary.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(in.bin, src, 0o700); err != nil { // #nosec G306 G703 -- the test's own executable copy, under its own temp directory
		t.Fatal(err)
	}

	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var req execRouterRequest
		_ = json.Unmarshal(body, &req)
		req.Raw = body
		in.router.mu.Lock()
		in.router.requests = append(in.router.requests, req)
		in.router.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"request_id":"req-exec","chosen":0,"candidates":[{"provider":"stub","kind":"search","status":"ok","answer":"stub answer"}]}`)
	}))
	t.Cleanup(router.Close)
	var platformCalls sync.Map
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		platformCalls.Store(r.Method+" "+r.URL.Path, true)
		http.Error(w, "the execution harness never talks to a platform", http.StatusTeapot)
	}))
	t.Cleanup(func() {
		platform.Close()
		platformCalls.Range(func(k, _ any) bool {
			t.Errorf("the installation called the platform stub: %v", k)
			return true
		})
	})

	doc := fmt.Sprintf(`[mining]
state_dir = %q
spool_dir = %q

[miner]
router_url = %q
intake_dir = %q
sessions_dir = %q

[platform]
base_url = %q
agents_api_url = %q
`, filepath.Join(home, "state"), filepath.Join(home, "spool"), router.URL,
		filepath.Join(home, "intake"), in.sessions, platform.URL, platform.URL)
	if err := os.WriteFile(in.cfg, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	in.entry = binEntry{command: in.bin, cfg: in.cfg}
	in.env = execEnv()
	return in
}

// execEnv is this process's environment without anything that could steer
// the binary from outside the test — no TOKENDROP_* variable, no host
// harness — plus a search key the stub router accepts and a closed loopback
// proxy, so a request meant for anywhere but the loopback stubs fails
// instead of leaving the machine.
func execEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		switch upper := strings.ToUpper(key); {
		case strings.HasPrefix(upper, "TOKENDROP_"),
			upper == "HTTP_PROXY", upper == "HTTPS_PROXY", upper == "NO_PROXY", upper == "ALL_PROXY":
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"TOKENDROP_API_KEY=sr-execution-harness-0000000000000000", // #nosec G101 -- a synthetic key only the stub router sees
		"HTTP_PROXY=http://127.0.0.1:9",
		"HTTPS_PROXY=http://127.0.0.1:9",
		"NO_PROXY=127.0.0.1,localhost",
	)
}

// renderedSkill is the SKILL.md host's install writes for this installation.
func (in *execInstallation) renderedSkill(host string) string {
	return renderedSkillFor(host, in.entry, runtime.GOOS)
}

// ── the harness, controlled ──────────────────────────────────────────────

// knownGoodSearch is a search written by hand in sh's own grammar, with the
// request on stdin: the positive control for that shell.
func knownGoodSearch(sh execShell, in *execInstallation) (script string, stdin []byte) {
	request := []byte(`{"version":1,"query":"exact query text"}`)
	single := func(s, quote, escaped string) string { return quote + strings.ReplaceAll(s, quote, escaped) + quote }
	switch sh.kind {
	case shellPOSIX:
		return single(in.bin, "'", `'\''`) + " search -config " + single(in.cfg, "'", `'\''`) + " --stdin", request
	case shellPowerShell:
		return single(string(request), "'", "''") + " | & " + single(in.bin, "'", "''") + " search -config " + single(in.cfg, "'", "''") + " --stdin", nil
	case shellCmd:
		return `"` + in.bin + `" search -config "` + in.cfg + `" --stdin`, request
	case shellArgv:
		bin, _ := hermesQuoteArg(in.bin, runtime.GOOS == "windows")
		cfg, _ := hermesQuoteArg(in.cfg, runtime.GOOS == "windows")
		return bin + " search -config " + cfg + " --stdin", request
	}
	return "", nil
}

// TestExecHarnessRunsAKnownGoodSearchInEveryShell is what keeps every
// "does not run here" row below from being vacuous. Each v0.2.9 failure is
// asserted as nothing arriving and a non-zero exit — which a runner that
// never starts its shell, or starts it with the wrong input, would also
// produce. So every real shell this runner offers must first carry a search
// written by hand in its own grammar to the binary.
//
// Windows PowerShell 5.1 was the one shell where this control had to stop
// short of the router: a request piped from a literal inside the script
// arrives there behind a byte-order mark, which `search --stdin` refused.
// The binary now tolerates one leading mark, so every shell here carries a
// search the whole way again.
func TestExecHarnessRunsAKnownGoodSearchInEveryShell(t *testing.T) {
	shells := []execShell{shellBash, shellSh, shellHermesArgv}
	if runtime.GOOS == "windows" {
		shells = []execShell{shellGitBash, shellWinPS, shellPwsh, shellCmdExe, shellHermesArgv}
	}
	for _, sh := range shells {
		t.Run(sh.name, func(t *testing.T) {
			in := newExecInstallation(t)
			script, stdin := knownGoodSearch(sh, in)
			out := runInShell(t, sh, script, stdin, in.env)
			requireOneRequest(t, in, out, "exact query text")
		})
	}
}

// ── the bytes a shell delivers ───────────────────────────────────────────

const (
	hexDumpHelperEnv = "DROPIN_MINER_TEST_HEXDUMP_STDIN"
	hexDumpMarker    = "STDINHEX:"
)

// TestHexDumpStdinHelperProcess is not a test: it is the program the
// byte-delivery characterization runs inside each shell. It writes its own
// stdin back as hex, so what a shell delivers to a native program is
// evidence rather than inference. Inert unless its variable is set.
func TestHexDumpStdinHelperProcess(t *testing.T) {
	if os.Getenv(hexDumpHelperEnv) != "1" {
		t.Skip("the stdin hex-dump helper runs only when the byte-delivery characterization starts it")
	}
	b, _ := io.ReadAll(os.Stdin)
	fmt.Printf("%s%x\n", hexDumpMarker, b)
}

// hexDumpCommand renders, in sh's own grammar, the command that runs this
// test binary's hex-dump helper. Every argument is quoted: PowerShell
// handed an unquoted -test.count=1 to the helper as "-test".
func hexDumpCommand(t *testing.T, sh execShell) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"-test.run=^TestHexDumpStdinHelperProcess$", "-test.count=1"}
	switch sh.kind {
	case shellCmd:
		out := `"` + self + `"`
		for _, a := range args {
			out += ` "` + a + `"`
		}
		return out
	case shellPowerShell:
		out := "& '" + strings.ReplaceAll(self, "'", "''") + "'"
		for _, a := range args {
			out += " '" + strings.ReplaceAll(a, "'", "''") + "'"
		}
		return out
	default:
		out := "'" + strings.ReplaceAll(self, "'", `'\''`) + "'"
		for _, a := range args {
			out += " '" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		return out
	}
}

// stdinDelivery is how the request reaches the program: as the shell
// process's own standard input, or piped from a literal written inside the
// script, which is how a PowerShell host's command has to carry a request
// body (a heredoc's POSIX equivalent).
type stdinDelivery string

const (
	deliveryInherited stdinDelivery = "inherited"
	// deliveryLiteral is the rendered form: the encoding line the renderer
	// writes, then the here-string piped in.
	deliveryLiteral stdinDelivery = "literal"
	// deliveryBareLiteral is the same pipe WITHOUT that line, which is what
	// the form looked like before H2 and what makes the line necessary.
	deliveryBareLiteral stdinDelivery = "literal-without-the-encoding-line"
)

// stdinBytesDelivered runs the hex-dump helper through sh and returns the
// bytes that actually arrived on its standard input.
func stdinBytesDelivered(t *testing.T, sh execShell, delivery stdinDelivery, request string) []byte {
	t.Helper()
	script := hexDumpCommand(t, sh)
	var stdin []byte
	switch delivery {
	case deliveryInherited:
		stdin = []byte(request)
	case deliveryLiteral, deliveryBareLiteral:
		if sh.kind != shellPowerShell {
			t.Fatalf("a literal-piped request is only rendered for PowerShell, not %s", sh.name)
		}
		script = "@'\n" + request + "\n'@ | " + script
		if delivery == deliveryLiteral {
			script = psOutputEncodingLine + "\n" + script
		}
	}
	out := runInShell(t, sh, script, stdin, append(execEnv(), hexDumpHelperEnv+"=1"))
	for _, line := range strings.Split(out.stdout, "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, hexDumpMarker); ok {
			got, err := hex.DecodeString(after)
			if err != nil {
				t.Fatalf("%s %s: the helper's dump does not decode: %v", sh.name, delivery, err)
			}
			return got
		}
	}
	t.Fatalf("%s %s: the hex-dump helper did not run\n%s", sh.name, delivery, out)
	return nil
}

// ── v0.2.9, characterized ────────────────────────────────────────────────

// deliveredBytes is what each shell delivers to a native program's stdin,
// hex-dumped on the CI runners.
//
// Everything delivers the request byte for byte except one case: Windows
// PowerShell 5.1 piping a literal written inside the script. There the bytes
// arrive behind a UTF-8 byte-order mark, with CRLF after them. The mark is
// the reason the binary tolerates one (trimUTF8BOM), and it is recorded as
// observed rather than explained: 5.1 reports $OutputEncoding us-ascii with
// an EMPTY preamble, and the mark survives setting $OutputEncoding and
// [Console]::OutputEncoding both, so it is not that preamble.
//
// What the rendered form does fix is the content. Without its first line,
// 5.1 replaces every UTF-16 unit outside ASCII with "?" — café arrives as
// caf?, an emoji as two question marks — and no tolerance in the binary can
// recover a query the shell already changed.
func deliveredBytes(sh execShell, delivery stdinDelivery, request string) []byte {
	if delivery == deliveryInherited {
		return []byte(request)
	}
	if sh == shellWinPS {
		return append(append([]byte{0xef, 0xbb, 0xbf}, request...), '\r', '\n')
	}
	return append([]byte(request), '\r', '\n')
}

// mangledByASCIIEncoding is what 5.1 delivers for a literal pipe WITHOUT the
// encoding line the rendered form carries: one "?" per UTF-16 unit outside
// ASCII, so an astral character becomes two.
func mangledByASCIIEncoding(request string) []byte {
	out := []byte{0xef, 0xbb, 0xbf}
	for _, unit := range utf16.Encode([]rune(request)) {
		if unit > 0x7f {
			out = append(out, '?')
			continue
		}
		out = append(out, byte(unit))
	}
	return append(out, '\r', '\n')
}

// stdinPayloads are the payload kinds the rendered forms must carry.
func stdinPayloads() map[string]string {
	return map[string]string{
		"ascii":       "exact query text",
		"latin1":      "café naïve Ärger",
		"cjk":         "東京の天気",
		"emoji":       "weather 😀 today",
		"adversarial": `it's \"quoted\" a\\b '@ inline café 東京 😀`,
	}
}

// TestStdinBytesEachShellDelivers dumps the exact bytes each real shell
// hands a native program's standard input, for every payload kind. The
// PowerShell literal rows are the ones H2 changed: with the encoding line
// the rendered form carries, the request arrives intact on both editions
// (behind 5.1's mark), where before every non-ASCII character was lost.
func TestStdinBytesEachShellDelivers(t *testing.T) {
	type run struct {
		sh       execShell
		delivery stdinDelivery
	}
	runs := []run{{shellBash, deliveryInherited}, {shellSh, deliveryInherited}, {shellHermesArgv, deliveryInherited}}
	if runtime.GOOS == "windows" {
		runs = []run{
			{shellWinPS, deliveryInherited}, {shellWinPS, deliveryLiteral},
			{shellPwsh, deliveryInherited}, {shellPwsh, deliveryLiteral},
			{shellCmdExe, deliveryInherited}, {shellGitBash, deliveryInherited}, {shellHermesArgv, deliveryInherited},
		}
	}
	for name, query := range stdinPayloads() {
		request := `{"version":1,"query":"` + query + `"}`
		for _, r := range runs {
			t.Run(name+"/"+r.sh.name+"/"+string(r.delivery), func(t *testing.T) {
				got := stdinBytesDelivered(t, r.sh, r.delivery, request)
				if want := deliveredBytes(r.sh, r.delivery, request); !bytes.Equal(got, want) {
					t.Fatalf("%s delivered\n got %x\nwant %x", r.sh.name, got, want)
				}
			})
		}
	}
}

// TestWithoutTheEncodingLineWindowsPowerShellMangsTheQuery is why the first
// line of the rendered PowerShell form is not optional: take it away and
// every non-ASCII character in the query is replaced by a question mark
// before the binary ever sees it — a search that runs, reports success, and
// answers a different question.
func TestWithoutTheEncodingLineWindowsPowerShellMangsTheQuery(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell 5.1 runs on the Windows runners")
	}
	request := `{"version":1,"query":"café 東京 😀"}`
	got := stdinBytesDelivered(t, shellWinPS, deliveryBareLiteral, request)
	if want := mangledByASCIIEncoding(request); !bytes.Equal(got, want) {
		t.Fatalf("a literal pipe without the encoding line delivered\n got %x\nwant %x", got, want)
	}
}

// TestPowerShellRenderedSearchIsServed is the search that follows from those
// bytes: the form the skill renders reaches the binary and is served on both
// PowerShell editions. On 5.1 that needs both halves of the fix — the
// encoding line for the content, the binary's tolerance for the mark.
func TestPowerShellRenderedSearchIsServed(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell is the Windows runner's shell")
	}
	for _, sh := range []execShell{shellWinPS, shellPwsh} {
		t.Run(sh.name, func(t *testing.T) {
			in := newExecInstallation(t)
			_, script, err := searchBlockForShell(shellPowerShell, in.entry, `{"version":1,"query":"exact query text"}`)
			if err != nil {
				t.Fatal(err)
			}
			out := runInShell(t, sh, script, nil, in.env)
			requireOneRequest(t, in, out, "exact query text")
		})
	}
}

// TestTheQueryArrivesExactlyInEveryShell is the end-to-end proof: for every
// payload kind, the rendered search for each shell this runner offers, run
// in that shell, and the query the router receives compared with the one the
// request carried. This is what "the query is not in argv and not changed by
// the shell" means in practice.
func TestTheQueryArrivesExactlyInEveryShell(t *testing.T) {
	shells := []execShell{shellBash, shellSh}
	if runtime.GOOS == "windows" {
		shells = []execShell{shellGitBash, shellWinPS, shellPwsh}
	}
	for name, query := range stdinPayloads() {
		for _, sh := range shells {
			t.Run(name+"/"+sh.name, func(t *testing.T) {
				in := newExecInstallation(t)
				request, err := json.Marshal(map[string]any{"version": 1, "query": query})
				if err != nil {
					t.Fatal(err)
				}
				_, script, err := searchBlockForShell(sh.kind, in.entry, string(request))
				if err != nil {
					t.Fatal(err)
				}
				out := runInShell(t, sh, script, nil, in.env)
				requireOneRequest(t, in, out, query)
			})
		}
	}
}

// TestAdversarialPathsRunInEveryShell renders the search with an executable
// and a config whose directories carry everything a participant's home might
// — a space, an apostrophe, &, $, parentheses, and on Windows %, ^ and ! —
// and runs it. v0.2.9 quoted every path with Go's %q, which leaves $ and `
// live inside the double quotes it writes.
func TestAdversarialPathsRunInEveryShell(t *testing.T) {
	names := []string{"we ird & $tuff (x)'q"}
	if runtime.GOOS == "windows" {
		names = append(names, "pct % caret ^ bang !")
	}
	shells := []execShell{shellBash, shellSh}
	if runtime.GOOS == "windows" {
		shells = []execShell{shellGitBash, shellWinPS, shellPwsh}
	}
	for _, name := range names {
		for _, sh := range shells {
			t.Run(name+"/"+sh.name, func(t *testing.T) {
				in := newExecInstallationIn(t, name)
				_, script, err := searchBlockForShell(sh.kind, in.entry, `{"version":1,"query":"exact query text"}`)
				if err != nil {
					t.Fatal(err)
				}
				out := runInShell(t, sh, script, nil, in.env)
				requireOneRequest(t, in, out, "exact query text")
			})
		}
	}
}

// hostShellsOnThisOS are the real shells a host's cell names on this runner,
// and — for a cell nothing has established — the candidates its evidence
// names, so the unknown is characterized rather than skipped.
func hostShellsOnThisOS(t *testing.T, host string, ch shellChannel) []execShell {
	t.Helper()
	tg, ok := targetByID(installTargets, host)
	if !ok {
		t.Fatalf("no target %q", host)
	}
	kinds, err := declaredShells(tg, runtime.GOOS, ch)
	if err != nil {
		candidates, known := unknownCellCandidates[host+" "+runtime.GOOS+" "+string(ch)]
		if !known {
			t.Fatalf("%v, and no characterization candidates are named for it", err)
		}
		kinds = candidates
	}
	var out []execShell
	for _, k := range kinds {
		out = append(out, execShellsFor(k, ch == channelHook)...)
	}
	return out
}

// hookRunnerItIsRenderedFor answers, for a host's hook cell on this OS,
// which runner the installed command is actually written for, and the note
// the install plan prints when the cell names more runners than that one.
//
// Cursor on Windows is the case: the cell is ruled {cmd, powershell}, and
// NO single string runs under cmd, Windows PowerShell 5.1 and pwsh across
// the paths a participant may have — six candidate forms were measured on a
// Windows runner against four paths, and the best of them (cmd /c call
// "<exe>") stops at a path containing a $ or a %. So the command is the one
// form proven under cmd, and the plan says so.
//
// The tests below assert BOTH halves of that ruling: the command runs under
// the runner it is rendered for, and it does not run under the others. The
// second half is what the note describes; skipping it would let the note go
// stale, and asserting the first half alone would have the suite claim a
// coverage the ruling says does not exist.
func hookRunnerItIsRenderedFor(t *testing.T, host string, e binEntry) (shellKind, string) {
	t.Helper()
	tg, ok := targetByID(installTargets, host)
	if !ok {
		t.Fatalf("no target %q", host)
	}
	kinds, err := declaredShells(tg, runtime.GOOS, channelHook)
	if err != nil {
		candidates, known := unknownCellCandidates[host+" "+runtime.GOOS+" hook"]
		if !known {
			t.Fatalf("%v, and no characterization candidates are named for it", err)
		}
		kinds = candidates
	}
	_, note, err := e.hookCommandForRunners(kinds, "probe")
	if err != nil {
		t.Fatalf("rendering %s's hook command for %v: %v", host, kinds, err)
	}
	if note != "" {
		// hookCommandForRunners renders a multi-runner cell under cmd.
		return shellCmd, note
	}
	return kinds[0], ""
}

// unknownCellCandidates are the shells an unknown cell is characterized
// under: the ones its evidence points at without establishing. Cursor's
// Linux hook runner is unnamed, and macOS ran the POSIX form.
//
// Codex on Windows was here until a live run established it as powershell;
// its row now comes from the declaration like every other established cell,
// which is the point of the map being small.
var unknownCellCandidates = map[string][]shellKind{
	"cursor linux hook": {shellPOSIX},
}

func requireOneRequest(t *testing.T, in *execInstallation, out execOutcome, query string) execRouterRequest {
	t.Helper()
	got := in.router.received()
	if len(got) != 1 || got[0].Query != query {
		t.Fatalf("the router received %d request(s) %v, want exactly one with query %q\n%s", len(got), got, query, out)
	}
	return got[0]
}

// TestSkillCommandsRunInTheShellTheyAreRenderedFor runs the three commands
// every host's skill renders — the search, the preference command and the
// human form — in the shell each was rendered for, on this runner.
//
// This is #67's guard. v0.2.9 rendered one Bash form for every host and OS,
// so on Windows two of the three main hosts could not search at all; each of
// these rows was a failure then and runs now. A host whose shell is not
// established keeps the Bash form, and its row is
// TestUnknownToolCellKeepsTheBashForm below rather than this one.
func TestSkillCommandsRunInTheShellTheyAreRenderedFor(t *testing.T) {
	for _, host := range []string{"claude", "codex", "cursor", "pi", "hermes"} {
		for _, sh := range renderedShellsOnThisOS(t, host) {
			t.Run(host+"/"+sh.name, func(t *testing.T) {
				t.Run("search", func(t *testing.T) {
					in := newExecInstallation(t)
					block := skillBlockFor(t, in.renderedSkill(host), sh.kind, "search")
					out := runInShell(t, sh, block.body, nil, in.env)
					requireOneRequest(t, in, out, "exact query text")
				})
				t.Run("preference", func(t *testing.T) {
					in := newExecInstallation(t)
					block := skillBlockFor(t, in.renderedSkill(host), sh.kind, "prefer")
					script := strings.Replace(block.body, "<argument>", "status", 1)
					out := runInShell(t, sh, script, nil, in.env)
					if out.exit != 0 || !strings.Contains(out.stdout, "search default:") {
						t.Fatalf("the preference command did not run under %s\n%s", sh.name, out)
					}
				})
				t.Run("human form", func(t *testing.T) {
					in := newExecInstallation(t)
					block := skillBlockFor(t, in.renderedSkill(host), sh.kind, "human")
					script := strings.Replace(block.body, "<query>", "exact query text", 1)
					out := runInShell(t, sh, script, nil, in.env)
					requireOneRequest(t, in, out, "exact query text")
				})
			})
		}
	}
}

// renderedShellsOnThisOS are the shells a host's skill is rendered for on
// this runner: its declaration, or the POSIX fallback when nothing
// established it.
func renderedShellsOnThisOS(t *testing.T, host string) []execShell {
	t.Helper()
	tg, ok := targetByID(installTargets, host)
	if !ok {
		t.Fatalf("no target %q", host)
	}
	kinds, _ := toolShellsForSkill(tg, runtime.GOOS)
	var out []execShell
	for _, k := range kinds.kinds {
		out = append(out, execShellsFor(k, false)...)
	}
	return out
}

// TestUnknownToolCellKeepsTheBashForm is H-R5's fallback: a host × OS nobody
// has established keeps v0.2.9's Bash form and the install plan says which
// host and OS that applies to. Refusing to render would take the host away
// entirely, which is the regression H-R5 forbids.
//
// Its subject is a fixture, not a real host. It used to be Codex on Windows,
// the one declared cell still unknown, and that cell is now established — so
// the rule would have lost its only guard exactly when the last unknown cell
// was filled in, which is when a rule like this stops being exercised and
// starts being folklore. A fixture keeps it asserted whatever the real
// declaration says.
type unestablishedToolHost struct{ powerShellDeclaringHost }

func (unestablishedToolHost) ID() string    { return "fake-unestablished-host" }
func (unestablishedToolHost) Label() string { return "Fake Unestablished Host" }
func (unestablishedToolHost) Shells(string) hostShells {
	return hostShells{tool: cellUnknown, hook: cellNoChannel}
}

func TestUnknownToolCellKeepsTheBashForm(t *testing.T) {
	kinds, note := toolShellsForSkill(unestablishedToolHost{}, "windows")
	if len(kinds.kinds) != 1 || kinds.kinds[0] != shellPOSIX {
		t.Fatalf("an unknown cell renders for %v, want the POSIX fallback", kinds.kinds)
	}
	if !strings.Contains(note, "not established") || !strings.Contains(note, "Fake Unestablished Host") {
		t.Fatalf("the install plan says %q, which does not name the host and the reason", note)
	}
	// And the rule is not vacuous: an established cell renders for what it
	// declares and prints no note at all.
	codex, _ := targetByID(installTargets, "codex")
	kinds, note = toolShellsForSkill(codex, "windows")
	if len(kinds.kinds) != 1 || kinds.kinds[0] != shellPowerShell || note != "" {
		t.Fatalf("Codex on Windows renders for %v with note %q, want powershell and no note", kinds.kinds, note)
	}
}

// TestRulesLineCommandRunsInOpencodesShell runs the command opencode's
// AGENTS.md line renders, with the JSON request on stdin, in the shell
// opencode runs its bash tool in on this OS. v0.2.9 rendered a POSIX string
// for every OS, which PowerShell could not parse (#67).
func TestRulesLineCommandRunsInOpencodesShell(t *testing.T) {
	for _, sh := range renderedShellsOnThisOS(t, "opencode") {
		t.Run(sh.name, func(t *testing.T) {
			in := newExecInstallation(t)
			command, err := in.entry.stdinCommandForShell(sh.kind)
			if err != nil {
				t.Fatal(err)
			}
			out := runInShell(t, sh, command, []byte(`{"version":1,"query":"exact query text"}`), in.env)
			requireOneRequest(t, in, out, "exact query text")
		})
	}
}

// hookSpecFor is the hook entries this host's install writes on this runner.
func hookSpecFor(t *testing.T, host string, entry binEntry) hooksSpec {
	t.Helper()
	return goldenHookSpec(t, host, entry, runtime.GOOS)
}

// hookCase is one installed hook command, run with a real payload, and the
// observable that proves the binary ran it.
type hookCase struct {
	event   string
	payload func(in *execInstallation) any
	// proof reports whether the hook demonstrably ran, from its output and
	// the files it writes.
	proof func(t *testing.T, in *execInstallation, out execOutcome) bool
}

func lineageFileExists(in *execInstallation) bool {
	_, err := os.Stat(lineagePath(in.sessions, in.root))
	return err == nil
}

// cursorLineageFileExists: Cursor keys its file by conversation as well
// (#109), and every Cursor case here speaks for "exec-conversation".
func cursorLineageFileExists(in *execInstallation) bool {
	_, err := os.Stat(conversationLineagePath(in.sessions, in.root, "exec-conversation"))
	return err == nil
}

// The hook events that spawn a detached flush (Claude Code's SessionStart
// and Stop, Cursor's sessionStart and stop) are not run here: a flush that
// outlives its test holds files in the test's temporary directory. H3's hook
// execution test runs every installed hook.
var v029HookCases = map[string][]hookCase{
	"claude": {
		{
			event: "PreToolUse",
			payload: func(in *execInstallation) any {
				return map[string]any{"session_id": "exec-session", "tool_use_id": "exec-call", "tool_name": "Bash", "cwd": in.root,
					"tool_input": map[string]any{"command": in.entry.stdinCommand()}}
			},
			proof: func(t *testing.T, in *execInstallation, out execOutcome) bool {
				return out.exit == 0 && strings.Contains(out.stdout, `"updatedInput"`) && strings.Contains(out.stdout, bridgeEnv+"=") && lineageFileExists(in)
			},
		},
		{
			event:   "PreCompact",
			payload: func(*execInstallation) any { return map[string]any{"session_id": "exec-session"} },
			proof: func(t *testing.T, in *execInstallation, out execOutcome) bool {
				_, err := os.Stat(filepath.Join(in.sessions, hookStateFile))
				return out.exit == 0 && err == nil
			},
		},
		{
			event:   "PostCompact",
			payload: func(*execInstallation) any { return map[string]any{"session_id": "exec-session"} },
			proof: func(t *testing.T, in *execInstallation, out execOutcome) bool {
				_, err := os.Stat(filepath.Join(in.sessions, hookStateFile))
				return out.exit == 0 && err == nil
			},
		},
	},
	"cursor": {
		{
			event: "beforeShellExecution",
			payload: func(in *execInstallation) any {
				// What Cursor is about to run is what its own skill renders,
				// which from H2 is the only command this hook allows.
				shells, err := declaredShells(cursorTarget{}, runtime.GOOS, channelTool)
				if err != nil {
					shells = []shellKind{shellPOSIX}
				}
				_, search, err := searchBlockForShell(shells[0], in.entry, `{"version":1,"query":"exact query text"}`)
				if err != nil {
					search = in.entry.stdinCommand()
				}
				return map[string]any{"conversation_id": "exec-conversation", "generation_id": "exec-generation",
					"workspace_roots": []string{in.root}, "command": search}
			},
			proof: func(t *testing.T, in *execInstallation, out execOutcome) bool {
				return out.exit == 0 && strings.TrimSpace(out.stdout) == `{"permission":"allow"}` && cursorLineageFileExists(in)
			},
		},
		{
			event: "afterAgentThought",
			payload: func(in *execInstallation) any {
				return map[string]any{"conversation_id": "exec-conversation", "workspace_roots": []string{in.root}, "text": "thinking before a search"}
			},
			proof: func(t *testing.T, in *execInstallation, out execOutcome) bool {
				return out.exit == 0 && cursorLineageFileExists(in)
			},
		},
		{
			event: "afterAgentResponse",
			payload: func(in *execInstallation) any {
				return map[string]any{"conversation_id": "exec-conversation", "workspace_roots": []string{in.root}, "text": "a sentence before a search"}
			},
			proof: func(t *testing.T, in *execInstallation, out execOutcome) bool {
				return out.exit == 0 && cursorLineageFileExists(in)
			},
		},
		{
			event: "preCompact",
			payload: func(in *execInstallation) any {
				return map[string]any{"conversation_id": "exec-conversation", "workspace_roots": []string{in.root}}
			},
			proof: func(t *testing.T, in *execInstallation, out execOutcome) bool {
				return out.exit == 0 && cursorLineageFileExists(in)
			},
		},
	},
}

// TestInstalledHookCommandsRunInTheirRunner reads each hook command back from
// the file the install writes and runs it, with a real payload, through the
// runner that host's hook cell names on this OS.
//
// This is #69's guard. v0.2.9 wrote every hook command with a %q-quoted path,
// which cmd and a POSIX shell accept and PowerShell reads as an expression
// whose next word is a parse error — so on Windows no Cursor hook ever ran,
// nothing recorded the failure, and the binary never started.
func TestInstalledHookCommandsRunInTheirRunner(t *testing.T) {
	for _, host := range []string{"claude", "cursor"} {
		for _, sh := range hostShellsOnThisOS(t, host, channelHook) {
			for _, hc := range v029HookCases[host] {
				t.Run(host+"/"+sh.name+"/"+hc.event, func(t *testing.T) {
					in := newExecInstallation(t)
					spec := hookSpecFor(t, host, in.entry)
					command := ""
					for _, h := range installedHookCommands(t, installedHookFile(t, spec, in.entry), spec) {
						if h.event == hc.event {
							command = h.command
						}
					}
					if command == "" {
						t.Fatalf("no installed %s hook for %s", host, hc.event)
					}
					payload, _ := json.Marshal(hc.payload(in))
					out := runInShell(t, sh, command, payload, in.env)
					if rendered, note := hookRunnerItIsRenderedFor(t, host, in.entry); sh.kind != rendered {
						// A runner the cell names that no single form serves.
						// Assert the measured failure, so the day some form does
						// serve all of them this goes red and the plan's note,
						// the ruling and this branch are revisited together.
						if out.exit == 0 || hc.proof(t, in, out) {
							t.Fatalf("the %s hook now runs under %s as well; the install plan's note and the ruling behind it are stale\ncommand: %s\n%s",
								hc.event, sh.name, command, out)
						}
						t.Logf("as ruled, not served under %s: %s", sh.name, note)
						return
					}
					if !hc.proof(t, in, out) {
						t.Fatalf("the installed %s hook did not run under %s\ncommand: %s\n%s", hc.event, sh.name, command, out)
					}
				})
			}
		}
	}
}

// TestCursorShellHookRecognizesTheSkillsOwnSearch feeds Cursor's installed
// beforeShellExecution hook the search command its own skill renders,
// through Cursor's hook runner on this OS, and requires the answer Cursor
// waits for: allow, with the turn and call stamped into the lineage file.
//
// This is #66's guard, end to end through the real binary: v0.2.9 answered
// nothing here, so every Cursor search waited for a human and none carried
// per-call lineage.
//
// EVERY declared tool shell is fed through, not the first: Cursor on Windows
// now teaches a form for each terminal the participant may have configured
// (#96), and a form we teach and the hook then refuses is #66 again for that
// terminal. Taking shells[0] alone would have left the Git Bash form
// unexercised on the one runner that could catch it.
func TestCursorShellHookRecognizesTheSkillsOwnSearch(t *testing.T) {
	toolShells, err := declaredShells(cursorTarget{}, runtime.GOOS, channelTool)
	if err != nil {
		t.Skipf("Cursor declares no tool shell on %s", runtime.GOOS)
	}
	for _, sh := range hostShellsOnThisOS(t, "cursor", channelHook) {
		for _, toolShell := range toolShells {
			t.Run(sh.name+"/"+string(toolShell), func(t *testing.T) {
				in := newExecInstallation(t)
				spec := hookSpecFor(t, "cursor", in.entry)
				command := ""
				for _, h := range installedHookCommands(t, installedHookFile(t, spec, in.entry), spec) {
					if h.event == "beforeShellExecution" {
						command = h.command
					}
				}
				search := skillBlockFor(t, in.renderedSkill("cursor"), toolShell, "search").body
				payload, _ := json.Marshal(map[string]any{"conversation_id": "exec-conversation", "generation_id": "exec-generation",
					"workspace_roots": []string{in.root}, "command": search})
				out := runInShell(t, sh, command, payload, in.env)
				if rendered, note := hookRunnerItIsRenderedFor(t, "cursor", in.entry); sh.kind != rendered {
					// The same ruling as TestInstalledHookCommandsRunInTheirRunner:
					// this hook command is rendered for one runner and the cell
					// names more. Where it cannot run there is no recognizer to
					// exercise, and saying so is the honest assertion.
					if out.exit == 0 {
						t.Fatalf("Cursor's hook now runs under %s as well; the ruling that kept v0.2.9's form is stale\ncommand: %s\n%s", sh.name, command, out)
					}
					t.Logf("as ruled, not served under %s: %s", sh.name, note)
					return
				}
				if out.exit != 0 || strings.TrimSpace(out.stdout) != `{"permission":"allow"}` {
					t.Fatalf("the %s search form the skill teaches was not auto-allowed:\n%s", toolShell, out)
				}
				if !cursorLineageFileExists(in) {
					t.Fatal("no lineage was stamped for the search Cursor was about to run")
				}
			})
		}
	}
}

// TestHermesHookCommandRunsThroughItsSplitter runs the hook command the
// Hermes install writes through Hermes' own splitter and spawn — the runner
// its cell names, which is no shell at all.
func TestHermesHookCommandRunsThroughItsSplitter(t *testing.T) {
	in := newExecInstallation(t)
	command, ok := hermesHookCommand(in.entry, runtime.GOOS == "windows")
	if !ok {
		t.Fatal("no Hermes hook command for this installation")
	}
	search := skillSearchBlock(t, in.renderedSkill("hermes")).body
	payload, _ := json.Marshal(map[string]any{"hook_event_name": "pre_tool_call", "tool_name": "terminal", "session_id": "exec-session",
		"tool_input": map[string]any{"command": search}, "extra": map[string]any{"tool_call_id": "exec-call"}})
	for _, sh := range hostShellsOnThisOS(t, "hermes", channelHook) {
		out := runInShell(t, sh, command, payload, in.env)
		var got hermesModifyDirective
		if out.exit != 0 || json.Unmarshal([]byte(out.stdout), &got) != nil || got.Decision != "modify" {
			t.Fatalf("the Hermes hook did not answer a modify directive through %s:\n%s", sh.name, out)
		}
		if cmd, _ := got.ToolInput["command"].(string); !strings.HasPrefix(cmd, bridgeEnv+"=") {
			t.Fatalf("modify directive without the bridge: %q", cmd)
		}
	}
}

// harnessFor is the harness value each adapter writes into its bridge.
var harnessFor = map[string]string{"claude": "claude-code", "hermes": "hermes", "opencode": "opencode", "pi": "pi"}

// TestBridgedSearchCarriesItsHostsHarness takes the command each lineage
// adapter hands its host — the adapter itself, run in process or in Node —
// runs it in the shell that host runs tool calls in, and reads the trace the
// router received.
//
// This is #68's guard. v0.2.9 wrote one POSIX assignment for every host, so
// on Windows opencode's prefix was looked up as a program name: the first
// searches failed outright, and the one that ran reached the router with the
// binary's own `cli` fallback harness — a search with no lineage at all. The
// harness assertion is what makes that impossible now.
func TestBridgedSearchCarriesItsHostsHarness(t *testing.T) {
	for _, host := range []string{"claude", "hermes", "opencode", "pi"} {
		for _, sh := range renderedShellsOnThisOS(t, host) {
			t.Run(host+"/"+sh.name, func(t *testing.T) {
				in := newExecInstallation(t)
				var stdin []byte
				command := ""
				if host == "opencode" {
					// opencode has no skill: its command is the rules line's,
					// with the request on stdin.
					rendered, err := in.entry.stdinCommandForShell(sh.kind)
					if err != nil {
						t.Fatal(err)
					}
					command = rendered
					stdin = []byte(`{"version":1,"query":"exact query text"}`)
				} else {
					command = skillBlockFor(t, in.renderedSkill(host), sh.kind, "search").body
				}
				bridged := bridgedCommand(t, host, command, sh.kind)
				if bridged == command || !strings.Contains(bridged, bridgeEnv) {
					t.Fatalf("the %s adapter wrote no bridge for %s: %q", host, sh.kind, bridged)
				}
				out := runInShell(t, sh, bridged, stdin, in.env)
				req := requireOneRequest(t, in, out, "exact query text")
				if req.Trace == nil || req.Trace.Harness != harnessFor[host] {
					t.Fatalf("the router received trace %+v, want harness %q\n%s", req.Trace, harnessFor[host], req.Raw)
				}
			})
		}
	}
}
