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
// H1 is characterization. The tests named TestV029… record v0.2.9's results
// exactly as they are, the failures included, and each names the commit that
// changes it: H2 for the skill's commands and Cursor's recognizer, H3 for
// hook commands and bridge prefixes.

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
	return renderedSkillFor(host, in.entry)
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
// Windows PowerShell 5.1 is the one shell where the control stops short of
// the router, and deliberately: a request piped from a literal inside the
// script arrives there with a byte-order mark in front of it, which
// `search --stdin` refuses (TestV029PowerShellLiteralRequestIsRefused
// records that, and TestV029StdinBytesEachShellDelivers the bytes). What
// this control proves for 5.1 is everything up to that: the shell parsed the
// string, started the binary, and the binary read its stdin and answered.
// Its own standard input, inherited from the host, does arrive exact — so
// the shortfall is 5.1's literal pipe, not the runner.
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
			if sh == shellWinPS {
				requireBinaryAnswered(t, in, out)
				return
			}
			requireOneRequest(t, in, out, "exact query text")
		})
	}
}

// requireBinaryAnswered is the weaker control: the shell ran the binary and
// the binary read stdin and printed its machine envelope, whatever it made
// of the request.
func requireBinaryAnswered(t *testing.T, in *execInstallation, out execOutcome) {
	t.Helper()
	var env struct {
		Version int    `json:"version"`
		Command string `json:"command"`
		Code    string `json:"code"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.stdout)), &env); err != nil || env.Version != machineVersion || env.Command != "search" {
		t.Fatalf("the binary did not answer with a search envelope (%v)\n%s", err, out)
	}
	if len(in.router.received()) == 0 && env.Code == "ok" {
		t.Fatalf("an ok envelope with nothing at the router\n%s", out)
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
	deliveryLiteral   stdinDelivery = "literal"
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
	case deliveryLiteral:
		if sh.kind != shellPowerShell {
			t.Fatalf("a literal-piped request is only rendered for PowerShell, not %s", sh.name)
		}
		script = "'" + strings.ReplaceAll(request, "'", "''") + "' | " + script
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

// v029RunsIn is v0.2.9's one rendering, a POSIX string with %q-quoted paths,
// meeting each real shell. It runs where the shell is a POSIX shell, and in
// cmd for the commands whose only POSIX-specific part is the double-quoted
// path; it does not parse in either PowerShell edition.
func v029RunsIn(sh execShell, cmdAccepts bool) bool {
	switch sh.kind {
	case shellPOSIX, shellArgv:
		return true
	case shellCmd:
		return cmdAccepts
	}
	return false
}

// v029DeliveredBytes is what each shell delivered to a native program's
// stdin when this was characterized, as hex-dumped on the CI runners.
//
// Everything delivers the request byte for byte, whatever the payload,
// except one case: Windows PowerShell 5.1 piping a literal written inside
// the script. There the bytes arrive with a UTF-8 byte-order mark in front,
// CRLF behind, and every UTF-16 unit outside ASCII replaced by "?" — an
// astral character, being two units, arrives as two. (5.1 reports
// $OutputEncoding us-ascii with an empty preamble, so the mark is recorded
// as observed, not explained.) pwsh delivers the request exactly, with CRLF
// behind it.
//
// This matters because a PowerShell host has no heredoc: a literal piped
// inside the one command string is how a request body reaches the binary
// there. H2 owns the fix — a form that carries arbitrary query text byte
// for byte — and flips these rows.
func v029DeliveredBytes(sh execShell, delivery stdinDelivery, request string) []byte {
	if delivery == deliveryInherited {
		return []byte(request)
	}
	if sh == shellWinPS {
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
	return append([]byte(request), '\r', '\n')
}

// TestV029StdinBytesEachShellDelivers dumps, for four payload kinds, the
// exact bytes each real shell hands a native program's standard input.
// H2 chooses the PowerShell form from this evidence and flips the rows it
// changes.
func TestV029StdinBytesEachShellDelivers(t *testing.T) {
	payloads := map[string]string{
		"ascii":  "exact query text",
		"latin1": "café naïve Ärger",
		"cjk":    "東京の天気",
		"emoji":  "weather 😀 today",
	}
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
	for name, query := range payloads {
		request := `{"version":1,"query":"` + query + `"}`
		for _, r := range runs {
			t.Run(name+"/"+r.sh.name+"/"+string(r.delivery), func(t *testing.T) {
				got := stdinBytesDelivered(t, r.sh, r.delivery, request)
				if want := v029DeliveredBytes(r.sh, r.delivery, request); !bytes.Equal(got, want) {
					t.Fatalf("%s delivered\n got %x\nwant %x", r.sh.name, got, want)
				}
			})
		}
	}
}

// TestV029PowerShellLiteralRequestIsRefused is the search that follows from
// those bytes: on Windows PowerShell 5.1 a request piped from a literal —
// the only way a PowerShell command carries a body — reaches the binary and
// is refused, because of the byte-order mark in front of it. pwsh, whose
// pipe delivers the request exactly, is served.
//
// v0.2.9 has no PowerShell form of its own; this is the form the soak team
// proved reaches the binary (#67), failing for a second reason. H2 renders a
// PowerShell search that carries arbitrary query text and flips this.
func TestV029PowerShellLiteralRequestIsRefused(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell is the Windows runner's shell")
	}
	for _, sh := range []execShell{shellWinPS, shellPwsh} {
		t.Run(sh.name, func(t *testing.T) {
			in := newExecInstallation(t)
			script, _ := knownGoodSearch(sh, in)
			out := runInShell(t, sh, script, nil, in.env)
			var env struct {
				OK   bool   `json:"ok"`
				Code string `json:"code"`
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(out.stdout)), &env); err != nil {
				t.Fatalf("no machine envelope from the binary (%v)\n%s", err, out)
			}
			if sh == shellPwsh {
				if !env.OK {
					t.Fatalf("pwsh's pipe was refused: %s\n%s", env.Code, out)
				}
				requireOneRequest(t, in, out, "exact query text")
				return
			}
			if env.OK || env.Code != "invalid_json" {
				t.Fatalf("Windows PowerShell 5.1: ok=%v code=%q, want a refusal with invalid_json\n%s", env.OK, env.Code, out)
			}
			requireNoRequest(t, in, out)
		})
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

// unknownCellCandidates are the shells an unknown cell is characterized
// under: the ones its evidence points at without establishing. Codex on
// Windows defaults to PowerShell in its source; Cursor's Linux hook runner is
// unnamed, and macOS ran the POSIX form.
var unknownCellCandidates = map[string][]shellKind{
	"codex windows tool": {shellPowerShell},
	"cursor linux hook":  {shellPOSIX},
}

func requireOneRequest(t *testing.T, in *execInstallation, out execOutcome, query string) execRouterRequest {
	t.Helper()
	got := in.router.received()
	if len(got) != 1 || got[0].Query != query {
		t.Fatalf("the router received %d request(s) %v, want exactly one with query %q\n%s", len(got), got, query, out)
	}
	return got[0]
}

func requireNoRequest(t *testing.T, in *execInstallation, out execOutcome) {
	t.Helper()
	if got := in.router.received(); len(got) != 0 {
		t.Fatalf("the router received %d request(s), want none (v0.2.9 does not run here)\n%s", len(got), out)
	}
	if out.exit == 0 {
		t.Fatalf("the shell exited 0 without the binary reaching the router\n%s", out)
	}
}

// TestV029SkillCommandsInEachHostsShell runs the three commands every
// host's skill renders — the heredoc search, the preference command and the
// human form — in the shell that host runs tool calls in on this OS.
//
// v0.2.9: every one is a POSIX string, so each runs under a POSIX shell and
// none parses in PowerShell (#67). H2 renders each host's commands for its
// own shell and flips the PowerShell rows.
func TestV029SkillCommandsInEachHostsShell(t *testing.T) {
	for _, host := range []string{"claude", "codex", "cursor", "pi", "hermes"} {
		for _, sh := range hostShellsOnThisOS(t, host, channelTool) {
			t.Run(host+"/"+sh.name, func(t *testing.T) {
				t.Run("search", func(t *testing.T) {
					in := newExecInstallation(t)
					block := skillSearchBlock(t, in.renderedSkill(host))
					out := runInShell(t, sh, block.body, nil, in.env)
					if v029RunsIn(sh, false) {
						requireOneRequest(t, in, out, "exact query text")
					} else {
						requireNoRequest(t, in, out)
					}
				})
				t.Run("preference", func(t *testing.T) {
					in := newExecInstallation(t)
					block := skillPreferBlock(t, in.renderedSkill(host))
					script := strings.Replace(block.body, "<argument>", "status", 1)
					out := runInShell(t, sh, script, nil, in.env)
					ran := out.exit == 0 && strings.Contains(out.stdout, "search default:")
					if ran != v029RunsIn(sh, true) {
						t.Fatalf("preference command ran=%v, v0.2.9 on %s: %v\n%s", ran, sh.name, v029RunsIn(sh, true), out)
					}
				})
				t.Run("human form", func(t *testing.T) {
					in := newExecInstallation(t)
					lines := skillProseCommandLines(in.renderedSkill(host))
					if len(lines) != 1 {
						t.Fatalf("want one prose command line, got %q", lines)
					}
					_, rest, _ := strings.Cut(lines[0], "`")
					command, _, _ := strings.Cut(rest, "`")
					script := strings.Replace(command, "<query>", "exact query text", 1)
					out := runInShell(t, sh, script, nil, in.env)
					if v029RunsIn(sh, true) {
						requireOneRequest(t, in, out, "exact query text")
					} else {
						requireNoRequest(t, in, out)
					}
				})
			})
		}
	}
}

// TestV029RulesLineCommandInOpencodesShell runs the command opencode's
// AGENTS.md line renders, with the JSON request on stdin, in the shell
// opencode runs its bash tool in on this OS.
//
// v0.2.9: a POSIX string; it runs under POSIX and does not parse in either
// PowerShell edition. H2 renders it for opencode's shell.
func TestV029RulesLineCommandInOpencodesShell(t *testing.T) {
	for _, sh := range hostShellsOnThisOS(t, "opencode", channelTool) {
		t.Run(sh.name, func(t *testing.T) {
			in := newExecInstallation(t)
			out := runInShell(t, sh, in.entry.stdinCommand(), []byte(`{"version":1,"query":"exact query text"}`), in.env)
			if v029RunsIn(sh, true) {
				requireOneRequest(t, in, out, "exact query text")
			} else {
				requireNoRequest(t, in, out)
			}
		})
	}
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
				return map[string]any{"conversation_id": "exec-conversation", "generation_id": "exec-generation",
					"workspace_roots": []string{in.root}, "command": in.entry.stdinCommand()}
			},
			proof: func(t *testing.T, in *execInstallation, out execOutcome) bool {
				return out.exit == 0 && strings.TrimSpace(out.stdout) == `{"permission":"allow"}` && lineageFileExists(in)
			},
		},
		{
			event: "afterAgentThought",
			payload: func(in *execInstallation) any {
				return map[string]any{"conversation_id": "exec-conversation", "workspace_roots": []string{in.root}, "text": "thinking before a search"}
			},
			proof: func(t *testing.T, in *execInstallation, out execOutcome) bool {
				return out.exit == 0 && lineageFileExists(in)
			},
		},
		{
			event: "afterAgentResponse",
			payload: func(in *execInstallation) any {
				return map[string]any{"conversation_id": "exec-conversation", "workspace_roots": []string{in.root}, "text": "a sentence before a search"}
			},
			proof: func(t *testing.T, in *execInstallation, out execOutcome) bool {
				return out.exit == 0 && lineageFileExists(in)
			},
		},
		{
			event: "preCompact",
			payload: func(in *execInstallation) any {
				return map[string]any{"conversation_id": "exec-conversation", "workspace_roots": []string{in.root}}
			},
			proof: func(t *testing.T, in *execInstallation, out execOutcome) bool {
				return out.exit == 0 && lineageFileExists(in)
			},
		},
	},
}

// TestV029InstalledHookCommandsInEachHookRunner reads each hook command back
// from the file the install writes and runs it, with a real payload, through
// the runner that host's hook cell names on this OS (all three for Cursor on
// Windows).
//
// v0.2.9: every command begins with a %q-quoted path. It runs under POSIX sh,
// Git Bash and cmd; in PowerShell a command that starts with a quoted string
// is an expression, and the next word is a parse error (#69). H3 renders hook
// commands for their runner and flips the PowerShell rows.
func TestV029InstalledHookCommandsInEachHookRunner(t *testing.T) {
	for _, host := range []string{"claude", "cursor"} {
		for _, sh := range hostShellsOnThisOS(t, host, channelHook) {
			for _, hc := range v029HookCases[host] {
				t.Run(host+"/"+sh.name+"/"+hc.event, func(t *testing.T) {
					in := newExecInstallation(t)
					spec := claudeHooks(in.entry)
					if host == "cursor" {
						spec = cursorHooks(in.entry)
					}
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
					if ran := hc.proof(t, in, out); ran != v029RunsIn(sh, true) {
						t.Fatalf("%s ran=%v under %s; v0.2.9: %v\ncommand: %s\n%s", hc.event, ran, sh.name, v029RunsIn(sh, true), command, out)
					}
				})
			}
		}
	}
}

// TestV029CursorShellHookDoesNotRecognizeTheSkillsSearch feeds Cursor's
// installed beforeShellExecution hook the search command its own skill
// renders, through Cursor's hook runner on this OS.
//
// v0.2.9: the recognizer refuses the heredoc — no allow, no turn or call
// stamped into the lineage file (#66). H2 makes it recognize exactly the
// rendered form.
func TestV029CursorShellHookDoesNotRecognizeTheSkillsSearch(t *testing.T) {
	for _, sh := range hostShellsOnThisOS(t, "cursor", channelHook) {
		if !v029RunsIn(sh, true) {
			continue // the hook command itself does not run here; #69, not #66
		}
		t.Run(sh.name, func(t *testing.T) {
			in := newExecInstallation(t)
			spec := cursorHooks(in.entry)
			command := ""
			for _, h := range installedHookCommands(t, installedHookFile(t, spec, in.entry), spec) {
				if h.event == "beforeShellExecution" {
					command = h.command
				}
			}
			search := skillSearchBlock(t, in.renderedSkill("cursor")).body
			payload, _ := json.Marshal(map[string]any{"conversation_id": "exec-conversation", "generation_id": "exec-generation",
				"workspace_roots": []string{in.root}, "command": search})
			out := runInShell(t, sh, command, payload, in.env)
			if out.exit != 0 || strings.TrimSpace(out.stdout) != "" {
				t.Fatalf("v0.2.9's recognizer answered for the skill's own search:\n%s", out)
			}
			if lineageFileExists(in) {
				t.Fatal("v0.2.9 stamped lineage for a command it did not recognize")
			}
		})
	}
}

// TestV029HermesHookCommandThroughItsSplitter runs the hook command the
// Hermes install writes through Hermes' own splitter and spawn.
//
// v0.2.9: it runs on every OS and answers a modify directive with the bridge.
// H3 keeps it unless the evidence table says otherwise.
func TestV029HermesHookCommandThroughItsSplitter(t *testing.T) {
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

// TestV029BridgedSearchInEachHostsShell takes the command each lineage
// adapter hands its host — the adapter itself, run in process or in Node —
// and runs it in that host's tool shell on this OS, then reads the trace the
// router received.
//
// v0.2.9: every adapter prefixes the POSIX assignment TOKENDROP_TRACE_BRIDGE=…
// It carries the host's harness under a POSIX shell; in PowerShell the prefix
// is looked up as a command and the search never runs (#68). H3 writes each
// prefix in its host's shell and flips the PowerShell rows.
func TestV029BridgedSearchInEachHostsShell(t *testing.T) {
	for _, host := range []string{"claude", "hermes", "opencode", "pi"} {
		for _, sh := range hostShellsOnThisOS(t, host, channelTool) {
			t.Run(host+"/"+sh.name, func(t *testing.T) {
				in := newExecInstallation(t)
				command := skillSearchBlock(t, in.renderedSkill("claude")).body
				var stdin []byte
				if host == "opencode" {
					// opencode has no skill: its command is the rules line's,
					// with the request on stdin.
					command = in.entry.stdinCommand()
					stdin = []byte(`{"version":1,"query":"exact query text"}`)
				}
				bridged := bridgedCommand(t, host, command)
				if !strings.HasPrefix(bridged, bridgeEnv+"=") {
					t.Fatalf("the %s adapter wrote no bridge: %q", host, bridged)
				}
				out := runInShell(t, sh, bridged, stdin, in.env)
				if !v029RunsIn(sh, false) {
					requireNoRequest(t, in, out)
					return
				}
				req := requireOneRequest(t, in, out, "exact query text")
				if req.Trace == nil || req.Trace.Harness != harnessFor[host] {
					t.Fatalf("the router received trace %+v, want harness %q\n%s", req.Trace, harnessFor[host], req.Raw)
				}
			})
		}
	}
}
