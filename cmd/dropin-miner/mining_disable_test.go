package main

// `mining disable` (design §5.5, and the follow-on plan against
// main@c2fa625 §5.5: "stop mining here" — the client's own decision,
// separate from the AS family and the platform's standing scope grant).
// These tests prove the brick closed: LastEnrollmentSlot being cleared
// used to leave a disabled installation permanently unable to re-enroll
// because nothing had actually asked it to stop, only to never have
// started. Now something does, and enabling again is the existing
// command minting a fresh family, not a silent no-op.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/twilight-project/dropin-miner/pkg/auth"
)

// fullyEnrolledInstall builds an installation all the way through a real
// AS enrollment — the same two-run register/claim/enroll shape
// TestConnectCaseAccountExistsSearchAndMining uses — so these tests
// start from a state that is actually enrolled (a live refresh token, a
// redeemed assertion) rather than one that only claims to be.
func fullyEnrolledInstall(t *testing.T) (cfgPath, stateDir string, platform *stubPlatform, as *stubOnboardingAS) {
	t.Helper()
	withShortConnectTimings(t)
	platform = newStubPlatform(t)
	as = newStubAS(t)
	cfgPath, stateDir = connectConfig(t, platform.srv.URL, as.srv.URL)

	if code, _, _ := runConnect(t, cfgPath, nil, "-mining"); code != exitOK {
		t.Fatal("first connect failed")
	}
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SavePayoutAddress("twilight1uwew6p63453wm0znz723lrneuls4xy29swp89n"); err != nil {
		t.Fatal(err)
	}
	platform.claim("credits", "mining")

	code, out, errOut := runConnect(t, cfgPath, nil)
	if code != exitOK || !strings.Contains(out, "enrolled for mining") {
		t.Fatalf("second connect exited %d: stdout=%s stderr=%s", code, out, errOut)
	}
	if _, ok, _ := store.LoadRefreshToken(); !ok {
		t.Fatal("setup: not actually enrolled at the AS")
	}
	return cfgPath, stateDir, platform, as
}

func runMiningDisable(t *testing.T, cfgPath string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = cmdMining([]string{"disable", "-config", cfgPath}, &bytes.Buffer{}, &out, &errOut, noEnv)
	return code, out.String(), errOut.String()
}

func runMiningEnable(t *testing.T, cfgPath string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = cmdMining([]string{"enable", "-config", cfgPath}, &bytes.Buffer{}, &out, &errOut, noEnv)
	return code, out.String(), errOut.String()
}

// The brick, closed: disable stops mining, and enable — the existing
// command, no new path — mints a fresh family under the same
// installation. LastEnrollmentSlot being empty must not be
// distinguishable, to the re-enroll path, from "never enrolled at all".
func TestMiningDisableThenEnableClosesTheBrick(t *testing.T) {
	cfgPath, stateDir, _, as := fullyEnrolledInstall(t)
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runMiningDisable(t, cfgPath)
	if code != exitOK {
		t.Fatalf("disable exited %d: stdout=%s stderr=%s", code, out, errOut)
	}
	if !strings.Contains(out, "mining here: stopped") || !strings.Contains(out, "platform authorization: still granted") {
		t.Fatalf("disable did not print the permanent pair: %q", out)
	}
	if enabled, ok, _ := store.LoadMiningEnabled(); !ok || enabled {
		t.Fatalf("decision not persisted as off: enabled=%v ok=%v", enabled, ok)
	}
	reg, ok := loadAgent(t, stateDir)
	if !ok || reg.LastEnrollmentSlot != "" || reg.LastEnrollmentAt != "" {
		t.Fatalf("enrollment record not cleared: %+v", reg)
	}
	if _, ok, _ := store.LoadRefreshToken(); ok {
		t.Fatal("refresh token still on file after a successful revoke")
	}
	if got := as.revokeCallCount(); got != 1 {
		t.Fatalf("revoke calls = %d, want 1", got)
	}
	redemptionsBefore := as.assertionRedemptionCount()

	code, out, errOut = runMiningEnable(t, cfgPath)
	if code != exitOK {
		t.Fatalf("enable exited %d: stdout=%s stderr=%s", code, out, errOut)
	}
	reg, ok = loadAgent(t, stateDir)
	if !ok || reg.LastEnrollmentSlot != "twilight-slot-3" {
		t.Fatalf("re-enable did not re-enroll: %+v ok=%v", reg, ok)
	}
	if enabled, ok, _ := store.LoadMiningEnabled(); !ok || !enabled {
		t.Fatalf("decision not persisted as on after re-enable: enabled=%v ok=%v", enabled, ok)
	}
	if _, ok, _ := store.LoadRefreshToken(); !ok {
		t.Fatal("re-enable did not mint a fresh refresh token")
	}
	if got := as.assertionRedemptionCount(); got != redemptionsBefore+1 {
		t.Fatalf("assertion redemptions = %d, want %d (one fresh enrollment)", got, redemptionsBefore+1)
	}
}

// Idempotent throughout: nothing to disable is reported plainly, never
// as an error, whether there was never a registration, a registration
// that never enrolled, or a previous disable that already settled it.
func TestMiningDisableIsIdempotentWhenNothingIsEnrolled(t *testing.T) {
	t.Run("no registration at all", func(t *testing.T) {
		platform := newStubPlatform(t)
		cfgPath, _ := connectConfig(t, platform.srv.URL, "")
		code, out, errOut := runMiningDisable(t, cfgPath)
		if code != exitOK {
			t.Fatalf("code=%d stderr=%s", code, errOut)
		}
		if !strings.Contains(out, "nothing to disable") {
			t.Fatalf("got %q", out)
		}
	})

	t.Run("claimed but never enrolled, no AS configured", func(t *testing.T) {
		withShortConnectTimings(t)
		platform := newStubPlatform(t)
		cfgPath, _ := connectConfig(t, platform.srv.URL, "")
		if code, _, _ := runConnect(t, cfgPath, nil); code != exitOK {
			t.Fatal("first connect failed")
		}
		platform.claim("credits", "mining")
		if code, _, _ := runConnect(t, cfgPath, nil); code != exitOK {
			t.Fatal("second connect failed")
		}
		code, out, errOut := runMiningDisable(t, cfgPath)
		if code != exitOK {
			t.Fatalf("code=%d stderr=%s", code, errOut)
		}
		if !strings.Contains(out, "mining is not enabled here") {
			t.Fatalf("got %q", out)
		}
	})

	t.Run("second disable after a clean first one", func(t *testing.T) {
		cfgPath, _, _, _ := fullyEnrolledInstall(t)
		if code, _, errOut := runMiningDisable(t, cfgPath); code != exitOK {
			t.Fatalf("first disable exited %d: %s", code, errOut)
		}
		code, out, errOut := runMiningDisable(t, cfgPath)
		if code != exitOK {
			t.Fatalf("second disable exited %d: %s", code, errOut)
		}
		if !strings.Contains(out, "mining is not enabled here") {
			t.Fatalf("second disable did not report the settled state: %q", out)
		}
	})
}

// A resume must never re-enroll a disabled installation, even though the
// stub's granted scope never changed — the platform's standing
// authorization and the client's own decision are two different facts,
// and only the second one is what a resume is required to honor.
func TestResumeDoesNotReEnrollAfterDisableEvenWithScopeStillGranted(t *testing.T) {
	cfgPath, stateDir, _, as := fullyEnrolledInstall(t)

	if code, _, errOut := runMiningDisable(t, cfgPath); code != exitOK {
		t.Fatalf("disable exited %d: %s", code, errOut)
	}
	redemptionsBefore := as.assertionRedemptionCount()

	if code, out, errOut := runConnect(t, cfgPath, nil, "-resume"); code != exitOK {
		t.Fatalf("resume exited %d: stdout=%s stderr=%s", code, out, errOut)
	}
	if got := as.assertionRedemptionCount(); got != redemptionsBefore {
		t.Fatalf("resume re-enrolled after disable: redemptions %d -> %d", redemptionsBefore, got)
	}
	reg, ok := loadAgent(t, stateDir)
	if !ok || reg.LastEnrollmentSlot != "" {
		t.Fatalf("resume repopulated the enrollment record after disable: %+v", reg)
	}
}

// The stop never depends on the network: a participant who wants to
// stop while the AS is unreachable must still be able to. The AS-side
// revocation is deferred, not skipped — a revoke_pending marker survives
// for the next flush or resume to retry, and clears once one succeeds.
func TestMiningDisableWithASUnreachableStopsLocallyAndRetriesOnResume(t *testing.T) {
	cfgPath, stateDir, _, as := fullyEnrolledInstall(t)
	as.setRevokeFails(true)

	code, out, errOut := runMiningDisable(t, cfgPath)
	if code != exitOK {
		t.Fatalf("disable exited %d: %s", code, errOut)
	}
	if !strings.Contains(out, "stopped here; the AS will be told on the next run") {
		t.Fatalf("did not report the deferred revoke: %q", out)
	}
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if pending, _ := store.LoadRevokePending(); !pending {
		t.Fatal("revoke_pending marker was not set")
	}
	if enabled, ok, _ := store.LoadMiningEnabled(); !ok || enabled {
		t.Fatal("the local stop did not happen despite the AS being unreachable")
	}
	if _, ok, _ := store.LoadRefreshToken(); !ok {
		t.Fatal("the refresh token was deleted even though the AS never confirmed the revoke")
	}

	as.setRevokeFails(false)
	if code, out, errOut := runConnect(t, cfgPath, nil, "-resume"); code != exitOK {
		t.Fatalf("resume exited %d: stdout=%s stderr=%s", code, out, errOut)
	}
	if pending, _ := store.LoadRevokePending(); pending {
		t.Fatal("revoke_pending marker was not cleared after the resume's retry succeeded")
	}
	if got := as.revokeCallCount(); got != 2 {
		t.Fatalf("revoke calls = %d, want 2 (the failed disable attempt + the resume's retry)", got)
	}
}

// The flush's own retry, symmetric with the resume's: named alongside
// it in design item 2.3 ("the resume and the flush retry"), and it is
// one of the five gates design item 1 requires reads mining_decision.json
// and nothing else — a disabled installation's flush must do none of
// the join/promote/submit work, not just skip re-enrolling.
func TestFlushRetriesAPendingRevokeAndSkipsTheMiningPlaneWhileDisabled(t *testing.T) {
	cfgPath, stateDir, _, as := fullyEnrolledInstall(t)
	as.setRevokeFails(true)
	if code, _, errOut := runMiningDisable(t, cfgPath); code != exitOK {
		t.Fatalf("disable exited %d: %s", code, errOut)
	}
	as.setRevokeFails(false)

	cfg, _, err := loadConfig(cfgPath, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	var fout, ferrOut bytes.Buffer
	rep, code := runFlush(context.Background(), cfg, cfgPath, false, &fout, &ferrOut)
	if code != exitOK {
		t.Fatalf("flush exited %d: %s", code, ferrOut.String())
	}
	if !strings.Contains(fout.String(), "mining is stopped here") {
		t.Fatalf("flush did not report the stopped state: %q", fout.String())
	}
	if rep.Promoted != 0 || rep.Delivered != 0 {
		t.Fatalf("flush did the mining plane's work while disabled: %+v", rep)
	}
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if pending, _ := store.LoadRevokePending(); pending {
		t.Fatal("the flush did not clear the pending revoke marker")
	}
	if got := as.revokeCallCount(); got != 2 {
		t.Fatalf("revoke calls = %d, want 2 (the failed disable attempt + the flush's retry)", got)
	}
}

// A stale revoke_pending marker must not survive a re-enable: if it did,
// the next resume or flush would call Revoke against the FRESH family
// mining enable just minted — the danger auth.Store.ClearRevokePending's
// own comment names — accidentally un-mining a participant who just
// re-enabled.
func TestMiningEnableClearsAStaleRevokePendingMarkerSoAFreshFamilySurvives(t *testing.T) {
	cfgPath, stateDir, _, as := fullyEnrolledInstall(t)
	as.setRevokeFails(true)
	if code, _, errOut := runMiningDisable(t, cfgPath); code != exitOK {
		t.Fatalf("disable exited %d: %s", code, errOut)
	}
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if pending, _ := store.LoadRevokePending(); !pending {
		t.Fatal("setup: the marker was not actually left pending")
	}
	as.setRevokeFails(false) // the AS recovers, but nothing should call Revoke against the fresh family below

	if code, _, errOut := runMiningEnable(t, cfgPath); code != exitOK {
		t.Fatalf("enable exited %d: %s", code, errOut)
	}
	if _, ok, _ := store.LoadRefreshToken(); !ok {
		t.Fatal("re-enable did not mint a fresh refresh token")
	}
	revokesBeforeResume := as.revokeCallCount()

	if code, out, errOut := runConnect(t, cfgPath, nil, "-resume"); code != exitOK {
		t.Fatalf("resume exited %d: stdout=%s stderr=%s", code, out, errOut)
	}
	if got := as.revokeCallCount(); got != revokesBeforeResume {
		t.Fatalf("the resume called revoke again (count %d -> %d): a stale marker was retried against the new family",
			revokesBeforeResume, got)
	}
	if _, ok, _ := store.LoadRefreshToken(); !ok {
		t.Fatal("the fresh family was revoked by a stale revoke_pending marker surviving re-enable")
	}
}

// status shows the permanent pair the whole time mining is stopped but
// the platform scope survives — not only in disable's own one-time
// output.
func TestStatusShowsThePermanentPairAfterDisable(t *testing.T) {
	cfgPath, _, _, _ := fullyEnrolledInstall(t)
	if code, _, errOut := runMiningDisable(t, cfgPath); code != exitOK {
		t.Fatalf("disable exited %d: %s", code, errOut)
	}
	// The return value (whether cmdStatus should attempt the AS-facing
	// report below) is not this test's concern — only the identity block
	// printAgentIdentityStatus prints unconditionally, which is where the
	// permanent pair lives.
	var out, errOut bytes.Buffer
	_ = printAgentIdentityStatus([]string{"-config", cfgPath}, &out, &errOut, noEnv)
	if !strings.Contains(out.String(), "mining here: stopped") || !strings.Contains(out.String(), "platform authorization: still granted") {
		t.Fatalf("status did not show the permanent pair: %q", out.String())
	}
}
