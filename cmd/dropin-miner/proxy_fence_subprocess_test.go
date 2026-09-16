package main

// The masking bug D1's review found is not hypothetical on a real
// developer machine: HTTPS_PROXY is routinely already set — a corporate
// proxy agent, a VPN client, a local debugging proxy — entirely
// independently of anything this repository's own tests do. Go's
// http.ProxyFromEnvironment reads the environment exactly once per process
// and caches the result forever after; internal/networkfence.Install now
// clears the proxy environment before doing anything else, but that fix
// can only be proven in a process whose own first HTTP use happens AFTER
// its own TestMain has run Install — which this package's normal test
// binary cannot demonstrate, because by the time any test in it runs,
// TestMain has already called Install once for the whole binary. Proving
// the fix means re-executing a FRESH process with the proxy environment
// already poisoned before that process's own TestMain gets a chance to
// clear it, and confirming a real listener standing in for that proxy
// never receives a single connection.
//
// The subprocess idiom (a sentinel first argument, dispatched in TestMain
// before the normal test run) is internal/selfupdate/acceptance_test.go's
// own acceptanceHelper, reused here for the same reason it exists there:
// some properties are properties of a whole process's lifetime, not of a
// function call within one already-running test binary.

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/internal/networkfence"
	"github.com/twilight-project/dropin-miner/internal/selfupdate"
	"github.com/twilight-project/dropin-miner/pkg/platform"
)

const proxyFenceHelperArg = "__proxyfence-subprocess-helper"

// proxyFenceProbe is one line of the helper's report: which client, and
// whether it got the fence's own typed refusal.
type proxyFenceProbe struct {
	name    string
	refused bool
	detail  string
}

// runProxyFenceHelper runs inside the re-executed subprocess. It installs
// the fence — which must clear HTTPS_PROXY (and the rest of
// proxyEnvVars) before anything else, or the environment this process was
// launched with is already cached by the time any of the three calls
// below run — then drives the platform client, the search client and the
// self-update source at fenceProbeHost, and reports one line per client on
// stdout: "<name>: refused" or "<name>: NOT REFUSED: <detail>". Exit code
// is 0 only when all three were refused.
func runProxyFenceHelper() int {
	if err := networkfence.Install(); err != nil {
		fmt.Fprintln(os.Stderr, "runProxyFenceHelper: Install:", err)
		return 2
	}

	probes := []proxyFenceProbe{
		probeSearchClient(),
		probePlatformClient(),
		probeSelfUpdateSource(),
	}
	allRefused := true
	for _, p := range probes {
		if p.refused {
			fmt.Printf("%s: refused\n", p.name)
		} else {
			fmt.Printf("%s: NOT REFUSED: %s\n", p.name, p.detail)
			allRefused = false
		}
	}
	if !allRefused {
		return 1
	}
	return 0
}

func probeSearchClient() proxyFenceProbe {
	ctx, cancel := context.WithTimeout(context.Background(), fencedCallTimeout)
	defer cancel()
	call := searchCall{
		Endpoint: "https://" + fenceProbeHost + "/v1/search",
		Key:      "canary-not-a-real-key", // #nosec G101 -- a syntactically valid placeholder, never sent anywhere
		Query:    "proxy fence subprocess probe — must never leave the machine",
	}
	out := performSearch(ctx, time.Now, call)
	return classifyProbe("search", out.Err)
}

func probePlatformClient() proxyFenceProbe {
	ctx, cancel := context.WithTimeout(context.Background(), fencedCallTimeout)
	defer cancel()
	client := platform.New("https://"+fenceProbeHost, "https://"+fenceProbeHost)
	_, err := client.Register(ctx, "proxy-fence-subprocess-probe", []string{"credits"})
	return classifyProbe("platform", err)
}

func probeSelfUpdateSource() proxyFenceProbe {
	// selfupdate.NewHTTPSource's apiBase/downloadBase are compiled-in
	// constants with no override seam (D1c's own report on this), so
	// driving it at fenceProbeHost means going straight through
	// NewHTTPClient — the exported production client every source method
	// uses underneath — rather than through Release, which would only
	// ever be able to target the real GitHub API.
	ctx, cancel := context.WithTimeout(context.Background(), fencedCallTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+fenceProbeHost, nil)
	if err != nil {
		return proxyFenceProbe{name: "selfupdate", refused: false, detail: err.Error()}
	}
	_, err = selfupdate.NewHTTPClient().Do(req)
	return classifyProbe("selfupdate", err)
}

func classifyProbe(name string, err error) proxyFenceProbe {
	if err == nil {
		return proxyFenceProbe{name: name, refused: false, detail: "no error at all — did it actually reach the network?"}
	}
	refusal, ok := networkfence.AsRefusal(err)
	if !ok {
		return proxyFenceProbe{name: name, refused: false, detail: err.Error()}
	}
	if refusal.Addr != fenceProbeHost+":443" {
		return proxyFenceProbe{name: name, refused: false, detail: fmt.Sprintf("refused %q, want %q", refusal.Addr, fenceProbeHost+":443")}
	}
	return proxyFenceProbe{name: name, refused: true}
}

// TestNetworkFenceClearsAnInheritedProxyBeforeAnyHTTPUse is the review's
// own reproduction, made permanent: a loopback listener stands in for a
// proxy the developer's machine already had running — a corporate proxy
// agent, a VPN client, a debugging tool — before this fence ever
// installed. If Install fails to clear HTTPS_PROXY before Go's
// http.ProxyFromEnvironment caches it (once per process, for the rest of
// the process's life), every Clone()-based client in this module
// (Proxy: http.ProxyFromEnvironment, deliberately preserved from
// http.DefaultTransport) would send its CONNECT to this listener instead
// of being refused — and a real proxy, unlike this test's listener, would
// then forward that CONNECT to the real production host.
func TestNetworkFenceClearsAnInheritedProxyBeforeAnyHTTPUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	var mu sync.Mutex
	var connects []string
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_ = c.SetReadDeadline(time.Now().Add(fencedCallTimeout))
				line, _ := bufio.NewReader(c).ReadString('\n')
				if line == "" {
					return
				}
				mu.Lock()
				connects = append(connects, strings.TrimSpace(line))
				mu.Unlock()
			}(conn)
		}
	}()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// #nosec G204 -- exe is this test binary's own path (os.Executable), the argument is the fixed sentinel above
	cmd := exec.Command(exe, proxyFenceHelperArg)
	cmd.Env = append(os.Environ(), "HTTPS_PROXY=http://"+ln.Addr().String(), "https_proxy=http://"+ln.Addr().String())
	out, runErr := cmd.CombinedOutput()

	_ = ln.Close()
	<-acceptDone

	mu.Lock()
	gotConnects := append([]string(nil), connects...)
	mu.Unlock()

	if len(gotConnects) != 0 {
		t.Fatalf("the loopback proxy stand-in received %d connection(s), want 0 — the fence did not clear "+
			"HTTPS_PROXY before an HTTP client cached it:\n%v\nsubprocess output:\n%s", len(gotConnects), gotConnects, out)
	}
	if runErr != nil {
		t.Fatalf("subprocess reported a client that was not refused (exit error: %v):\n%s", runErr, out)
	}
	for _, name := range []string{"search", "platform", "selfupdate"} {
		if !strings.Contains(string(out), name+": refused") {
			t.Errorf("subprocess output does not report %q refused:\n%s", name, out)
		}
	}
}
