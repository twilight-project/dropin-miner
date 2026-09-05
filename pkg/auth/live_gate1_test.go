package auth

// GATE-1 live legs: the REAL proxy OAuth client against the REAL AS.
// Requires TOKENDROP_LIVE_AS_URL and TOKENDROP_LIVE_TEST_CONTROL_URL
// (the gate runner provides both; everything else skips). The test
// drives the participant's browser role itself: it POSTs credentials to
// the authorization endpoint and follows the redirect into the proxy's
// loopback listener.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/mining/scope"
)

type liveEnv struct {
	asURL    string
	username string
	password string
	client   *OAuthClient
	store    *Store
}

func newLiveEnv(t *testing.T) *liveEnv {
	t.Helper()
	asURL := os.Getenv("TOKENDROP_LIVE_AS_URL")
	tcURL := os.Getenv("TOKENDROP_LIVE_TEST_CONTROL_URL")
	if asURL == "" || tcURL == "" {
		t.Skip("TOKENDROP_LIVE_AS_URL / TOKENDROP_LIVE_TEST_CONTROL_URL not set (GATE-1 live legs)")
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	env := &liveEnv{
		asURL:    asURL,
		username: "gate1-" + hex.EncodeToString(suffix),
		password: "gate1-password-1",
	}
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, env.username, env.password)
	resp, err := http.Post(tcURL+"/test/identity", "application/json", strings.NewReader(body)) // #nosec G704 -- operator-supplied test-control URL
	if err != nil {
		t.Fatalf("provision identity: %v", err)
	}
	defer resp.Body.Close()
	var prov struct {
		ParticipantID string `json:"participant_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&prov); err != nil || prov.ParticipantID == "" {
		t.Fatalf("provisioning failed: %v %+v (status %d)", err, prov, resp.StatusCode)
	}

	store, err := OpenStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewDiscoverer(DiscoveryConfig{BaseURL: asURL, ChainID: "twilight-1", SlotID: 7})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewOAuthClient(context.Background(), d, store)
	if err != nil {
		t.Fatal(err)
	}
	env.client = client
	env.store = store
	return env
}

// approveInBrowser plays the participant: POST credentials + approve to
// the authorization URL and follow the redirect chain into the proxy's
// loopback callback. Runs on a helper goroutine, so failures use
// t.Error (the main goroutine's Wait deadline converts them to a Fatal).
func (e *liveEnv) approveInBrowser(t *testing.T, authorizeURL string) {
	resp, err := http.PostForm(authorizeURL, url.Values{ // #nosec G107 -- authorize URL built by our own client
		"username": {e.username},
		"password": {e.password},
		"action":   {"approve"},
	})
	if err != nil {
		t.Errorf("browser approval: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("browser approval final status %d", resp.StatusCode)
	}
}

// GATE-1: the complete proxy↔AS OAuth lifecycle over real HTTP.
func TestLiveGate1OAuthLifecycle(t *testing.T) {
	env := newLiveEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// AUTH: interactive code flow (PKCE + DPoP + loopback redirect).
	pending, err := env.client.StartAuthorization(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go env.approveInBrowser(t, pending.URL)
	tok, err := pending.Wait(ctx)
	if err != nil {
		t.Fatalf("code flow: %v", err)
	}
	if !strings.EqualFold(tok.TokenType, "DPoP") {
		t.Fatalf("token_type = %q, want DPoP", tok.TokenType)
	}
	if _, ok, _ := env.store.LoadRefreshToken(); !ok {
		t.Fatal("refresh authorization not persisted after code flow")
	}

	// AUTH: refresh rotates against the real AS.
	tok2, err := env.client.Refresh(ctx)
	if err != nil {
		t.Fatalf("live refresh: %v", err)
	}
	if tok2.AccessToken == tok.AccessToken {
		t.Fatal("refresh did not rotate the access token")
	}

	// AUTH: a different installation key cannot use this grant. A thief
	// with the refresh token but a fresh key must be refused.
	thiefStore, err := OpenStore(filepath.Join(t.TempDir(), "thief"))
	if err != nil {
		t.Fatal(err)
	}
	stolen, _, err := env.store.LoadRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := thiefStore.SaveRefreshToken(stolen); err != nil {
		t.Fatal(err)
	}
	d, err := NewDiscoverer(DiscoveryConfig{BaseURL: env.asURL, ChainID: "twilight-1", SlotID: 7})
	if err != nil {
		t.Fatal(err)
	}
	thief, err := NewOAuthClient(ctx, d, thiefStore)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := thief.Refresh(ctx); err == nil {
		t.Fatal("stolen refresh token accepted under a different DPoP key")
	}

	// AUTH: revocation from the rightful installation, then refresh dies.
	if err := env.client.Revoke(ctx); err != nil {
		t.Fatalf("revocation: %v", err)
	}
	if err := env.store.SaveRefreshToken(stolen); err != nil {
		t.Fatal(err) // restore the (now revoked) token to prove refusal
	}
	if _, err := env.client.Refresh(ctx); err == nil {
		t.Fatal("revoked refresh authorization still refreshes")
	}
}

// GATE-1: headless bootstrap — the device grant end to end.
func TestLiveGate1DeviceFlow(t *testing.T) {
	env := newLiveEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	da, err := env.client.StartDeviceAuthorization(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if da.UserCode == "" || da.VerificationURI == "" {
		t.Fatalf("device authorization incomplete: %+v", da)
	}
	// Participant approves on their other device.
	resp, err := http.PostForm(env.asURL+"/device", url.Values{
		"user_code": {da.UserCode},
		"username":  {env.username},
		"password":  {env.password},
		"action":    {"approve"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("device approval status %d", resp.StatusCode)
	}

	tok, err := env.client.WaitForDeviceApproval(ctx, da)
	if err != nil {
		t.Fatalf("device poll: %v", err)
	}
	if !strings.EqualFold(tok.TokenType, "DPoP") || tok.RefreshToken == "" {
		t.Fatalf("device tokens malformed: type=%q rt-empty=%v", tok.TokenType, tok.RefreshToken == "")
	}
}

// GATE-2 live legs: real proxy joins a real epoch on the real AS and
// verifies the receipt against the published keys.
func TestLiveGate2JoinEpoch(t *testing.T) {
	env := newLiveEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Authorize this installation first (code flow).
	pending, err := env.client.StartAuthorization(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go env.approveInBrowser(t, pending.URL)
	if _, err := pending.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	// Prepare a fresh target via test-control (unique epoch per run).
	epochNum := 100000 + int(time.Now().UnixNano()%100000)
	body := fmt.Sprintf(`{"slot_id":7,"target_epoch":%d,"enrollment_close_height":900000,"mode":"PROTOCOL_SELECTION"}`, epochNum)
	tcTarget := os.Getenv("TOKENDROP_LIVE_TEST_CONTROL_URL") + "/test/chain/target"
	resp, err := http.Post(tcTarget, "application/json", strings.NewReader(body)) // #nosec G107,G704 -- operator-supplied loopback test-control URL
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("prepare target: %v (status %v)", err, resp.Status)
	}
	_ = resp.Body.Close()

	d, err := NewDiscoverer(DiscoveryConfig{BaseURL: env.asURL, ChainID: "twilight-1", SlotID: 7})
	if err != nil {
		t.Fatal(err)
	}
	mining := NewMiningClient(d, env.client, env.store)

	// Status first: joinable must be true.
	st, err := mining.Status(ctx, uint64(epochNum))
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Joinable || st.JoinStatus != "NOT_JOINED" {
		t.Fatalf("pre-join status: %+v", st)
	}

	// Join: derives the draw ID, verifies + stores the receipt.
	res, err := mining.JoinEpoch(ctx, uint64(epochNum))
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if res.Status != "ACCEPTED" || len(res.DrawID) != 64 {
		t.Fatalf("join result: %+v", res)
	}
	// Idempotent retry returns the same verified bytes.
	res2, err := mining.JoinEpoch(ctx, uint64(epochNum))
	if err != nil || res2.Status != "ALREADY_ACCEPTED" || res2.Receipt != res.Receipt {
		t.Fatalf("replay: %v %+v", err, res2)
	}
	// Receipt persisted.
	stored, ok, err := env.store.LoadReceipt(7, uint64(epochNum))
	if err != nil || !ok || stored != res.Receipt {
		t.Fatalf("receipt persistence: ok=%v err=%v", ok, err)
	}
	// Status reflects the enrollment.
	st, err = mining.Status(ctx, uint64(epochNum))
	if err != nil || st.JoinStatus != "ACCEPTED" || st.Joinable {
		t.Fatalf("post-join status: %v %+v", err, st)
	}
	t.Logf("GATE-2 live: enrolled epoch %d draw %s.., receipt verified + stored", epochNum, res.DrawID[:16])
}

// GATE-3 live legs: the real proxy trades its normal authorization for
// a Participation Capability against the real AS and publishes it into
// the request-path scope holder.
func TestLiveGate3Capability(t *testing.T) {
	env := newLiveEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pending, err := env.client.StartAuthorization(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go env.approveInBrowser(t, pending.URL)
	if _, err := pending.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	// Trusted-mode target (the V1 participation policy authorizes all
	// accepted enrollments).
	epochNum := uint64(200000 + time.Now().UnixNano()%100000)
	body := fmt.Sprintf(`{"slot_id":7,"target_epoch":%d,"mode":"TRUSTED_AS_DISTRIBUTION"}`, epochNum)
	tcTarget := os.Getenv("TOKENDROP_LIVE_TEST_CONTROL_URL") + "/test/chain/target"
	resp, err := http.Post(tcTarget, "application/json", strings.NewReader(body)) // #nosec G107,G704 -- operator-supplied loopback test-control URL
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("prepare target: %v (status %v)", err, resp.Status)
	}
	_ = resp.Body.Close()

	d, err := NewDiscoverer(DiscoveryConfig{BaseURL: env.asURL, ChainID: "twilight-1", SlotID: 7})
	if err != nil {
		t.Fatal(err)
	}
	mining := NewMiningClient(d, env.client, env.store)

	// A capability requires an accepted enrollment first.
	var holder scope.Holder
	caps := NewCapabilityClient(mining, &holder)
	if _, err := caps.Ensure(ctx, epochNum); err == nil {
		t.Fatal("capability issued without an enrollment")
	}

	if _, err := mining.JoinEpoch(ctx, epochNum); err != nil {
		t.Fatalf("join: %v", err)
	}

	got, err := caps.Ensure(ctx, epochNum)
	if err != nil {
		t.Fatalf("capability exchange: %v", err)
	}
	if got.Capability == "" || got.TargetEpoch != epochNum || got.SlotID != 7 {
		t.Fatalf("capability context wrong: %+v", got)
	}
	if !got.Valid(time.Now()) {
		t.Fatal("fresh capability is not valid")
	}
	// The request path sees it via a single atomic snapshot.
	snap := holder.Snapshot()
	if snap == nil || snap.Capability != got.Capability {
		t.Fatalf("scope holder not published: %+v", snap)
	}
	// Cached until near expiry: no needless re-exchange.
	again, err := caps.Ensure(ctx, epochNum)
	if err != nil || again.Capability != got.Capability {
		t.Fatalf("cache miss on a fresh capability: %v", err)
	}
	t.Logf("GATE-3 live: capability for epoch %d, expires in %s", epochNum, time.Until(got.ExpiresAt).Round(time.Second))
}

// GATE-4 live legs: provider-verification binding lifecycle driven by
// the real proxy client against the real AS. Uses the AS's fixture
// checker (no live OpenRouter): the zero-spend policy is proven, the
// live-provider legs belong to GATE-6.
func TestLiveGate4ProviderVerification(t *testing.T) {
	env := newLiveEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pending, err := env.client.StartAuthorization(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go env.approveInBrowser(t, pending.URL)
	if _, err := pending.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	d, err := NewDiscoverer(DiscoveryConfig{BaseURL: env.asURL, ChainID: "twilight-1", SlotID: 7})
	if err != nil {
		t.Fatal(err)
	}
	mining := NewMiningClient(d, env.client, env.store)

	// Nothing bound yet.
	status, err := mining.ProviderStatus(ctx)
	if err != nil {
		t.Fatalf("provider status: %v", err)
	}
	if status.Status != ProviderStatusNotConfigured {
		t.Fatalf("expected NOT_CONFIGURED, got %+v", status)
	}

	// A spendable key must be refused by the AS policy.
	if _, err := mining.RegisterProviderCredential(ctx, os.Getenv("TOKENDROP_LIVE_SPENDABLE_KEY")); err == nil {
		t.Fatal("a spendable verification key was accepted")
	}

	// The zero-spend key binds.
	zeroSpend := os.Getenv("TOKENDROP_LIVE_ZERO_SPEND_KEY")
	if zeroSpend == "" {
		t.Skip("TOKENDROP_LIVE_ZERO_SPEND_KEY not set")
	}
	binding, err := mining.RegisterProviderCredential(ctx, zeroSpend)
	if err != nil {
		t.Fatalf("registration: %v", err)
	}
	if binding.Status != ProviderStatusReady || binding.KeyFingerprint == "" {
		t.Fatalf("binding: %+v", binding)
	}
	if strings.Contains(fmt.Sprintf("%+v", binding), zeroSpend) {
		t.Fatal("binding document echoed the key")
	}

	status, err = mining.ProviderStatus(ctx)
	if err != nil || status.Status != ProviderStatusReady {
		t.Fatalf("post-registration status: %v %+v", err, status)
	}

	if err := mining.RemoveProviderCredential(ctx); err != nil {
		t.Fatalf("removal: %v", err)
	}
	status, err = mining.ProviderStatus(ctx)
	if err != nil || status.Status != ProviderStatusNotConfigured {
		t.Fatalf("post-removal status: %v %+v", err, status)
	}
	t.Logf("GATE-4 live: bind/replace-refusal/status/remove proven; %s", RevokeAtProviderNotice)
}
