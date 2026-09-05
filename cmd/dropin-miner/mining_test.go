package main

// The epoch driver against an AS that answers for real: discovery,
// OAuth, the current-target endpoint, epoch status, join (with a
// receipt the client verifies against the published key), and the
// capability exchange. The loop's whole job is to survive whatever this
// server does, so the tests script the server rather than the clients.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/auth"
	"github.com/twilight-project/dropin-miner/pkg/mining/scope"
	"github.com/twilight-project/dropin-miner/pkg/redact"
)

const (
	testChainID = "twilight-1"
	testSlotID  = 7
)

// asState is everything a test scripts about the AS.
type asState struct {
	// advertise controls whether the discovery document carries
	// current_target_endpoint_template at all.
	advertise bool
	// targetStatus, when non-zero, is the status code the current-target
	// endpoint answers with instead of a body.
	targetStatus int
	// tokenStatus, when non-zero, is the status the TOKEN endpoint answers
	// with, over an invalid_grant body: a revoked family, which is where a
	// participant lands when a rotation is reused. It refuses at the token
	// endpoint rather than at a resource route deliberately — a credential
	// failure reproduced anywhere else proves something about routing.
	tokenStatus int
	// epoch is the open target; -1 means the AS has nothing open.
	epoch      int64
	joinable   bool
	joinStatus string
}

type fakeAS struct {
	srv        *httptest.Server
	receiptKey ed25519.PrivateKey

	mu    sync.Mutex
	state asState

	currentCalls atomic.Int64
	statusCalls  atomic.Int64
	joinCalls    atomic.Int64
	// probes counts every request mentioning current-target, routed or
	// not: a client that assembled the URL itself is still seen here.
	probes atomic.Int64
}

func (f *fakeAS) set(mutate func(*asState)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mutate(&f.state)
}

func (f *fakeAS) get() asState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func newFakeAS(t *testing.T) *fakeAS {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeAS{
		receiptKey: priv,
		state: asState{
			advertise:  true,
			epoch:      -1,
			joinStatus: "NOT_JOINED",
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/twilight-mining", func(w http.ResponseWriter, _ *http.Request) {
		f.writeJSON(t, w, f.document())
	})
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		base := f.srv.URL
		f.writeJSON(t, w, map[string]string{
			"issuer":                        base,
			"authorization_endpoint":        base + "/oauth/authorize",
			"token_endpoint":                base + "/oauth/token",
			"device_authorization_endpoint": base + "/oauth/device_authorization",
			"revocation_endpoint":           base + "/oauth/revoke",
			"jwks_uri":                      base + "/oauth/jwks.json",
		})
	})
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) { f.token(t, w, r) })
	mux.HandleFunc("GET /oauth/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		pub, ok := f.receiptKey.Public().(ed25519.PublicKey)
		if !ok {
			t.Fatal("receipt key is not Ed25519")
		}
		f.writeJSON(t, w, map[string]any{"keys": []map[string]string{{
			"kty": "OKP", "crv": "Ed25519", "use": "sig", "alg": "EdDSA", "kid": "receipt-1",
			"x": base64.RawURLEncoding.EncodeToString(pub),
		}}})
	})
	mux.HandleFunc("GET /v1/mining/slots/{slot}/current-target", func(w http.ResponseWriter, _ *http.Request) {
		f.currentCalls.Add(1)
		st := f.get()
		if st.targetStatus != 0 {
			w.WriteHeader(st.targetStatus)
			f.writeJSON(t, w, map[string]any{"error": map[string]string{"code": "UNAVAILABLE", "message": "down"}})
			return
		}
		if st.epoch < 0 {
			f.writeJSON(t, w, map[string]any{"status": auth.NoOpenTarget, "target": nil})
			return
		}
		f.writeJSON(t, w, map[string]any{"status": "OPEN", "target": map[string]any{
			"slot_id":           strconv.Itoa(testSlotID),
			"target_epoch":      strconv.FormatInt(st.epoch, 10),
			"joinable":          st.joinable,
			"distribution_mode": "RANDOM_SLOT",
			"join_status":       st.joinStatus,
			"phase":             "ENROLLMENT",
		}})
	})
	mux.HandleFunc("GET /v1/mining/slots/{slot}/epochs/{epoch}/status", func(w http.ResponseWriter, r *http.Request) {
		f.statusCalls.Add(1)
		st := f.get()
		f.writeJSON(t, w, map[string]any{
			"slot_id":           r.PathValue("slot"),
			"target_epoch":      r.PathValue("epoch"),
			"phase":             "ENROLLMENT",
			"distribution_mode": "RANDOM_SLOT",
			"joinable":          st.joinable,
			"join_status":       st.joinStatus,
		})
	})
	mux.HandleFunc("POST /v1/mining/slots/{slot}/epochs/{epoch}/join", func(w http.ResponseWriter, r *http.Request) {
		f.joinCalls.Add(1)
		var body struct {
			ChainID string `json:"chain_id"`
			DrawID  string `json:"draw_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("join body: %v", err)
		}
		epoch, err := strconv.ParseUint(r.PathValue("epoch"), 10, 64)
		if err != nil {
			t.Errorf("join path epoch %q: %v", r.PathValue("epoch"), err)
		}
		w.WriteHeader(http.StatusCreated)
		f.writeJSON(t, w, map[string]string{
			"status":  auth.JoinAccepted,
			"receipt": f.receipt(t, epoch, body.DrawID),
		})
	})

	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "current-target") {
			f.probes.Add(1)
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAS) writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Error(err)
	}
}

// document is the §19 discovery document, with the optional
// current-target template present only when the test says so.
func (f *fakeAS) document() map[string]any {
	base := f.srv.URL
	doc := map[string]any{
		"version":                                 "twilight-mining-as-v1",
		"chain_id":                                testChainID,
		"slot_id":                                 strconv.Itoa(testSlotID),
		"authorization_server":                    base,
		"join_epoch_endpoint_template":            base + "/v1/mining/slots/{slot_id}/epochs/{target_epoch}/join",
		"epoch_status_endpoint_template":          base + "/v1/mining/slots/{slot_id}/epochs/{target_epoch}/status",
		"candidate_list_endpoint_template":        base + "/v1/selection/slots/{slot_id}/epochs/{target_epoch}/candidates",
		"participation_resource":                  base + "/v1/mining/observations",
		"observations_endpoint":                   base + "/v1/mining/observations",
		"observations_batch_endpoint":             base + "/v1/mining/observations/batch",
		"observation_status_endpoint_template":    base + "/v1/mining/observations/{observation_id}",
		"activity_status_endpoint_template":       base + "/v1/mining/slots/{slot_id}/epochs/{target_epoch}/activity",
		"provider_verification_endpoint_template": base + "/v1/provider-verification/{provider}",
		"source_profiles":                         []string{"OPENROUTER_V1"},
		"max_observation_bytes":                   16384,
		"max_observation_batch_size":              100,
		"max_observation_batch_bytes":             1048576,
	}
	if f.get().advertise {
		doc["current_target_endpoint_template"] = base + "/v1/mining/slots/{slot_id}/current-target"
	}
	return doc
}

// token serves both grants the driver needs: the access-token refresh
// and the RFC 8693 exchange that mints a Participation Capability.
func (f *fakeAS) token(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	if err := r.ParseForm(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	status := f.state.tokenStatus
	f.mu.Unlock()
	if status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"token family revoked"}`))
		return
	}
	if r.PostFormValue("grant_type") != "urn:ietf:params:oauth:grant-type:token-exchange" {
		f.writeJSON(t, w, map[string]any{
			"access_token": "at-1", "token_type": "DPoP", "expires_in": 900, "refresh_token": "rt-1",
		})
		return
	}
	epoch := r.PostFormValue("twilight_target_epoch")
	f.writeJSON(t, w, map[string]any{
		"access_token":          "capability-" + epoch,
		"token_type":            "DPoP",
		"expires_in":            900,
		"scope":                 auth.CapabilityScope,
		"twilight_slot_id":      r.PostFormValue("twilight_slot_id"),
		"twilight_target_epoch": epoch,
	})
}

// receipt is a §24-shaped enrollment receipt: EdDSA, the exact typ, a
// kid inside the receipt namespace, and claims matching the enrollment.
func (f *fakeAS) receipt(t *testing.T, epoch uint64, drawID string) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{
		"alg": "EdDSA", "typ": auth.ReceiptTyp, "kid": "receipt-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{
		"receipt_version": 1,
		"iss":             f.srv.URL,
		"chain_id":        testChainID,
		"slot_id":         strconv.Itoa(testSlotID),
		"target_epoch":    strconv.FormatUint(epoch, 10),
		"draw_id":         drawID,
	})
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.RawURLEncoding.EncodeToString
	signing := b64(header) + "." + b64(payload)
	return signing + "." + b64(ed25519.Sign(f.receiptKey, []byte(signing)))
}

// driverFixture is one assembled driver plus the two things a test
// inspects: the scope holder the request path would read, and the log.
type driverFixture struct {
	driver *epochDriver
	holder *scope.Holder
	log    *bytes.Buffer
	as     *fakeAS
}

func newDriverFixture(t *testing.T, as *fakeAS, pinned *uint64) *driverFixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := auth.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The enrolled state: a refresh authorization already in custody.
	if err := store.SaveRefreshToken("rt-0"); err != nil {
		t.Fatal(err)
	}
	disc, err := auth.NewDiscoverer(auth.DiscoveryConfig{
		BaseURL: as.srv.URL, ChainID: testChainID, SlotID: testSlotID,
	})
	if err != nil {
		t.Fatal(err)
	}
	oauthClient, err := auth.NewOAuthClient(context.Background(), disc, store)
	if err != nil {
		t.Fatal(err)
	}
	mining := auth.NewMiningClient(disc, oauthClient, store)
	holder := &scope.Holder{}
	buf := &bytes.Buffer{}
	return &driverFixture{
		driver: newEpochDriver(mining, auth.NewCapabilityClient(mining, holder), pinned,
			redact.NewLogger(buf, slog.LevelDebug), func() bool { return true }),
		holder: holder,
		log:    buf,
		as:     as,
	}
}

func (f *driverFixture) tick() { f.driver.tick(context.Background()) }

func (f *driverFixture) lines(needle string) int {
	return strings.Count(f.log.String(), needle)
}

func (f *driverFixture) heldEpoch(t *testing.T) (uint64, bool) {
	t.Helper()
	got := f.holder.Snapshot()
	if got == nil {
		return 0, false
	}
	return got.TargetEpoch, true
}

// The whole point: an epoch nobody configured is discovered, joined and
// held, and the join happens exactly once.
func TestEpochDriverJoinsTheTargetTheASOffers(t *testing.T) {
	as := newFakeAS(t)
	as.set(func(s *asState) { s.epoch, s.joinable = 1042, true })
	f := newDriverFixture(t, as, nil)

	f.tick()

	if n := as.joinCalls.Load(); n != 1 {
		t.Fatalf("join calls = %d, want exactly 1", n)
	}
	epoch, ok := f.heldEpoch(t)
	if !ok || epoch != 1042 {
		t.Fatalf("capability for epoch %d held=%v; the discovered target was not entered", epoch, ok)
	}

	// What the AS says after a join: not joinable any more, and already
	// accepted. The driver must read that as "we are in", not as a
	// reason to try again.
	as.set(func(s *asState) { s.joinable, s.joinStatus = false, auth.JoinAccepted })
	f.tick()
	f.tick()

	if n := as.joinCalls.Load(); n != 1 {
		t.Fatalf("join calls = %d after three ticks: the driver re-joined a target it holds", n)
	}
	if n := as.statusCalls.Load(); n != 1 {
		t.Fatalf("status calls = %d: a remembered join must not be re-checked every minute", n)
	}
	if epoch, ok := f.heldEpoch(t); !ok || epoch != 1042 {
		t.Fatalf("capability lost across ticks: epoch %d held=%v", epoch, ok)
	}
}

// A target this process never joined, that the AS says is already
// accepted (a restart, or the operator's `join` command): ASKED, never
// inferred — joinable=false alone would look identical to "closed
// without us".
func TestEpochDriverDoesNotRejoinAnAlreadyHeldTarget(t *testing.T) {
	as := newFakeAS(t)
	as.set(func(s *asState) {
		s.epoch, s.joinable, s.joinStatus = 1042, false, auth.JoinAlreadyAccepted
	})
	f := newDriverFixture(t, as, nil)

	f.tick()
	f.tick()

	if n := as.joinCalls.Load(); n != 0 {
		t.Fatalf("join calls = %d: an already-accepted target was joined again", n)
	}
	if n := as.statusCalls.Load(); n != 1 {
		t.Fatalf("status calls = %d, want 1: the answer should be remembered after the first ask", n)
	}
	if epoch, ok := f.heldEpoch(t); !ok || epoch != 1042 {
		t.Fatalf("no capability for a target we already hold: epoch %d held=%v", epoch, ok)
	}
}

// Between targets the AS answers 200 with nothing open. That is an
// answer: no join, no capability, and nothing in the log — this is the
// ordinary state around every epoch boundary, and it must not read as a
// fault.
func TestEpochDriverIsQuietWhenNothingIsOpen(t *testing.T) {
	as := newFakeAS(t)
	f := newDriverFixture(t, as, nil)

	f.tick()
	f.tick()

	if as.statusCalls.Load() != 0 || as.joinCalls.Load() != 0 {
		t.Fatalf("NO_OPEN_TARGET provoked work: status=%d join=%d",
			as.statusCalls.Load(), as.joinCalls.Load())
	}
	if _, ok := f.heldEpoch(t); ok {
		t.Fatal("a capability was obtained with no open target")
	}
	if f.log.Len() != 0 {
		t.Fatalf("NO_OPEN_TARGET logged something: %s", f.log.String())
	}
}

// A broken AS must cost one log line and nothing else — not a wedged
// loop, not a line a minute — and the driver must pick the target up
// the moment the AS comes back.
func TestEpochDriverSurvivesAFailingEndpointAndRecovers(t *testing.T) {
	as := newFakeAS(t)
	as.set(func(s *asState) { s.targetStatus = http.StatusServiceUnavailable })
	f := newDriverFixture(t, as, nil)

	for range 5 {
		f.tick()
	}
	if _, ok := f.heldEpoch(t); ok {
		t.Fatal("a capability appeared while the AS was refusing to answer")
	}
	if n := f.lines("current target unavailable"); n != 1 {
		t.Fatalf("five failing ticks logged %d lines; a persistent failure must be said once", n)
	}
	if n := as.currentCalls.Load(); n != 5 {
		t.Fatalf("current-target calls = %d after five ticks: the loop stopped asking", n)
	}

	// The AS returns.
	as.set(func(s *asState) { s.targetStatus, s.epoch, s.joinable = 0, 1042, true })
	f.tick()
	if epoch, ok := f.heldEpoch(t); !ok || epoch != 1042 {
		t.Fatalf("the driver did not recover: epoch %d held=%v", epoch, ok)
	}

	// And it breaks again: a recurrence after a recovery is news.
	as.set(func(s *asState) { s.targetStatus = http.StatusServiceUnavailable })
	f.tick()
	if n := f.lines("current target unavailable"); n != 2 {
		t.Fatalf("a failure recurring after recovery was swallowed (%d lines)", n)
	}
}

// The override: mining.target_epoch names the target and the endpoint is
// never asked. Pinned to epoch 0 on purpose — 0 is a legal epoch, and
// only the absence of the key means "ask the AS".
func TestEpochDriverPinnedEpochSkipsTheQuery(t *testing.T) {
	as := newFakeAS(t)
	as.set(func(s *asState) { s.joinable = true })
	pinned := uint64(0)
	f := newDriverFixture(t, as, &pinned)

	f.tick()

	if n := as.currentCalls.Load(); n != 0 {
		t.Fatalf("current-target was queried %d times despite a pin", n)
	}
	if n := as.joinCalls.Load(); n != 1 {
		t.Fatalf("join calls = %d for the pinned epoch", n)
	}
	epoch, ok := f.heldEpoch(t)
	if !ok || epoch != 0 {
		t.Fatalf("epoch 0 was not honored as a pin: epoch %d held=%v", epoch, ok)
	}
}

// An AS that predates the endpoint: degrade, say so once, and never
// assemble the URL locally to get around it.
func TestEpochDriverDegradesWhenTheASLacksTheTemplate(t *testing.T) {
	as := newFakeAS(t)
	as.set(func(s *asState) { s.advertise, s.epoch, s.joinable = false, 1042, true })
	f := newDriverFixture(t, as, nil)

	f.tick()
	f.tick()
	f.tick()

	if n := as.probes.Load(); n != 0 {
		t.Fatalf("%d request(s) reached a current-target URL the AS never advertised", n)
	}
	if _, ok := f.heldEpoch(t); ok {
		t.Fatal("a capability was obtained without an epoch source")
	}
	if as.statusCalls.Load() != 0 || as.joinCalls.Load() != 0 {
		t.Fatal("the driver acted on an epoch it never learned")
	}
	if n := f.lines("no current-target endpoint"); n != 1 {
		t.Fatalf("the missing endpoint was reported %d times, want exactly 1", n)
	}
	if !strings.Contains(f.log.String(), `"level":"WARN"`) {
		t.Fatalf("an AS the operator must upgrade was not reported at WARN: %s", f.log.String())
	}
}

// The joined ring is fixed-size: a process that runs for months must not
// grow one entry per epoch. Forgetting is safe — the AS is asked again.
func TestJoinedRingIsBounded(t *testing.T) {
	d := newEpochDriver(nil, nil, nil, redact.NewLogger(&bytes.Buffer{}, slog.LevelDebug), nil)
	for epoch := range uint64(joinedMemory * 4) {
		d.remember(epoch)
		if !d.hasJoined(epoch) {
			t.Fatalf("epoch %d was not remembered", epoch)
		}
		if len(d.joined) > joinedMemory {
			t.Fatalf("joined set grew to %d entries", len(d.joined))
		}
	}
	if d.hasJoined(0) {
		t.Fatal("the oldest epoch was never evicted; the ring is not recycling")
	}
	// Re-remembering the newest must not consume a slot.
	newest := uint64(joinedMemory*4 - 1)
	before := len(d.joined)
	d.remember(newest)
	if len(d.joined) != before || !d.hasJoined(newest) {
		t.Fatalf("re-remembering churned the ring: %d → %d", before, len(d.joined))
	}
}

// The de-duplicated log is keyed by site, so two sites failing in the
// same tick do not take turns overwriting each other's memory and log
// forever.
func TestDegradationNotesAreDeduplicatedPerSite(t *testing.T) {
	buf := &bytes.Buffer{}
	d := newEpochDriver(nil, nil, nil, redact.NewLogger(buf, slog.LevelDebug), nil)
	ctx := context.Background()
	for range 3 {
		d.once(ctx, slog.LevelDebug, noteTarget, "first", fmt.Errorf("a"))
		d.once(ctx, slog.LevelDebug, noteCapability, "second", fmt.Errorf("b"))
	}
	if n := strings.Count(buf.String(), `"msg":"first"`); n != 1 {
		t.Fatalf("first site logged %d times", n)
	}
	if n := strings.Count(buf.String(), `"msg":"second"`); n != 1 {
		t.Fatalf("second site logged %d times", n)
	}
	if len(d.notes) != 2 {
		t.Fatalf("notes holds %d entries; it is keyed by a closed set of sites", len(d.notes))
	}
	// A different failure at the same site is still news.
	d.once(ctx, slog.LevelDebug, noteTarget, "first", fmt.Errorf("c"))
	if n := strings.Count(buf.String(), `"msg":"first"`); n != 2 {
		t.Fatalf("a changed failure was swallowed (%d lines)", n)
	}
	// Shutdown is not a degradation.
	d.once(ctx, slog.LevelDebug, noteJoin, "canceled", context.Canceled)
	if strings.Contains(buf.String(), "canceled") {
		t.Fatal("context cancellation was logged as a degradation")
	}
}

// A credential failure speaks at Warn once the installation is enrolled,
// and at Debug before.
//
// The Debug this replaces was justified for the pre-enrollment state — it
// fails on every tick by design there — and that justification does not
// survive enrollment. The Slot 3 run of 2026-08-27 lost ninety minutes to
// the difference: a daemon whose credential family had been revoked kept
// forwarding searches, answered /healthz, and logged one line.
func TestCredentialFailureLevelFollowsEnrollment(t *testing.T) {
	for _, tc := range []struct {
		name     string
		enrolled bool
		want     string
	}{
		{"not enrolled — the ordinary startup state", false, `"level":"DEBUG"`},
		{"enrolled — a participant who expects to be earning", true, `"level":"WARN"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := &bytes.Buffer{}
			d := newEpochDriver(nil, nil, nil, redact.NewLogger(buf, slog.LevelDebug),
				func() bool { return tc.enrolled })
			d.once(context.Background(), d.credentialLevel(), noteJoin, "mining: join refused", fmt.Errorf("invalid_grant"))
			if !strings.Contains(buf.String(), tc.want) {
				t.Fatalf("logged %q, want %s", buf.String(), tc.want)
			}
		})
	}
}

// A persisting failure says so again, on a slow cadence.
//
// Once-and-never-again was the old behavior and it is indistinguishable
// from a healthy log after the first minute. The loop ticks every minute,
// so the suppression window is the only thing standing between one line an
// hour and sixty.
func TestAPersistingFailureRepeatsOnASlowCadence(t *testing.T) {
	buf := &bytes.Buffer{}
	d := newEpochDriver(nil, nil, nil, redact.NewLogger(buf, slog.LevelWarn), func() bool { return true })
	d.repeat = 50 * time.Millisecond
	ctx := context.Background()

	for range 5 {
		d.once(ctx, slog.LevelWarn, noteJoin, "mining: join refused", fmt.Errorf("invalid_grant"))
	}
	if n := strings.Count(buf.String(), `"msg":"mining: join refused"`); n != 1 {
		t.Fatalf("the same failure logged %d times inside the suppression window, want 1", n)
	}

	time.Sleep(60 * time.Millisecond)
	d.once(ctx, slog.LevelWarn, noteJoin, "mining: join refused", fmt.Errorf("invalid_grant"))
	if n := strings.Count(buf.String(), `"msg":"mining: join refused"`); n != 2 {
		t.Fatalf("the failure logged %d times after the window elapsed, want 2 — a permanent failure that says nothing is what this exists to prevent", n)
	}
}

// The health summary fires on the JOIN path, with nothing queued.
//
// This is the defect itself. Queued and ConsecutiveFailures are written by
// the delivery path, so a daemon that cannot join an epoch leaves both at
// zero — and the old summary, gated on both being positive, could not fire
// on this failure however long it lasted. Everything an operator could see
// said healthy.
