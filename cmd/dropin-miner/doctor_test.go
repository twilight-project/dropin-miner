package main

// `doctor`. The interesting property is not that a healthy installation
// reports five OKs — it is that an unhealthy or half-answered one never
// reports a fact it did not establish. "No payout address is in force" is a
// claim about a participant's setup; making it from a run that never reached
// the AS is the specific failure these tests enumerate against.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/twilight-project/dropin-miner/pkg/auth"
	"github.com/twilight-project/dropin-miner/pkg/config"
	"github.com/twilight-project/dropin-miner/pkg/wire"
)

// stubAS answers whatever a case needs it to. internal/auth's own tests
// cover the real client against a fake AS at the wire level; what is under
// test here is what the report does with each answer.
type stubAS struct {
	doc      *wire.DiscoveryDocument
	docErr   error
	target   *auth.MiningTarget
	targetEr error
	status   *auth.EpochStatus
	statusEr error
	standing *auth.PayoutStanding
	standEr  error
	activity *auth.EpochActivity
	actEr    error

	calls []string
}

func (s *stubAS) ServiceDocument(context.Context) (*wire.DiscoveryDocument, error) {
	s.calls = append(s.calls, "document")
	return s.doc, s.docErr
}

func (s *stubAS) CurrentTarget(context.Context) (*auth.MiningTarget, error) {
	s.calls = append(s.calls, "current-target")
	return s.target, s.targetEr
}

func (s *stubAS) Status(context.Context, uint64) (*auth.EpochStatus, error) {
	s.calls = append(s.calls, "status")
	return s.status, s.statusEr
}

func (s *stubAS) PayoutStanding(context.Context) (*auth.PayoutStanding, error) {
	s.calls = append(s.calls, "payout")
	return s.standing, s.standEr
}

func (s *stubAS) EpochActivity(context.Context, uint64) (*auth.EpochActivity, error) {
	s.calls = append(s.calls, "activity")
	return s.activity, s.actEr
}

var errASDown = errors.New("auth: discovery fetch: no such host")

func verdictOf(checks []doctorCheck, name string) doctorCheck {
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	return doctorCheck{Name: name, Verdict: "MISSING"}
}

// A healthy installation: every input positive, every verdict OK.
func healthyFacts() doctorFacts {
	return doctorFacts{
		ASBaseURL:  "https://as.example.com",
		ChainID:    "twilight-1",
		SlotID:     7,
		Doc:        &wire.DiscoveryDocument{ChainID: "twilight-1", SlotID: "7"},
		Epoch:      1042,
		EpochKnown: true,
		Status:     &auth.EpochStatus{JoinStatus: auth.JoinAccepted, Phase: "ACTIVE"},
		Standing: &auth.PayoutStanding{Active: &auth.PayoutDeclaration{
			Address: "twilight1abc", CanonicalAddress: "twilight1abc", Effective: true, Status: "ACTIVE",
		}},
		Activity:   &auth.EpochActivity{VerifiedActivity: true, VerifiedObservationCount: 4},
		HasRefresh: true,
	}
}

func TestAHealthyInstallationIsFiveOKs(t *testing.T) {
	for _, c := range assembleDoctor(healthyFacts()) {
		if c.Verdict != verdictOK {
			t.Errorf("%s = %s (%s); a healthy installation should be OK", c.Name, c.Verdict, c.Detail)
		}
		if c.Fix != "" {
			t.Errorf("%s is OK and still offers a fix: %q", c.Name, c.Fix)
		}
	}
}

// The one that matters. With discovery failed, everything the AS owns is
// UNKNOWN — never NO. A NO is a statement about the participant's setup, and
// there is no evidence for one here.
func TestNothingIsClaimedAboutTheASWhenTheASDidNotAnswer(t *testing.T) {
	f := healthyFacts()
	f.DocErr = errASDown
	f.EpochErr, f.StatusErr, f.StandingErr, f.ActivityErr = errASDown, errASDown, errASDown, errASDown
	// The answers are still present in the struct, which is the point: even
	// with stale positives sitting there, a failed discovery must decide.
	checks := assembleDoctor(f)

	if got := verdictOf(checks, "authorization server"); got.Verdict != verdictNo {
		t.Errorf("the AS line is %s; an unreachable AS is a fact, not an unknown", got.Verdict)
	}
	for _, name := range []string{"joined this epoch", "payout address", "earning"} {
		got := verdictOf(checks, name)
		if got.Verdict != verdictUnknown {
			t.Errorf("%s = %s (%s); want UNKNOWN when the AS was never reached", name, got.Verdict, got.Detail)
		}
		if strings.Contains(got.Detail, "no address is in force") ||
			strings.Contains(got.Detail, "is not in it") {
			t.Errorf("%s asserts a fact it did not establish: %q", name, got.Detail)
		}
	}
}

// The local half still answers. A stored authorization and a recorded
// enrollment are facts about this disk, and an unreachable AS does not
// unmake them.
func TestTheLocalHalfIsReportedWhenTheASIsDown(t *testing.T) {
	f := healthyFacts()
	f.DocErr = errASDown
	f.EpochErr, f.StatusErr, f.StandingErr, f.ActivityErr = errASDown, errASDown, errASDown, errASDown
	f.HasEnrollment, f.LocalSlot, f.LocalEpoch = true, 7, 1041

	checks := assembleDoctor(f)
	enrolled := verdictOf(checks, "enrolled")
	if enrolled.Verdict != verdictOK {
		t.Errorf("enrolled = %s; a stored authorization is a local fact", enrolled.Verdict)
	}
	if !strings.Contains(enrolled.Detail, "locally") {
		t.Errorf("the enrolled line does not say where the answer came from: %q", enrolled.Detail)
	}
	joined := verdictOf(checks, "joined this epoch")
	if !strings.Contains(joined.Detail, "last joined epoch 1041") {
		t.Errorf("the local enrollment record was not offered: %q", joined.Detail)
	}
}

// A local enrollment record for a DIFFERENT slot is not this slot's, and
// must not be shown as though it were.
func TestAnEnrollmentRecordForAnotherSlotIsNotOffered(t *testing.T) {
	f := healthyFacts()
	f.DocErr = errASDown
	f.EpochErr = errASDown
	f.HasEnrollment, f.LocalSlot, f.LocalEpoch = true, 9, 1041
	if d := verdictOf(assembleDoctor(f), "joined this epoch").Detail; strings.Contains(d, "1041") {
		t.Errorf("another slot's enrollment was reported as this one's: %q", d)
	}
}

// Each check, over the states its own input can be in.
func TestDoctorVerdictsOverTheInputDomain(t *testing.T) {
	held := &auth.PayoutDeclaration{Address: "twilight1abc", CanonicalAddress: "twilight1abc"}
	for _, tc := range []struct {
		name   string
		mutate func(*doctorFacts)
		check  string
		want   doctorVerdict
		detail string // a substring the line must contain
		fix    string // a substring the fix must contain; "" means no fix
	}{
		{"no authorization stored", func(f *doctorFacts) { f.HasRefresh = false },
			"enrolled", verdictNo, "holds no authorization", "enroll"},
		{"state dir unreadable", func(f *doctorFacts) { f.LocalErr = errors.New("permission denied") },
			"enrolled", verdictUnknown, "state directory", "state_dir"},
		{"the AS refused a stored authorization", func(f *doctorFacts) { f.StatusErr = errors.New("401 invalid_grant") },
			"enrolled", verdictNo, "did not accept", "enroll"},

		{"joinable and not joined", func(f *doctorFacts) {
			f.Status = &auth.EpochStatus{JoinStatus: "NOT_JOINED", Joinable: true}
		}, "joined this epoch", verdictNo, "is open and this installation is not in it", "join"},
		{"closed and not joined", func(f *doctorFacts) {
			f.Status = &auth.EpochStatus{JoinStatus: "NOT_JOINED", Joinable: false, Phase: "SETTLEMENT"}
		}, "joined this epoch", verdictNo, "not joinable", "wait"},
		{"already accepted", func(f *doctorFacts) {
			f.Status = &auth.EpochStatus{JoinStatus: auth.JoinAlreadyAccepted}
		}, "joined this epoch", verdictOK, "already_accepted", ""},
		{"nothing open", func(f *doctorFacts) { f.EpochKnown = false },
			"joined this epoch", verdictNo, "no open target", ""},

		{"no declaration at all", func(f *doctorFacts) { f.Standing = &auth.PayoutStanding{} },
			"payout address", verdictNo, "nothing can be paid to you", "payout set"},
		{"standing is nil", func(f *doctorFacts) { f.Standing = nil },
			"payout address", verdictNo, "nothing can be paid to you", "payout set"},
		{"proposed, not in force", func(f *doctorFacts) { f.Standing = &auth.PayoutStanding{Pending: held} },
			"payout address", verdictNo, "NOT in force", "operator"},
		{"proposed and already taken", func(f *doctorFacts) {
			p := *held
			p.HeldFor = auth.HeldAddressInUse
			f.Standing = &auth.PayoutStanding{Pending: &p}
		}, "payout address", verdictNo, "registered to another participant", "different address"},
		{"active with a change pending", func(f *doctorFacts) {
			f.Standing = &auth.PayoutStanding{Active: held, Pending: held}
		}, "payout address", verdictOK, "waiting for an operator", ""},
		{"the AS could not be asked", func(f *doctorFacts) { f.StandingErr = errors.New("503") },
			"payout address", verdictUnknown, "could not ask", ""},

		{"nothing verified", func(f *doctorFacts) {
			f.Activity = &auth.EpochActivity{VerifiedActivity: false}
		}, "earning", verdictNo, "0 observations verified; 1 qualifies", "start"},
		{"verified but the AS says no", func(f *doctorFacts) {
			f.Activity = &auth.EpochActivity{VerifiedActivity: false, VerifiedObservationCount: 3}
		}, "earning", verdictNo, "3 observations verified", "start"},
		{"rejected observations are named", func(f *doctorFacts) {
			f.Activity = &auth.EpochActivity{VerifiedActivity: true, VerifiedObservationCount: 1, RejectedObservationCount: 2}
		}, "earning", verdictOK, "2 observations rejected", ""},
		{"pending observations are named", func(f *doctorFacts) {
			f.Activity = &auth.EpochActivity{VerifiedActivity: false, PendingObservationCount: 1}
		}, "earning", verdictNo, "1 observation not yet verified", "start"},
		{"nothing open to earn in", func(f *doctorFacts) { f.EpochKnown = false },
			"earning", verdictNo, "no epoch is open", ""},
		{"activity unavailable", func(f *doctorFacts) { f.ActivityErr = errors.New("503") },
			"earning", verdictUnknown, "could not ask", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := healthyFacts()
			tc.mutate(&f)
			got := verdictOf(assembleDoctor(f), tc.check)
			if got.Verdict != tc.want {
				t.Fatalf("%s = %s, want %s (%s)", tc.check, got.Verdict, tc.want, got.Detail)
			}
			if !strings.Contains(got.Detail, tc.detail) {
				t.Errorf("detail %q does not contain %q", got.Detail, tc.detail)
			}
			if tc.fix == "" && got.Fix != "" {
				t.Errorf("%s offers a fix it should not: %q", tc.check, got.Fix)
			}
			if tc.fix != "" && !strings.Contains(got.Fix, tc.fix) {
				t.Errorf("fix %q does not contain %q", got.Fix, tc.fix)
			}
		})
	}
}

// A count is never a quantity of money. The earning line has to say so on
// the line, where it is read, not only in the footer.
func TestTheEarningLineNamesTheThresholdAsAThreshold(t *testing.T) {
	d := verdictOf(assembleDoctor(healthyFacts()), "earning").Detail
	for _, want := range []string{"qualifies", "threshold", "more does not earn more"} {
		if !strings.Contains(d, want) {
			t.Errorf("the earning line does not say %q: %q", want, d)
		}
	}
}

// The report's own words. `doctor` reports setup; it must never look like a
// statement of money, and must never price a request.
func TestTheReportNeverReadsAsAStatementOfMoney(t *testing.T) {
	var b bytes.Buffer
	printDoctor(&b, assembleDoctor(healthyFacts()), doctorFacts{SpoolDir: t.TempDir()})
	out := b.String()
	lower := strings.ToLower(out)
	for _, forbidden := range []string{"you have earned", "per request", "per-request", "balance:"} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("the report contains %q:\n%s", forbidden, out)
		}
	}
	for _, want := range []string{
		"equal split among its eligible participants",
		"more do not earn more",
		"dropin-miner earnings",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}
}

// The checks that could not run are named. A report that quietly omitted
// them would read as a clean bill of health with two lines missing.
func TestTheReportNamesTheChecksThatCouldNotRun(t *testing.T) {
	f := healthyFacts()
	f.DocErr = errASDown
	f.EpochErr, f.StatusErr, f.StandingErr, f.ActivityErr = errASDown, errASDown, errASDown, errASDown
	var b bytes.Buffer
	printDoctor(&b, assembleDoctor(f), doctorFacts{SpoolDir: t.TempDir()})
	out := b.String()
	if !strings.Contains(out, "3 check(s) could not run") {
		t.Fatalf("the report does not say which checks could not run:\n%s", out)
	}
	for _, name := range []string{"joined this epoch", "payout address", "earning"} {
		if !strings.Contains(out, name) {
			t.Errorf("%q is not named among them:\n%s", name, out)
		}
	}
}

// The spool line is local and always present: an unreachable AS with a queue
// is a proxy doing its job, and saying so is the difference between "wait"
// and "something is broken".
func TestTheSpoolLineIsPrintedWhateverTheASSaid(t *testing.T) {
	var b bytes.Buffer
	printQueueTo(&b, filepath.Join(t.TempDir(), "spool"))
	if !strings.Contains(b.String(), "queued") {
		t.Fatalf("no spool line:\n%s", b.String())
	}
}

// ---- gathering ----

func doctorStateDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Discovery failing decides every AS-backed answer, and the gather must not
// then go on to make four more calls that can only fail the same way.
func TestAFailedDiscoveryStopsTheASCallsAndMarksThemAll(t *testing.T) {
	as := &stubAS{docErr: errASDown}
	f := gatherDoctorFacts(context.Background(), as, config.Mining{
		ASBaseURL: "https://as.example.com", ChainID: "twilight-1", SlotID: 7,
		StateDir: doctorStateDir(t), SpoolDir: t.TempDir(),
	})
	if len(as.calls) != 1 || as.calls[0] != "document" {
		t.Fatalf("calls after a failed discovery: %v", as.calls)
	}
	for name, err := range map[string]error{
		"epoch": f.EpochErr, "status": f.StatusErr, "standing": f.StandingErr, "activity": f.ActivityErr,
	} {
		if err == nil {
			t.Errorf("%s has no error recorded, so its check has no way to know it was never asked", name)
		}
	}
}

// Where a participant is paid does not depend on which epoch is open, so it
// is asked even when nothing is. Before this it was skipped, and the report
// said "no address is in force" on a run that never asked.
func TestThePayoutStandingIsAskedEvenWithNoOpenTarget(t *testing.T) {
	as := &stubAS{
		doc:      &wire.DiscoveryDocument{ChainID: "twilight-1", SlotID: "7"},
		target:   nil, // the AS answered: nothing is open
		standing: &auth.PayoutStanding{Active: &auth.PayoutDeclaration{Address: "twilight1abc", Effective: true}},
	}
	f := gatherDoctorFacts(context.Background(), as, config.Mining{
		ASBaseURL: "https://as.example.com", StateDir: doctorStateDir(t), SpoolDir: t.TempDir(),
	})
	if f.EpochKnown {
		t.Fatal("a nil target was read as an open epoch")
	}
	if f.Standing == nil {
		t.Fatal("the payout standing was not asked for when no target was open")
	}
	if got := verdictOf(assembleDoctor(f), "payout address"); got.Verdict != verdictOK {
		t.Fatalf("payout address = %s (%s), want OK", got.Verdict, got.Detail)
	}
}

// A stored refresh token and a recorded enrollment are read from the state
// directory, not inferred.
func TestTheLocalCustodyStateIsReadFromDisk(t *testing.T) {
	dir := doctorStateDir(t)
	store, err := auth.OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRefreshToken("rt-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveEnrollment(7, 1041); err != nil {
		t.Fatal(err)
	}
	f := gatherDoctorFacts(context.Background(), &stubAS{docErr: errASDown}, config.Mining{
		SlotID: 7, StateDir: dir, SpoolDir: t.TempDir(),
	})
	if !f.HasRefresh {
		t.Error("a stored refresh authorization was not seen")
	}
	if !f.HasEnrollment || f.LocalSlot != 7 || f.LocalEpoch != 1041 {
		t.Errorf("enrollment record misread: %+v", f)
	}
}

// A pinned epoch is used and labeled as pinned: a stale pin is exactly the
// thing a health check exists to surface.
func TestAPinnedEpochIsUsedAndSaidToBePinned(t *testing.T) {
	pinned := uint64(999)
	as := &stubAS{
		doc:      &wire.DiscoveryDocument{ChainID: "twilight-1", SlotID: "7"},
		status:   &auth.EpochStatus{JoinStatus: auth.JoinAccepted},
		standing: &auth.PayoutStanding{},
		activity: &auth.EpochActivity{},
	}
	f := gatherDoctorFacts(context.Background(), as, config.Mining{
		TargetEpoch: &pinned, StateDir: doctorStateDir(t), SpoolDir: t.TempDir(),
	})
	if f.Epoch != 999 || !f.EpochPinned {
		t.Fatalf("pinned epoch not honored: %+v", f)
	}
	for _, c := range as.calls {
		if c == "current-target" {
			t.Error("the AS was asked which epoch is open although one is pinned")
		}
	}
	if d := verdictOf(assembleDoctor(f), "joined this epoch").Detail; !strings.Contains(d, "pinned by mining.target_epoch") {
		t.Errorf("a pinned epoch is not labeled as one: %q", d)
	}
}
