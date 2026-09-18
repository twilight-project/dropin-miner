package main

// Whose session a search threads into (#97).
//
// The lineage sidecar is keyed on a workspace root, and `search` used to walk
// up to eight parent directories and take ANY sidecar less than twelve hours
// old. A host that writes none of its own therefore adopted whichever other
// host was working above it. Measured on the 0.2.10 release check: a Cursor
// CLI search in a subdirectory reached the router as harness=claude-code,
// carrying a Claude Code session's id AND its assistant text, and advancing
// that session's seq. A plain search from a terminal in the same tree did the
// same. The shape is ordinary — an editor open at a repository root, a second
// agent working in a subdirectory.
//
// The rule: the walk answers nothing unless the search can say whose session
// it is making, and the sidecar agrees.

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/config"
)

const (
	adoptSessions = "/sessions"
	adoptRoot     = "/home/u/project"
)

// lineageProbe is a workspace with one host's sidecar already written, and a
// search about to run somewhere inside it.
type lineageProbe struct {
	fs  *fakeHookFS
	ops searchOps
	now time.Time
	// reads counts lineage files opened, so a test can ask not only what a
	// search adopted but what it looked at.
	reads *int
}

func newLineageProbe(t *testing.T, cwd string, env map[string]string) *lineageProbe {
	t.Helper()
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	fs, hook := newFakeHookOps(env)
	hook.now = func() time.Time { return now }
	reads := 0
	underlying := hook.readFile
	hook.readFile = func(p string) ([]byte, error) {
		reads++
		return underlying(p)
	}
	return &lineageProbe{
		fs:    fs,
		now:   now,
		reads: &reads,
		ops: searchOps{
			getppid:  func() int { return 4242 },
			hostname: func() (string, error) { return "probe-host", nil },
			getwd:    func() (string, error) { return cwd, nil },
			now:      func() time.Time { return now },
			hook:     hook,
		},
	}
}

// write lays down one host's sidecar for a workspace root.
func (p *lineageProbe) write(t *testing.T, workspace, harness, session string) string {
	t.Helper()
	path := lineagePath(adoptSessions, workspace)
	err := updateLineage(p.ops.hook, path, p.now, func(l *lineageFile) {
		l.Harness, l.SessionID, l.Window = harness, session, "none"
		l.Seq = 7
		l.History = []traceHistory{{Role: "assistant", Text: "I'll start by checking whether the wiring step has already been run."}}
	})
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// seqOf reads a sidecar's sequence back off disk.
func (p *lineageProbe) seqOf(t *testing.T, path string) int {
	t.Helper()
	l, ok := loadLineage(p.ops.hook, path)
	if !ok {
		t.Fatalf("the sidecar at %s is gone", path)
	}
	return l.Seq
}

func (p *lineageProbe) trace(env map[string]string) *traceEnvelope {
	trace, _ := searchTrace(p.ops, config.Miner{SessionsDir: adoptSessions}, func(k string) string { return env[k] })
	return trace
}

// A sidecar written by host A is not adopted by a search from host B running
// in a subdirectory, and B's search does not touch A's sequence. This is
// #97's measured case.
func TestASearchDoesNotAdoptAnotherHostsLineage(t *testing.T) {
	p := newLineageProbe(t, filepath.Join(adoptRoot, "ws-cursor"), nil)
	claude := p.write(t, adoptRoot, "claude-code", "claude-session")

	env := p.trace(map[string]string{"TOKENDROP_HARNESS": "cursor"})

	if env == nil {
		t.Fatal("a search that can adopt nothing must still send a usable trace")
	}
	if env.Harness != "cursor" {
		t.Errorf("the search went out as harness %q, want cursor", env.Harness)
	}
	if env.SessionID == "claude-session" {
		t.Error("the search took another host's session id")
	}
	if len(env.History) != 0 {
		t.Errorf("the search carried another host's assistant text off the machine: %+v", env.History)
	}
	if got := p.seqOf(t, claude); got != 7 {
		t.Errorf("another session's seq moved to %d; a search must never advance a sequence it did not own", got)
	}
}

// A search that can say nothing about whose it is adopts nothing — the plain
// terminal search from #97, which had no host and no channel. It still gets a
// usable trace: its own per-shell identity, which is what harness=cli means.
func TestASearchThatNamesNoHostAdoptsNothingAndStillTraces(t *testing.T) {
	p := newLineageProbe(t, filepath.Join(adoptRoot, "ws-cursor"), nil)
	claude := p.write(t, adoptRoot, "claude-code", "claude-session")

	env := p.trace(nil)

	if env == nil {
		t.Fatal("a plain search sent no trace at all; a host with no lineage channel still gets its own identity")
	}
	if env.Harness != "cli" {
		t.Errorf("harness %q, want cli: the shell itself is the session when nothing else is", env.Harness)
	}
	if env.SessionID == "" || env.CallID == "" {
		t.Errorf("the per-shell identity is not usable: %+v", env)
	}
	if env.SessionID == "claude-session" {
		t.Error("the plain search took the session id of an agent working above it")
	}
	if got := p.seqOf(t, claude); got != 7 {
		t.Errorf("another session's seq moved to %d", got)
	}
}

// The walk still does the job it exists for: a host's OWN sidecar in a parent
// directory is adopted from a subdirectory, and that sequence is the one that
// advances. Without this the rule above would be "never adopt", which would
// cost every search that runs from a subshell its lineage.
func TestASearchStillAdoptsItsOwnHostsLineageFromASubdirectory(t *testing.T) {
	p := newLineageProbe(t, filepath.Join(adoptRoot, "src", "deep"), nil)
	mine := p.write(t, adoptRoot, "cursor", "cursor-session")

	env := p.trace(map[string]string{"TOKENDROP_HARNESS": "cursor"})

	if env == nil || env.SessionID != "cursor-session" {
		t.Fatalf("a host did not find its own sidecar from a subdirectory: %+v", env)
	}
	if env.Harness != "cursor" {
		t.Errorf("harness %q, want cursor", env.Harness)
	}
	if env.Seq != 8 {
		t.Errorf("seq %d, want 8: the search advances the sequence of the session it actually belongs to", env.Seq)
	}
	if got := p.seqOf(t, mine); got != 8 {
		t.Errorf("the adopted sidecar's seq on disk is %d, want 8", got)
	}
}

// A nearer sidecar belonging to someone else stops the walk: this directory
// is theirs, and a matching name further up would be a guess.
func TestAForeignSidecarStopsTheWalkRatherThanBeingClimbedPast(t *testing.T) {
	sub := filepath.Join(adoptRoot, "ws-cursor")
	p := newLineageProbe(t, sub, nil)
	mine := p.write(t, adoptRoot, "cursor", "cursor-session")
	theirs := p.write(t, sub, "claude-code", "claude-session")

	env := p.trace(map[string]string{"TOKENDROP_HARNESS": "cursor"})

	// Nothing is adopted, so the identity is this shell's own. The harness is
	// still cursor — that much the search does know about itself; what it
	// does not know is which cursor session, and it does not guess.
	if env == nil {
		t.Fatal("no trace at all")
	}
	if env.SessionID == "cursor-session" {
		t.Errorf("the walk climbed past a foreign sidecar to a matching one further up: %+v", env)
	}
	if env.SessionID == "claude-session" {
		t.Errorf("the foreign sidecar was adopted: %+v", env)
	}
	if got := p.seqOf(t, theirs); got != 7 {
		t.Errorf("the foreign sidecar's seq moved to %d", got)
	}
	if got := p.seqOf(t, mine); got != 7 {
		t.Errorf("a sidecar past the stopping point was advanced: %d", got)
	}
}

// The exact-path channel is unchanged: a host whose hook exported
// TOKENDROP_LINEAGE named its own file outright, and that is a declaration,
// not a guess.
func TestTheExactLineagePathIsStillHonoured(t *testing.T) {
	p := newLineageProbe(t, filepath.Join(adoptRoot, "ws-cursor"), nil)
	mine := p.write(t, adoptRoot, "cursor", "cursor-session")

	env := p.trace(map[string]string{lineageEnv: mine, "TOKENDROP_HARNESS": "cursor"})

	if env == nil || env.SessionID != "cursor-session" || env.Seq != 8 {
		t.Fatalf("the sidecar named outright was not used: %+v", env)
	}
}

// A search that can name no host does not even OPEN another session's
// sidecar.
//
// Refusing to adopt one after reading it would give the same trace, so this
// is a test about what the walk touches rather than what it returns: a search
// with nothing to match against has no business parsing the sessions of every
// agent working above it, and a walk it never starts cannot grow a way to
// adopt something later. The harness check and this gate are separate rules,
// and without this test only the first of them is guarded.
func TestASearchThatNamesNoHostReadsNoOtherSessionsFile(t *testing.T) {
	p := newLineageProbe(t, filepath.Join(adoptRoot, "ws-cursor"), nil)
	p.write(t, adoptRoot, "claude-code", "claude-session")

	*p.reads = 0
	env := p.trace(nil)

	if env == nil {
		t.Fatal("no trace at all")
	}
	if *p.reads != 0 {
		t.Errorf("a search naming no host opened %d lineage file(s) belonging to other sessions; it cannot own any of them, so it should not be reading them", *p.reads)
	}
}

// The harness names must match exactly, case included.
//
// The vocabulary is closed and lowercase and this client's hooks write both
// sides of the comparison, so a case variant can only come from a participant
// setting TOKENDROP_HARNESS by hand — and that same value is what the search
// sends to the router as its label. Adopting the session and relabelling it
// would put one session under two spellings downstream, which is #91's hazard
// arriving by a third route. Refusing costs that participant a threaded
// trace; accepting costs the router a split session.
func TestHarnessNamesMustMatchExactly(t *testing.T) {
	for _, declared := range []string{"Cursor", "CURSOR", "cursor "} {
		t.Run(declared, func(t *testing.T) {
			p := newLineageProbe(t, filepath.Join(adoptRoot, "src"), nil)
			mine := p.write(t, adoptRoot, "cursor", "cursor-session")

			env := p.trace(map[string]string{"TOKENDROP_HARNESS": declared})

			if env == nil {
				t.Fatal("no trace at all")
			}
			if env.SessionID == "cursor-session" {
				t.Errorf("a search declaring %q adopted the session of a sidecar recording %q; the label it sends is %q, so the session would appear under two spellings", declared, "cursor", declared)
			}
			if got := p.seqOf(t, mine); got != 7 {
				t.Errorf("its seq moved to %d", got)
			}
		})
	}
	// And the rule is not vacuous: the exact name is adopted.
	p := newLineageProbe(t, filepath.Join(adoptRoot, "src"), nil)
	p.write(t, adoptRoot, "cursor", "cursor-session")
	if env := p.trace(map[string]string{"TOKENDROP_HARNESS": "cursor"}); env == nil || env.SessionID != "cursor-session" {
		t.Fatalf("the exactly-matching name was not adopted: %+v", env)
	}
}

// A sidecar recording no harness at all — one written by a version before
// this rule — cannot be shown to belong to anyone, so it is not adopted by
// the walk. It is still reachable by the host that names it outright.
func TestASidecarWithNoHarnessIsNotAdoptedByTheWalk(t *testing.T) {
	p := newLineageProbe(t, filepath.Join(adoptRoot, "src"), nil)
	path := lineagePath(adoptSessions, adoptRoot)
	if err := updateLineage(p.ops.hook, path, p.now, func(l *lineageFile) {
		l.SessionID, l.Seq = "nameless-session", 7
	}); err != nil {
		t.Fatal(err)
	}

	env := p.trace(map[string]string{"TOKENDROP_HARNESS": "cursor"})

	if env == nil || env.SessionID == "nameless-session" {
		t.Fatalf("a sidecar naming no harness was adopted: %+v", env)
	}
	if got := p.seqOf(t, path); got != 7 {
		t.Errorf("its seq moved to %d", got)
	}
}
