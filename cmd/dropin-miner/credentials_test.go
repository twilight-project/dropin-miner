package main

// The key below the environment: resolution order, the file's posture,
// and `login` against a fake router. Every key here is a synthetic canary.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/twilight-project/dropin-miner/pkg/config"
)

// emptyMiner is a Miner whose credentials path is inside a fresh temp dir,
// so no test can read a developer's real file.
func emptyMiner(t *testing.T) config.Miner {
	t.Helper()
	root := t.TempDir()
	return config.Miner{IntakeDir: filepath.Join(root, "intake"), SessionsDir: filepath.Join(root, "sessions")}
}

func TestResolveAPIKeyOrderIsEnvThenFileThenOpenAI(t *testing.T) {
	m := emptyMiner(t)
	path := credentialsPath(m)
	if filepath.Dir(path) != filepath.Dir(m.IntakeDir) || filepath.Base(path) != credentialsFile {
		t.Fatalf("credentials file is not beside the intake dir: %s", path)
	}
	if err := writeCredentials(path, credentials{APIKey: "sr-canary-stored", Router: "https://router.fictional.test"}); err != nil {
		t.Fatal(err)
	}
	if posixModes {
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
			t.Errorf("credentials file mode %04o, want 0600", info.Mode().Perm())
		}
	}

	k, src, err := resolveAPIKey(envOf(map[string]string{"TOKENDROP_API_KEY": "sr-canary-env", "OPENAI_API_KEY": "sk-canary"}), m)
	if err != nil || k != "sr-canary-env" || src != keyFromEnv {
		t.Errorf("env should win: %q %q %v", k, src, err)
	}
	k, src, err = resolveAPIKey(envOf(map[string]string{"OPENAI_API_KEY": "sk-canary"}), m)
	if err != nil || k != "sr-canary-stored" || src != keyFromFile {
		t.Errorf("file should beat OPENAI_API_KEY: %q %q %v", k, src, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	k, src, err = resolveAPIKey(envOf(map[string]string{"OPENAI_API_KEY": "sk-canary"}), m)
	if err != nil || k != "sk-canary" || src != keyFromOpenAI {
		t.Errorf("OPENAI_API_KEY is the last resort: %q %q %v", k, src, err)
	}
}

func TestResolveAPIKeyRefusesAFileOthersCanRead(t *testing.T) {
	if !posixModes {
		t.Skip("mode bits are not meaningful on Windows")
	}
	m := emptyMiner(t)
	path := credentialsPath(m)
	if err := writeCredentials(path, credentials{APIKey: "sr-canary-stored"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	// Refused, and NOT silently replaced by the OpenAI fallback.
	k, src, err := resolveAPIKey(envOf(map[string]string{"OPENAI_API_KEY": "sk-canary"}), m)
	if err == nil || !strings.Contains(err.Error(), "refusing") || k != "" || src != keyFromNone {
		t.Errorf("a readable credentials file must be refused: %q %q %v", k, src, err)
	}
	if !strings.Contains(err.Error(), "dropin-miner login") {
		t.Errorf("the refusal should name the fix: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if k, _, err := resolveAPIKey(noEnv, m); err != nil || k != "sr-canary-stored" {
		t.Errorf("0600 again should read: %q %v", k, err)
	}
}

func TestResolveAPIKeyRefusesASymlinkAndACorruptFile(t *testing.T) {
	m := emptyMiner(t)
	path := credentialsPath(m)
	real := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := writeCredentials(real, credentials{APIKey: "sr-canary-linked"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, path); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	if _, _, err := resolveAPIKey(noEnv, m); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("symlink not refused: %v", err)
	}
	_ = os.Remove(path)

	if err := os.WriteFile(path, []byte(`{"v":1,"api_key":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveAPIKey(noEnv, m); err == nil || !strings.Contains(err.Error(), "dropin-miner login") {
		t.Errorf("empty key not refused with the fix named: %v", err)
	}
	if err := os.WriteFile(path, []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveAPIKey(noEnv, m); err == nil || !strings.Contains(err.Error(), "does not parse") {
		t.Errorf("garbage not refused: %v", err)
	}
}

func TestMaskKeyShowsPrefixAndTailOnly(t *testing.T) {
	if got := maskKey("sr-0123456789abcdef"); got != "sr-…cdef" {
		t.Errorf("mask: %q", got)
	}
	if got := maskKey("short"); got != "…" {
		t.Errorf("short keys show nothing: %q", got)
	}
	if strings.Contains(maskKey("sr-0123456789abcdef"), "0123") {
		t.Error("the mask leaks the body of the key")
	}
}

// keyGate is a router that authenticates before it validates: the good key
// gets a schema error for the missing query, everything else gets 401.
// The probe must never make it serve a search.
func keyGate(good string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+good {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"query is required"}`))
	}
}

func runLogin(t *testing.T, stdin string, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := cmdLogin(args, strings.NewReader(stdin), &out, &errOut, envOf(env))
	return code, out.String(), errOut.String()
}

func TestLoginVerifiesWithoutSpendingThenStoresAndSearchUsesIt(t *testing.T) {
	fr, cfg, root := newFakeRouter(t, keyGate("sr-canary-good"))
	path := filepath.Join(root, credentialsFile)

	code, out, errOut := runLogin(t, "sr-canary-good\n", nil, "-config", cfg)
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "sr-…good") || !strings.Contains(out, path) || strings.Contains(out, "sr-canary-good") {
		t.Errorf("login output should mask the key and name the file: %q", out)
	}
	req, sent := fr.last(t)
	if req.URL.Path != "/v1/search" || req.Header.Get("Authorization") != "Bearer sr-canary-good" {
		t.Errorf("probe: %s %q", req.URL.Path, req.Header.Get("Authorization"))
	}
	var body map[string]any
	if err := json.Unmarshal(sent, &body); err != nil || body["query"] != nil {
		t.Errorf("the probe must carry no query: %s", sent)
	}

	c, err := readCredentials(path)
	if err != nil || c.APIKey != "sr-canary-good" || c.V != 1 || !strings.HasPrefix(c.Router, "http://") {
		t.Fatalf("stored: %+v %v", c, err)
	}
	if posixModes {
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
			t.Errorf("mode %04o", info.Mode().Perm())
		}
	}

	// A search in a shell with no key at all now authenticates from the file.
	fr.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fr.mu.Lock()
		fr.reqs = append(fr.reqs, r.Clone(r.Context()))
		fr.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer sr-canary-good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("X-Request-Id", "01a03e86-fictional")
		_, _ = w.Write([]byte(routerBody))
	})
	h := fixedSearchOps(root)
	code, sOut, sErr := runSearch(t, h, nil, "-config", cfg, "q")
	if code != exitOK || sOut != routerBody {
		t.Fatalf("search from the stored key: exit %d err %q", code, sErr)
	}
	if req, _ := fr.last(t); req.Header.Get("Authorization") != "Bearer sr-canary-good" {
		t.Errorf("search did not send the stored key: %q", req.Header.Get("Authorization"))
	}
	// And the environment still wins over the file.
	runSearch(t, h, map[string]string{"TOKENDROP_API_KEY": "sr-canary-env"}, "-config", cfg, "q")
	if req, _ := fr.last(t); req.Header.Get("Authorization") != "Bearer sr-canary-env" {
		t.Errorf("env did not override the file: %q", req.Header.Get("Authorization"))
	}
}

func TestLoginRefusedKeyStoresNothing(t *testing.T) {
	_, cfg, root := newFakeRouter(t, keyGate("sr-canary-good"))
	code, _, errOut := runLogin(t, "sr-canary-wrong\n", nil, "-config", cfg)
	if code != exitClientErr || !strings.Contains(errOut, "refused this key") || !strings.Contains(errOut, "nothing was stored") {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(root, credentialsFile)); !os.IsNotExist(err) {
		t.Error("a refused key was written to disk")
	}
	if strings.Contains(errOut, "sr-canary-wrong") {
		t.Error("the refused key was echoed")
	}
}

func TestLoginRouterDownStoresNothingUnlessNoVerify(t *testing.T) {
	_, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	code, _, errOut := runLogin(t, "sr-canary-good\n", nil, "-config", cfg)
	if code != exitServerErr || !strings.Contains(errOut, "-no-verify") {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(root, credentialsFile)); !os.IsNotExist(err) {
		t.Error("an unverified key was written without -no-verify")
	}
	code, out, errOut := runLogin(t, "sr-canary-good\n", nil, "-config", cfg, "-no-verify")
	if code != exitOK || !strings.Contains(out, "NOT verified") {
		t.Fatalf("exit %d out %q err %q", code, out, errOut)
	}
	if c, err := readCredentials(filepath.Join(root, credentialsFile)); err != nil || c.APIKey != "sr-canary-good" {
		t.Errorf("stored: %+v %v", c, err)
	}
}

func TestLoginReadsFromANamedEnvVarAndNeverFromArgv(t *testing.T) {
	_, cfg, root := newFakeRouter(t, keyGate("sr-canary-good"))
	code, _, errOut := runLogin(t, "", map[string]string{"MY_KEY": "sr-canary-good"}, "-config", cfg, "-key-env", "MY_KEY")
	if code != exitOK {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	if c, err := readCredentials(filepath.Join(root, credentialsFile)); err != nil || c.APIKey != "sr-canary-good" {
		t.Errorf("stored: %+v %v", c, err)
	}
	if code, _, _ := runLogin(t, "", nil, "-config", cfg, "-key-env", "UNSET_VAR"); code != exitUsage {
		t.Errorf("an unset -key-env should be a usage error, got %d", code)
	}
	if code, _, errOut := runLogin(t, "", nil, "-config", cfg, "sr-canary-good"); code != exitUsage || !strings.Contains(errOut, "never from an argument") {
		t.Errorf("a key in argv must be refused: %d %q", code, errOut)
	}
	if code, _, errOut := runLogin(t, "\n", nil, "-config", cfg); code != exitUsage || !strings.Contains(errOut, "no key on stdin") {
		t.Errorf("empty stdin: %d %q", code, errOut)
	}
}

func TestLoginShowAndForget(t *testing.T) {
	_, cfg, root := newFakeRouter(t, keyGate("sr-canary-good"))
	path := filepath.Join(root, credentialsFile)

	code, out, _ := runLogin(t, "", nil, "-config", cfg, "-show")
	if code != exitOK || !strings.Contains(out, "no key") || !strings.Contains(out, "dropin-miner login") {
		t.Errorf("show with nothing: %d %q", code, out)
	}

	if code, _, errOut := runLogin(t, "sr-canary-good\n", nil, "-config", cfg); code != exitOK {
		t.Fatalf("login: %d %s", code, errOut)
	}
	code, out, _ = runLogin(t, "", nil, "-config", cfg, "-show")
	if code != exitOK || !strings.Contains(out, "sr-…good from credentials file") || strings.Contains(out, "sr-canary-good") {
		t.Errorf("show: %d %q", code, out)
	}
	code, out, _ = runLogin(t, "", map[string]string{"TOKENDROP_API_KEY": "sr-canary-envkey"}, "-config", cfg, "-show")
	if code != exitOK || !strings.Contains(out, "from TOKENDROP_API_KEY") || !strings.Contains(out, "shadowed by the environment") {
		t.Errorf("show with env set: %d %q", code, out)
	}

	code, out, _ = runLogin(t, "", nil, "-config", cfg, "-forget")
	if code != exitOK || !strings.Contains(out, "removed") {
		t.Errorf("forget: %d %q", code, out)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("forget left the file")
	}
	if code, out, _ := runLogin(t, "", nil, "-config", cfg, "-forget"); code != exitOK || !strings.Contains(out, "no stored key") {
		t.Errorf("second forget: %d %q", code, out)
	}
	if code, _, _ := runLogin(t, "", nil, "-config", cfg, "-show", "-forget"); code != exitUsage {
		t.Errorf("-show -forget together: %d", code)
	}
}

func TestSearchWithoutAnyKeyNamesLogin(t *testing.T) {
	fr, cfg, root := newFakeRouter(t, nil)
	h := fixedSearchOps(root)
	code, _, errOut := runSearch(t, h, nil, "-config", cfg, "q")
	if code != exitClientErr || !strings.Contains(errOut, "dropin-miner login") {
		t.Fatalf("exit %d err %q", code, errOut)
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if len(fr.reqs) != 0 {
		t.Error("the router was called without a key")
	}
}

func TestSearch401NamesTheKeySourceAndLogin(t *testing.T) {
	_, cfg, root := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	h := fixedSearchOps(root)
	code, _, errOut := runSearch(t, h, map[string]string{"TOKENDROP_API_KEY": "sr-canary-env"}, "-config", cfg, "q")
	if code != exitClientErr || !strings.Contains(errOut, "from TOKENDROP_API_KEY") || !strings.Contains(errOut, "dropin-miner login") {
		t.Errorf("exit %d err %q", code, errOut)
	}
}
