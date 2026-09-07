package main

// runFlush against a scripted AS and a router that must never be dialed.
//
// flush.go's own doc comment promises: "the one thing a flush never does
// is touch the router — it talks to the AS only." Nothing drove runFlush
// end to end before this file existed; the AS-facing pieces (driver,
// promote, collector) each had their own tests, but the assembled pass —
// the thing that actually ships the promise — did not.
//
// The router recorder counts CONNECTIONS, not completed requests: the
// wrong-observable hazard (a probe that opens a socket and never
// completes a request still "reached out") means counting handler
// invocations would pass even if something dialed the router and the
// dial merely failed to finish.

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/auth"
	"github.com/twilight-project/dropin-miner/pkg/config"
)

// routerRecorder stands in for the search router. Any TCP connection to
// it — whether or not a request ever completes on it — is a violation of
// "a flush talks to the AS only".
type routerRecorder struct {
	srv   *httptest.Server
	conns atomic.Int64
}

func newRouterRecorder(t *testing.T) *routerRecorder {
	t.Helper()
	r := &routerRecorder{}
	r.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	r.srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			r.conns.Add(1)
		}
	}
	r.srv.Start()
	t.Cleanup(r.srv.Close)
	return r
}

// flushFixture wires a real config file, an enrolled state directory, and
// a scripted AS the same way `flush` itself will see them at runtime:
// through miningClients -> config.Load, not through pkg/auth's
// constructors directly (that shortcut is what newDriverFixture in
// mining_test.go takes; it proves the AS wiring, not the command layer
// runFlush assembles on top of it).
type flushFixture struct {
	cfg     *config.Config
	cfgPath string
	router  *routerRecorder
}

func newFlushFixture(t *testing.T, as *fakeAS) *flushFixture {
	t.Helper()
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The enrolled state a real installation would have after `enroll`:
	// a refresh authorization already in custody.
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRefreshToken("rt-0"); err != nil {
		t.Fatal(err)
	}
	router := newRouterRecorder(t)
	doc := fmt.Sprintf(`
[mining]
enabled = true
as_url = %q
chain_id = %q
slot_id = %d
state_dir = %q
spool_dir = %q

[miner]
enabled = true
router_url = %q
intake_dir = %q
sessions_dir = %q
flush_interval = "1s"
`, as.srv.URL, testChainID, testSlotID, stateDir, filepath.Join(root, "spool"),
		router.srv.URL, filepath.Join(root, "intake"), filepath.Join(root, "sessions"))
	cfgPath := filepath.Join(root, "tokendrop.toml")
	if err := os.WriteFile(cfgPath, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := loadConfig(cfgPath, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	return &flushFixture{cfg: cfg, cfgPath: cfgPath, router: router}
}

// TestFlushNeverDialsTheRouterEvenOnFailure drives the real runFlush —
// join succeeding, and the AS refusing the one call that path depends on
// — and checks the one thing that must be true regardless: the router
// recorder sees zero connections either way.
func TestFlushNeverDialsTheRouterEvenOnFailure(t *testing.T) {
	cases := []struct {
		name string
		set  func(*asState)
	}{
		{"a joinable open epoch, join succeeds", func(s *asState) { s.epoch, s.joinable = 1042, true }},
		{"the AS refusing current-target", func(s *asState) { s.targetStatus = 500 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			as := newFakeAS(t)
			as.set(c.set)
			f := newFlushFixture(t, as)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var stdout, stderr bytes.Buffer
			_, code := runFlush(ctx, f.cfg, f.cfgPath, true, &stdout, &stderr)
			if code != exitOK {
				t.Fatalf("runFlush exit %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
			}
			if n := f.router.conns.Load(); n != 0 {
				t.Fatalf("flush opened %d connection(s) to the router; a flush must talk to the AS only", n)
			}
		})
	}
}
