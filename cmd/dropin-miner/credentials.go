package main

// The key below the environment.
//
// `search` sends the participant's sr- key in Authorization; the router
// meters against it and that is what makes a search payable. Until now
// the key lived only in TOKENDROP_API_KEY, which works for an agent
// started from a shell that exports it and fails for one launched from a
// Dock icon, a service, or a fresh terminal that never sourced the
// profile. `dropin-miner login` stores the key once, owner-only, beside
// the rest of the participant's files; `search` reads it when the
// environment has nothing.
//
// Resolution order, and why:
//
//	TOKENDROP_API_KEY        the environment wins so a shell can override
//	                         the stored key for one session (a second
//	                         account, a CI runner) without touching disk
//	credentials.json         what login wrote: ~/.tokendrop/credentials.json,
//	                         0600, refused if a symlink or readable by
//	                         anyone else — the same posture as the auth
//	                         store's refresh token
//	OPENAI_API_KEY           the fallback every SDK user already exports;
//	                         last, so a personal key never shadows the
//	                         tenant key and bills the wrong account
//
// The key reaches login on stdin or from a named environment variable,
// never as a flag: argv is visible to every process on the machine and
// lands in shell history. Before the file is written the key is checked
// against the router with a zero-spend probe — a search request with no
// query — so a mistyped key is caught now rather than on the first 401
// an agent sees.

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
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/twilight-project/dropin-miner/pkg/config"
)

const (
	credentialsFile    = "credentials.json"
	credentialsVersion = 1

	apiKeyEnv    = "TOKENDROP_API_KEY"
	openAIKeyEnv = "OPENAI_API_KEY"

	// loginProbeTimeout bounds the verification round trip; a router that
	// does not answer in this long is reported, not waited on.
	loginProbeTimeout = 30 * time.Second
)

// keySource names where a resolved key came from, for messages that must
// say which of three places to fix.
type keySource string

const (
	keyFromEnv    keySource = "TOKENDROP_API_KEY"
	keyFromFile   keySource = "credentials file"
	keyFromOpenAI keySource = "OPENAI_API_KEY"
	keyFromNone   keySource = ""
)

// credentials is the on-disk shape. Router records which gateway the key
// was verified against so a later `login -show` can say so; it is
// informational and never overrides the config.
type credentials struct {
	V      int    `json:"v"`
	APIKey string `json:"api_key"`
	Router string `json:"router,omitempty"`
}

// credentialsPath is the file beside the intake directory: with the
// standard layout that is ~/.tokendrop/credentials.json.
func credentialsPath(m config.Miner) string {
	return filepath.Join(minerRoot(m), credentialsFile)
}

// resolveAPIKey applies the order documented above. A credentials file
// that exists but cannot be trusted (symlink, readable by others,
// unparseable) is an error, not a fall-through: silently continuing to
// OPENAI_API_KEY would bill a different account without a word.
func resolveAPIKey(getenv func(string) string, m config.Miner) (string, keySource, error) {
	if k := getenv(apiKeyEnv); k != "" {
		return k, keyFromEnv, nil
	}
	creds, err := readCredentials(credentialsPath(m))
	switch {
	case err == nil:
		return creds.APIKey, keyFromFile, nil
	case !errors.Is(err, fs.ErrNotExist):
		return "", keyFromNone, err
	}
	if k := getenv(openAIKeyEnv); k != "" {
		return k, keyFromOpenAI, nil
	}
	return "", keyFromNone, nil
}

// readCredentials reads and validates the file. Mode checks are gated by
// posixModes exactly as the wallet's are: Go reports 0777 for every file
// on Windows, where the directory ACL is the guard.
func readCredentials(path string) (*credentials, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("credentials: %s is a symlink; refusing", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("credentials: %s is not a regular file; refusing", path)
	}
	if posixModes && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("credentials: %s is readable by others (%04o); refusing — chmod 600 it, or re-run: dropin-miner login",
			path, info.Mode().Perm())
	}
	data, err := os.ReadFile(path) // #nosec G304 -- our own state dir plus a fixed name
	if err != nil {
		return nil, err
	}
	var c credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("credentials: %s does not parse: %w — re-run: dropin-miner login", path, err)
	}
	if c.V != credentialsVersion || strings.TrimSpace(c.APIKey) == "" {
		return nil, fmt.Errorf("credentials: %s is not a v%d credentials file with a key — re-run: dropin-miner login", path, credentialsVersion)
	}
	return &c, nil
}

// writeCredentials creates the file 0600 from the first byte: an O_EXCL
// temp file that never existed with wider bits, then a rename over the
// final name. A reader racing the write sees either the old file or the
// new one, never a partial or a briefly world-readable one.
func writeCredentials(path string, c credentials) error {
	c.V = credentialsVersion
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := fmt.Sprintf("%s.%s.tmp", path, randomSuffix())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- our own state dir
	if err != nil {
		return err
	}
	cleanup := func() { _ = os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// maskKey shows enough to recognize a key and nothing that would let
// anyone use it: the prefix through the first dash and the last four.
func maskKey(k string) string {
	if len(k) <= 10 {
		return "…"
	}
	prefix := ""
	if i := strings.IndexByte(k, '-'); i >= 0 && i < 6 {
		prefix = k[:i+1]
	}
	return prefix + "…" + k[len(k)-4:]
}

// ── the probe ───────────────────────────────────────────────────────────

// probeOutcome is what the router said about a key without spending
// anything on it.
type probeOutcome int

const (
	probeAccepted    probeOutcome = iota // the key authenticated; the request failed for another reason, as intended
	probeRefused                         // 401/403: the router does not know this key
	probeUnavailable                     // 5xx or no answer: nothing learned
)

// probeKey posts to /v1/search with no query and one unknown field. A
// router that authenticates first answers 401 for a bad key and a 4xx
// schema error for a good one; nothing is served, so nothing is billed.
// A 2xx would mean the router served an empty search, which is still a
// verified key.
func probeKey(ctx context.Context, routerURL, key string) (probeOutcome, string, error) {
	endpoint := strings.TrimRight(routerURL, "/") + "/v1/search"
	payload, _ := json.Marshal(map[string]any{"dropin_miner_probe": buildVersion()})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return probeUnavailable, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", searchUserAgent+"/"+strings.TrimPrefix(buildVersion(), "v"))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: loginProbeTimeout}).Do(req)
	if err != nil {
		return probeUnavailable, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return probeRefused, resp.Status, nil
	case resp.StatusCode >= 500:
		return probeUnavailable, resp.Status, nil
	default:
		return probeAccepted, resp.Status, nil
	}
}

// ── the command ─────────────────────────────────────────────────────────

// cmdLogin stores, shows or forgets the key.
//
//	dropin-miner login                 read the key from stdin, verify, store
//	dropin-miner login -key-env VAR    read it from $VAR instead of stdin
//	dropin-miner login -show           where a search would get its key from
//	dropin-miner login -forget         remove the stored file
func cmdLogin(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	flags := newFlagSet("login", stderr)
	cfgPath := flags.String("config", "", "path to TOML config file")
	keyEnv := flags.String("key-env", "", "read the key from this environment variable instead of stdin")
	show := flags.Bool("show", false, "report where a search would get its key from, masked")
	forget := flags.Bool("forget", false, "remove the stored credentials file")
	noVerify := flags.Bool("no-verify", false, "store the key without checking it against the router")
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "dropin-miner login: the key is read from stdin (or -key-env VAR), never from an argument")
		return exitUsage
	}
	if *show && *forget {
		fmt.Fprintln(stderr, "dropin-miner login: -show and -forget are exclusive")
		return exitUsage
	}

	cfg, cfgSource, err := loadConfig(*cfgPath, getenv)
	if err != nil {
		fmt.Fprintf(stderr, "dropin-miner: config (%s): %v\n", orDefaults(cfgSource), err)
		return exitTransport
	}
	path := credentialsPath(cfg.Miner)

	switch {
	case *show:
		return loginShow(stdout, stderr, getenv, cfg.Miner, path)
	case *forget:
		if err := os.Remove(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				fmt.Fprintf(stdout, "no stored key at %s\n", path)
				return exitOK
			}
			fmt.Fprintln(stderr, "dropin-miner login: forget:", err)
			return exitTransport
		}
		fmt.Fprintf(stdout, "removed %s\n", path)
		if getenv(apiKeyEnv) != "" {
			fmt.Fprintln(stdout, "note: TOKENDROP_API_KEY is still set in this shell, so searches here keep working")
		}
		return exitOK
	}

	var key string
	if *keyEnv != "" {
		key = strings.TrimSpace(getenv(*keyEnv))
		if key == "" {
			fmt.Fprintf(stderr, "dropin-miner login: %s is empty or not set\n", *keyEnv)
			return exitUsage
		}
	} else {
		fmt.Fprint(stderr, "paste your sr- key, then press Enter (it is not echoed, logged, or put in a command line): ")
		key, err = readSecret(stdin)
		fmt.Fprintln(stderr)
		if err != nil {
			fmt.Fprintln(stderr, "dropin-miner login:", err)
			return exitUsage
		}
	}

	router := ""
	if cfg.Miner.RouterURL != nil {
		router = strings.TrimRight(cfg.Miner.RouterURL.String(), "/")
	}
	if !*noVerify {
		if router == "" {
			fmt.Fprintln(stderr, "dropin-miner login: no router configured to verify against (miner.router_url or a [[provider]] upstream); use -no-verify to store anyway")
			return exitTransport
		}
		ctx, cancel := operatorContext(loginProbeTimeout)
		defer cancel()
		outcome, status, err := probeKey(ctx, router, key)
		switch {
		case err != nil:
			fmt.Fprintf(stderr, "dropin-miner login: could not reach %s to verify the key: %v\n  nothing was stored; retry, or store unverified with -no-verify\n", router, err)
			return exitTransport
		case outcome == probeUnavailable:
			fmt.Fprintf(stderr, "dropin-miner login: %s answered HTTP %s, so the key could not be verified\n  nothing was stored; retry, or store unverified with -no-verify\n", router, status)
			return exitServerErr
		case outcome == probeRefused:
			fmt.Fprintf(stderr, "dropin-miner login: %s refused this key (HTTP %s); nothing was stored\n  keys are minted at platform.nyks.dev → Keys, and must belong to the account you enrolled with\n", router, status)
			return exitClientErr
		}
	}

	if err := writeCredentials(path, credentials{APIKey: key, Router: router}); err != nil {
		fmt.Fprintln(stderr, "dropin-miner login: write:", err)
		return exitTransport
	}
	verb := "verified against " + router + " and "
	if *noVerify {
		verb = "NOT verified; "
	}
	fmt.Fprintf(stdout, "key %s %sstored in %s (owner-only)\n", maskKey(key), verb, path)
	if env := getenv(apiKeyEnv); env != "" && env != key {
		fmt.Fprintln(stdout, "note: TOKENDROP_API_KEY is set in this shell to a different key, and the environment wins here")
	}
	return exitOK
}

// loginShow reports resolution without printing a key: which source
// would win, masked, and what the others hold.
func loginShow(stdout, stderr io.Writer, getenv func(string) string, m config.Miner, path string) int {
	key, source, err := resolveAPIKey(getenv, m)
	if err != nil {
		fmt.Fprintln(stderr, "dropin-miner login:", err)
		return exitClientErr
	}
	if key == "" {
		fmt.Fprintf(stdout, "no key: TOKENDROP_API_KEY is unset, %s does not exist, OPENAI_API_KEY is unset\n  store one with: dropin-miner login\n", path)
		return exitOK
	}
	fmt.Fprintf(stdout, "key %s from %s\n", maskKey(key), source)
	if source != keyFromFile {
		if c, ferr := readCredentials(path); ferr == nil {
			fmt.Fprintf(stdout, "also stored: %s in %s (shadowed by the environment)\n", maskKey(c.APIKey), path)
		} else {
			fmt.Fprintf(stdout, "nothing stored at %s\n", path)
		}
	} else if c, ferr := readCredentials(path); ferr == nil && c.Router != "" {
		fmt.Fprintf(stdout, "  verified against %s when stored\n", c.Router)
	}
	return exitOK
}

// readSecret reads the key with echo off when stdin is a terminal, so the
// paste does not land on screen or in a scrollback; anything else (a pipe,
// a heredoc, a test buffer) is read as one line.
func readSecret(r io.Reader) (string, error) {
	if f, ok := r.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		data, err := term.ReadPassword(int(f.Fd()))
		if err != nil {
			return "", err
		}
		return secretFromLine(data)
	}
	return readSecretLine(r)
}

// readSecretLine reads one line and trims it; a final line without a
// newline (a piped `printf '%s'`) counts.
func readSecretLine(r io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(r, 4096))
	if err != nil {
		return "", err
	}
	return secretFromLine(data)
}

// secretFromLine keeps the first line and rejects an empty or spaced paste.
func secretFromLine(data []byte) (string, error) {
	line := data
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		line = data[:i]
	}
	key := strings.TrimSpace(string(line))
	if key == "" {
		return "", errors.New("no key on stdin")
	}
	if strings.ContainsAny(key, " \t") {
		return "", errors.New("the key contains whitespace; paste only the key")
	}
	return key, nil
}
