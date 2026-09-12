package main

// The two checks added for #21: can this process write where a search
// records, and does recent mining-plane activity have anything to show for
// itself.
//
// The probe is the only write doctor performs, so its tests assert on what
// is left on disk as well as on the verdict. The recording check is a
// heuristic over eight inputs, so its tests start from the one state that
// produces the advice and flip a single condition at a time — a table that
// only asserted the positive case would pass just as well if the check
// ignored six of its inputs.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/auth"
	"github.com/twilight-project/dropin-miner/pkg/config"
)

// ── the probe ───────────────────────────────────────────────────────────

// recordingProbeOps wraps the real seam, counting calls so a test can
// prove the probe did nothing rather than merely that it reported nothing.
type recordingProbeOps struct {
	ops     intakeProbeOps
	mkdir   int
	writes  int
	removes int
}

func realProbeCounting() *recordingProbeOps {
	r := &recordingProbeOps{}
	real := realIntakeProbeOps()
	r.ops = intakeProbeOps{
		mkdirAll: func(d string, m os.FileMode) error { r.mkdir++; return real.mkdirAll(d, m) },
		writeAtomic: func(dir, name string, data []byte, mode os.FileMode) error {
			r.writes++
			return real.writeAtomic(dir, name, data, mode)
		},
		remove: func(p string) error { r.removes++; return real.remove(p) },
	}
	return r
}

func (r *recordingProbeOps) calls() int { return r.mkdir + r.writes + r.removes }

// minerFacts is an installation with intake configured and mining on —
// the only state in which either new check does any work.
func minerFacts(intakeDir string) doctorFacts {
	return doctorFacts{
		MinerEnabled:    true,
		IntakeDir:       intakeDir,
		MiningDecision:  auth.MiningDecision{State: auth.MiningEnabled, Present: true},
		LocalStateKnown: true,
	}
}

func TestProbeSucceedsAndLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	ops := realProbeCounting()
	res := probeIntakeWritable(ops.ops, dir)
	if !res.ok() {
		t.Fatalf("probe failed: stage=%q err=%v leftover=%q", res.Stage, res.Err, res.Leftover)
	}
	if !res.Ran || res.Created {
		t.Errorf("ran=%v created=%v; the directory already existed", res.Ran, res.Created)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the probe left files behind: %v", names)
	}

	f := minerFacts(dir)
	f.IntakeProbe = res
	c := doctorIntakeCheck(f)
	if c.Verdict != verdictOK || c.Detail != "writable from this process" {
		t.Errorf("check = %s/%q, want OK/writable from this process", c.Verdict, c.Detail)
	}
	// The wording must not claim anything about an agent's sandbox, which
	// the probe never entered.
	if strings.Contains(c.Detail, "sandbox") {
		t.Errorf("a successful probe claimed something about the sandbox: %q", c.Detail)
	}
}

// The probe name is what keeps a leftover harmless. readIntake considers
// only .json files, so this is the property, not "never leaves a file".
func TestTheProbeNameCanNeverEnterTheMiningPipeline(t *testing.T) {
	dir := t.TempDir()
	var written string
	ops := intakeProbeOps{
		mkdirAll: os.MkdirAll,
		writeAtomic: func(d, name string, data []byte, mode os.FileMode) error {
			written = name
			return os.WriteFile(filepath.Join(d, name), data, mode)
		},
		remove: func(string) error { return nil }, // deliberately leave it
	}
	res := probeIntakeWritable(ops, dir)
	if written == "" {
		t.Fatal("no probe file was written")
	}
	if strings.HasSuffix(written, ".json") {
		t.Fatalf("the probe file %q ends in .json and could be promoted", written)
	}
	if !strings.HasPrefix(written, ".doctor-probe-") {
		t.Errorf("probe name %q is not recognizable as a probe", written)
	}
	_ = res
	// And the flush's own reader agrees it is not a record.
	recs, unreadable, err := readIntake(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 || len(unreadable) != 0 {
		t.Errorf("the leftover probe was visible to readIntake: %d records, %d unreadable", len(recs), len(unreadable))
	}
}

func TestEachProbeStageFailureIsReported(t *testing.T) {
	boom := errors.New("injected failure")
	for _, tc := range []struct {
		name      string
		ops       func(dir string) intakeProbeOps
		wantStage string
	}{
		{
			name: "mkdirAll",
			ops: func(string) intakeProbeOps {
				o := realIntakeProbeOps()
				o.mkdirAll = func(string, os.FileMode) error { return boom }
				return o
			},
			wantStage: probeStageMkdir,
		},
		{
			name: "writeAtomic",
			ops: func(string) intakeProbeOps {
				o := realIntakeProbeOps()
				o.writeAtomic = func(string, string, []byte, os.FileMode) error { return boom }
				return o
			},
			wantStage: probeStageWrite,
		},
		{
			name: "remove",
			ops: func(string) intakeProbeOps {
				o := realIntakeProbeOps()
				o.remove = func(string) error { return boom }
				return o
			},
			wantStage: probeStageRemove,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			res := probeIntakeWritable(tc.ops(dir), dir)
			if res.Stage != tc.wantStage {
				t.Fatalf("stage %q, want %q", res.Stage, tc.wantStage)
			}
			f := minerFacts(dir)
			f.IntakeProbe = res
			c := doctorIntakeCheck(f)
			if c.Verdict != verdictNo {
				t.Errorf("verdict %s, want NO", c.Verdict)
			}
			if !strings.Contains(c.Detail, tc.wantStage) {
				t.Errorf("detail does not name the failing stage %q: %q", tc.wantStage, c.Detail)
			}
			if !strings.Contains(c.Detail, dir) {
				t.Errorf("detail does not name the directory: %q", c.Detail)
			}
		})
	}
}

// Some write failures leave the final name in place, so cleanup has to run
// after a failed write too — not only on the success path.
func TestCleanupRunsEvenAfterAWriteFailure(t *testing.T) {
	dir := t.TempDir()
	removed := ""
	ops := intakeProbeOps{
		mkdirAll: os.MkdirAll,
		writeAtomic: func(d, name string, data []byte, mode os.FileMode) error {
			// The shape that matters: the final name exists AND the write
			// reports failure.
			if err := os.WriteFile(filepath.Join(d, name), data, mode); err != nil {
				return err
			}
			return errors.New("injected failure after publication")
		},
		remove: func(p string) error { removed = p; return os.Remove(p) }, // #nosec G703 -- this test's own temp dir
	}
	res := probeIntakeWritable(ops, dir)
	if res.Stage != probeStageWrite {
		t.Fatalf("stage %q, want %q", res.Stage, probeStageWrite)
	}
	if removed == "" {
		t.Fatal("cleanup did not run after the write failure")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the file the failed write published was left behind: %d entries", len(entries))
	}
}

func TestACleanupFailureIsAProbeFailureNamingTheLeftover(t *testing.T) {
	dir := t.TempDir()
	ops := realIntakeProbeOps()
	ops.remove = func(string) error { return errors.New("injected cleanup failure") }
	res := probeIntakeWritable(ops, dir)
	if res.Leftover == "" {
		t.Fatal("no leftover path recorded")
	}
	f := minerFacts(dir)
	f.IntakeProbe = res
	c := doctorIntakeCheck(f)
	if c.Verdict != verdictNo {
		t.Fatalf("verdict %s, want NO", c.Verdict)
	}
	if !strings.Contains(c.Detail, res.Leftover) {
		t.Errorf("detail does not name the leftover path %q: %q", res.Leftover, c.Detail)
	}
	// The file really is still there, and really is inert.
	if _, err := os.Stat(res.Leftover); err != nil {
		t.Errorf("the named leftover does not exist: %v", err)
	}
	if strings.HasSuffix(res.Leftover, ".json") {
		t.Error("the leftover ends in .json")
	}
}

func TestProbeIsSkippedWhenIntakeIsNotConfiguredOrMiningIsOff(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name   string
		facts  doctorFacts
		detail string
	}{
		{"no [miner]", doctorFacts{IntakeDir: dir}, "not configured"},
		{
			"mining off",
			doctorFacts{MinerEnabled: true, IntakeDir: dir, LocalStateKnown: true,
				MiningDecision: auth.MiningDecision{State: auth.MiningDisabled, Present: true}},
			"mining not active",
		},
		{
			"mining undecided",
			doctorFacts{MinerEnabled: true, IntakeDir: dir, LocalStateKnown: true,
				MiningDecision: auth.MiningDecision{State: auth.MiningUndecided}},
			"mining not active",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, c := range []doctorCheck{doctorIntakeCheck(tc.facts), doctorRecordingCheck(tc.facts)} {
				if c.Verdict != verdictOK {
					t.Errorf("%s: verdict %s, want OK — an unconfigured miner is not a failure", c.Name, c.Verdict)
				}
				if c.Detail != tc.detail {
					t.Errorf("%s: detail %q, want %q", c.Name, c.Detail, tc.detail)
				}
			}
		})
	}
}

// Bounded creation, both directions. README promises doctor does not build
// a state tree to diagnose one; the intake directory is the single
// exception, and only when its parent is already there.
func TestDirectoryCreationIsBoundedToTheIntakeDirectoryItself(t *testing.T) {
	t.Run("absent intake dir with an existing parent is created", func(t *testing.T) {
		parent := t.TempDir()
		dir := filepath.Join(parent, "intake")
		ops := realProbeCounting()
		res := probeIntakeWritable(ops.ops, dir)
		if !res.ok() || !res.Created {
			t.Fatalf("ok=%v created=%v stage=%q err=%v", res.ok(), res.Created, res.Stage, res.Err)
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("the intake directory was not created: %v", err)
		}
		f := minerFacts(dir)
		f.IntakeProbe = res
		c := doctorIntakeCheck(f)
		if c.Verdict != verdictOK || !strings.Contains(c.Detail, "created") {
			t.Errorf("check = %s/%q, want OK mentioning created", c.Verdict, c.Detail)
		}
	})

	t.Run("absent parent creates nothing and makes no seam call", func(t *testing.T) {
		root := t.TempDir()
		dir := filepath.Join(root, "missing-parent", "intake")
		ops := realProbeCounting()
		res := probeIntakeWritable(ops.ops, dir)
		if !res.ParentMissing || res.Ran {
			t.Fatalf("parentMissing=%v ran=%v", res.ParentMissing, res.Ran)
		}
		if n := ops.calls(); n != 0 {
			t.Errorf("%d seam call(s) made for an absent parent", n)
		}
		if _, err := os.Stat(filepath.Join(root, "missing-parent")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("doctor created a directory above the intake directory: %v", err)
		}
		f := minerFacts(dir)
		f.IntakeProbe = res
		c := doctorIntakeCheck(f)
		if c.Verdict != verdictOK {
			t.Errorf("verdict %s, want OK — nothing is wrong yet", c.Verdict)
		}
		if !strings.Contains(c.Detail, "not created yet") || !strings.Contains(c.Detail, dir) {
			t.Errorf("detail %q, want 'not created yet' naming %s", c.Detail, dir)
		}
	})
}

// The real thing, against a real unwritable directory: the seam models the
// writer faithfully, but only the filesystem can prove the permission
// shape produces the conditional Codex wording.
func TestAnUnwritableIntakeDirectoryReportsTheSandboxFix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits; Windows permissions do not deny the owner this way")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the mode bits this case depends on")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "intake")
	if err := os.Mkdir(dir, 0o500); err != nil { // r-x: listable, not writable
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) // #nosec G302 -- a directory, restored so TempDir cleanup can remove it

	res := probeIntakeWritable(realIntakeProbeOps(), dir)
	if res.ok() {
		t.Fatal("the probe succeeded in a 0500 directory")
	}
	f := minerFacts(dir)
	f.IntakeProbe = res
	c := doctorIntakeCheck(f)
	if c.Verdict != verdictNo {
		t.Fatalf("verdict %s, want NO", c.Verdict)
	}
	if !strings.Contains(c.Fix, "agents install") || !strings.Contains(c.Fix, dir) {
		t.Errorf("fix %q does not offer the Codex remedy naming %s", c.Fix, dir)
	}
	// It suggests; it does not assert the sandbox is the cause.
	if !strings.Contains(c.Fix, "if searches run under Codex") {
		t.Errorf("fix asserts the sandbox rather than suggesting it: %q", c.Fix)
	}
}

// ── recording ───────────────────────────────────────────────────────────

var recordingNow = time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

// suspiciousFacts is the exact state the advice is for: mining on, a flush
// within the window, a probe that worked, nothing in intake, nothing in
// the spool or its quarantine, no capture health, and an AS reporting
// nothing at all for the epoch.
func suspiciousFacts() doctorFacts {
	f := minerFacts("/fictional/tokendrop/intake")
	f.Now = recordingNow
	f.IntakeProbe = intakeProbeResult{Ran: true, Dir: f.IntakeDir}
	f.Stamp = flushStamp{V: 1, LastFlush: recordingNow.Add(-time.Hour)}
	f.StampPresent = true
	f.Activity = &auth.EpochActivity{}
	return f
}

func TestTheAdviceAppearsOnlyInTheSuspiciousState(t *testing.T) {
	c := doctorRecordingCheck(suspiciousFacts())
	if c.Verdict != verdictUnknown {
		t.Fatalf("verdict %s, want UNKNOWN — this is a heuristic, not a fault", c.Verdict)
	}
	for _, want := range []string{
		"recent miner activity",
		"nothing is queued locally or verified at the AS",
		"the directory the agent's hook writes to",
		"/fictional/tokendrop/intake",
	} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail is missing %q:\n%s", want, c.Detail)
		}
	}
	if strings.HasPrefix(c.Detail, "could not determine") {
		t.Error("the suspicious state was reported as indeterminate")
	}
	if c.Fix == "" {
		t.Error("the advice offers no next command")
	}
}

// Flip one condition at a time. Each alone must change the answer, or the
// check is ignoring that input.
func TestEachConditionAloneChangesTheRecordingVerdict(t *testing.T) {
	advice := doctorRecordingCheck(suspiciousFacts()).Detail

	for _, tc := range []struct {
		name         string
		mutate       func(*doctorFacts)
		wantVerdict  doctorVerdict
		wantContains string
		undetermined bool
	}{
		{
			name:         "stamp absent",
			mutate:       func(f *doctorFacts) { f.StampPresent, f.Stamp = false, flushStamp{} },
			wantVerdict:  verdictOK,
			wantContains: "no recent activity",
		},
		{
			name: "stamp 24h+1s old",
			mutate: func(f *doctorFacts) {
				f.Stamp.LastFlush = recordingNow.Add(-recordingWindow - time.Second)
			},
			wantVerdict:  verdictOK,
			wantContains: "no recent activity",
		},
		{
			name:         "stamp in the future",
			mutate:       func(f *doctorFacts) { f.Stamp.LastFlush = recordingNow.Add(time.Minute) },
			wantVerdict:  verdictUnknown,
			wantContains: "flush stamp is in the future",
			undetermined: true,
		},
		{
			name: "capture health present",
			mutate: func(f *doctorFacts) {
				f.Health = []auth.HealthRecord{{
					Version: 1, Component: auth.HealthCapture, Reason: auth.HealthIntakeUnwritable,
				}}
			},
			wantVerdict:  verdictOK,
			wantContains: "capture failure is already recorded",
		},
		{
			name: "decision off",
			mutate: func(f *doctorFacts) {
				f.MiningDecision = auth.MiningDecision{State: auth.MiningDisabled, Present: true}
			},
			wantVerdict:  verdictOK,
			wantContains: "mining not active",
		},
		{
			name:         "intake non-empty",
			mutate:       func(f *doctorFacts) { f.IntakeCount = 2 },
			wantVerdict:  verdictOK,
			wantContains: "waiting in",
		},
		{
			name:         "spool non-empty",
			mutate:       func(f *doctorFacts) { f.SpoolCount = 3 },
			wantVerdict:  verdictOK,
			wantContains: "queued in the spool",
		},
		{
			// The case Count alone would miss: a quarantined record is
			// strong evidence something WAS recorded.
			name:         "quarantine non-empty with the spool empty",
			mutate:       func(f *doctorFacts) { f.QuarantineCount = 1 },
			wantVerdict:  verdictOK,
			wantContains: "quarantined",
		},
		{
			name: "probe failed",
			mutate: func(f *doctorFacts) {
				f.IntakeProbe.Stage, f.IntakeProbe.Err = probeStageWrite, errors.New("nope")
			},
			wantVerdict:  verdictUnknown,
			wantContains: "the intake probe failed; see intake writable",
			undetermined: true,
		},
		{
			name:         "AS activity unavailable",
			mutate:       func(f *doctorFacts) { f.Activity, f.ActivityErr = nil, errASDown },
			wantVerdict:  verdictUnknown,
			wantContains: "the AS did not report",
			undetermined: true,
		},
		{
			name:         "pending > 0",
			mutate:       func(f *doctorFacts) { f.Activity = &auth.EpochActivity{PendingObservationCount: 1} },
			wantVerdict:  verdictOK,
			wantContains: "1 pending",
		},
		{
			// A rejected observation reached the AS, so "nothing was
			// recorded" would be false.
			name:         "rejected > 0",
			mutate:       func(f *doctorFacts) { f.Activity = &auth.EpochActivity{RejectedObservationCount: 1} },
			wantVerdict:  verdictOK,
			wantContains: "1 rejected",
		},
		{
			name:         "verified > 0",
			mutate:       func(f *doctorFacts) { f.Activity = &auth.EpochActivity{VerifiedObservationCount: 4} },
			wantVerdict:  verdictOK,
			wantContains: "4 verified",
		},
		{
			name:         "unreadable stamp",
			mutate:       func(f *doctorFacts) { f.StampErr = errors.New("permission denied") },
			wantVerdict:  verdictUnknown,
			wantContains: "the flush stamp could not be read",
			undetermined: true,
		},
		{
			name:         "unreadable spool",
			mutate:       func(f *doctorFacts) { f.SpoolErr = errors.New("permission denied") },
			wantVerdict:  verdictUnknown,
			wantContains: "the spool could not be read",
			undetermined: true,
		},
		{
			name:         "unreadable intake",
			mutate:       func(f *doctorFacts) { f.IntakeErr = errors.New("permission denied") },
			wantVerdict:  verdictUnknown,
			wantContains: "the intake directory could not be read",
			undetermined: true,
		},
		{
			name:         "unreadable component health",
			mutate:       func(f *doctorFacts) { f.HealthErr = errors.New("permission denied") },
			wantVerdict:  verdictUnknown,
			wantContains: "component health could not be read",
			undetermined: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := suspiciousFacts()
			tc.mutate(&f)
			c := doctorRecordingCheck(f)

			if c.Verdict != tc.wantVerdict {
				t.Errorf("verdict %s, want %s (detail %q)", c.Verdict, tc.wantVerdict, c.Detail)
			}
			if !strings.Contains(c.Detail, tc.wantContains) {
				t.Errorf("detail %q does not contain %q", c.Detail, tc.wantContains)
			}
			// The point of the table: this condition alone moved the
			// answer off the advice.
			if c.Detail == advice {
				t.Errorf("flipping %q did not change the verdict; the check ignores it", tc.name)
			}
			// Every indeterminate answer carries its reason, and none of
			// them is a NO.
			if tc.undetermined && !strings.HasPrefix(c.Detail, "could not determine — ") {
				t.Errorf("indeterminate case does not say why: %q", c.Detail)
			}
			if c.Verdict == verdictNo {
				t.Errorf("recording returned NO; it is a heuristic and may never assert a fault (%q)", c.Detail)
			}
		})
	}
}

// The window is inclusive at both ends and measured against f.Now, never
// against the wall clock.
func TestTheRecordingWindowIsMeasuredAgainstTheGatheredTime(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset time.Duration
		recent bool
	}{
		{"just now", 0, true},
		{"one second inside", -recordingWindow + time.Second, true},
		{"exactly 24h", -recordingWindow, true},
		{"one second outside", -recordingWindow - time.Second, false},
		{"a week ago", -7 * 24 * time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := suspiciousFacts()
			f.Stamp.LastFlush = recordingNow.Add(tc.offset)
			c := doctorRecordingCheck(f)
			gotRecent := strings.Contains(c.Detail, "recent miner activity")
			if gotRecent != tc.recent {
				t.Errorf("recent=%v, want %v (detail %q)", gotRecent, tc.recent, c.Detail)
			}
		})
	}
}

// ── the gatherer wires the real readers to the real state ───────────────

func TestGatherReadsTheMinerHalfFromDisk(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	intakeDir := filepath.Join(root, "miner", "intake")
	spoolDir := filepath.Join(root, "spool")
	if err := os.MkdirAll(filepath.Join(root, "miner"), 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := auth.OpenStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMiningEnabled(true); err != nil {
		t.Fatal(err)
	}
	// One record in intake, one queued, one quarantined.
	if err := os.MkdirAll(intakeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(intakeDir, "0001-a.json"), []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(spoolDir, "quarantine"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spoolDir, "q1.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spoolDir, "quarantine", "q2.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	f := gatherDoctorFactsFor(t.Context(), &stubAS{docErr: errASDown},
		config.Mining{StateDir: stateDir, SpoolDir: spoolDir},
		config.Miner{Enabled: true, IntakeDir: intakeDir},
		realIntakeProbeOps())

	if f.Now.Before(before) || f.Now.After(time.Now()) {
		t.Errorf("Now was not sampled during the gather: %s", f.Now)
	}
	if f.IntakeCount != 1 {
		t.Errorf("intake count %d, want 1", f.IntakeCount)
	}
	if f.SpoolCount != 1 || f.QuarantineCount != 1 {
		t.Errorf("spool=%d quarantine=%d, want 1 and 1", f.SpoolCount, f.QuarantineCount)
	}
	if f.StampPresent || f.StampErr != nil {
		t.Errorf("stamp present=%v err=%v; none was written", f.StampPresent, f.StampErr)
	}
	if !f.IntakeProbe.ok() {
		t.Errorf("probe failed: %q %v", f.IntakeProbe.Stage, f.IntakeProbe.Err)
	}
	// The probe cleaned up after itself, and the real record is untouched.
	entries, err := os.ReadDir(intakeDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "0001-a.json" {
		t.Errorf("intake directory changed: %v", entries)
	}
}

// The doctor-only stamp reader keeps the three answers apart, which is the
// entire reason it exists beside readFlushStamp.
func TestTheDoctorStampReaderSeparatesAbsentFromUnreadable(t *testing.T) {
	dir := t.TempDir()
	t.Run("absent", func(t *testing.T) {
		st, present, err := readFlushStampForDoctor(filepath.Join(dir, "nope.json"))
		if present || err != nil {
			t.Errorf("present=%v err=%v, want false/nil", present, err)
		}
		_ = st
	})
	t.Run("valid v1", func(t *testing.T) {
		path := filepath.Join(dir, "ok.json")
		when := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		body := fmt.Sprintf(`{"v":1,"last_flush":%q}`, when.Format(time.RFC3339Nano))
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		st, present, err := readFlushStampForDoctor(path)
		if !present || err != nil {
			t.Fatalf("present=%v err=%v", present, err)
		}
		if !st.LastFlush.Equal(when) {
			t.Errorf("last flush %s, want %s", st.LastFlush, when)
		}
	})
	for name, body := range map[string]string{
		"malformed JSON": `{"v":1,`,
		"wrong version":  `{"v":2}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, present, err := readFlushStampForDoctor(path)
			if err == nil {
				t.Fatalf("%s was reported as readable (present=%v)", name, present)
			}
			if present {
				t.Error("present is true alongside an error")
			}
		})
	}
	// readFlushStamp still folds all of it into a zero stamp, unchanged.
	path := filepath.Join(dir, "malformed-JSON.json")
	if st := readFlushStamp(path); st.V != 0 {
		t.Errorf("readFlushStamp changed behavior: %+v", st)
	}
}
