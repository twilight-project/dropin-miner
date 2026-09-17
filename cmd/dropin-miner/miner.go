package main

// The drop-in miner's shared pieces: what `search`, `flush` and the hooks
// pass between themselves through the filesystem, since no process
// outlives a tool call.
//
//   intake    one small JSON file per served search (request id, timings,
//             status). Written by `search` the moment the router answers,
//             promoted into the spool by the next `flush` under whatever
//             (slot, epoch) that flush finds joined. Metadata only — no
//             query, no result, nothing the router did not already record.
//   sidecar   one JSON file per workspace holding the lineage the hooks
//             learned (hashed session/turn/call ids, the window generation,
//             the assistant text just before the search). `search` reads
//             it when no bridge arrived in its environment, so a host that
//             cannot rewrite a shell command still threads its searches.
//   lock      one flock/handle per flush so two agents searching at once
//             queue rather than double-submit.
//   detach    how `search` and the hooks start a flush without waiting for
//             it: a child with no terminal that exits when the work is done.
//
// None of this is a daemon. A flush that finds nothing to do exits in
// well under a second; a machine with no searches runs nothing at all.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/config"
	"github.com/twilight-project/dropin-miner/pkg/fsx"
	"github.com/twilight-project/dropin-miner/pkg/mining/spool"
	"github.com/twilight-project/dropin-miner/pkg/observe"
)

const (
	intakeVersion  = 1
	lineageVersion = 1
	// lineageMaxAge is how long a workspace's lineage stays believable. A
	// sidecar older than this describes a conversation that is over; a
	// search after it belongs to a new one, so `search` falls back to its
	// per-shell session identity rather than thread into the old lane.
	lineageMaxAge = 12 * time.Hour
	// lineageWalkUp bounds how many parent directories `search` climbs
	// looking for the workspace a hook wrote for. Hooks key on the project
	// root; agents run commands from subdirectories of it.
	lineageWalkUp = 8
)

// loadConfig resolves the config exactly as describeConfigSource does
// (ruling D-R1: -config, TOKENDROP_CONFIG, ./tokendrop.toml, the
// installation's own config, then defaults) and returns the full config so
// the miner can read [miner] and [mining]. pkg/config is not changed: the
// resolved path, once found, is handed to config.Load as an explicit
// -config, which is exactly what makes it required to exist — a guarantee
// this function has already checked for the two soft-discovered steps
// before choosing them.
func loadConfig(cfgPath string, getenv func(string) string) (*config.Config, string, error) {
	src := describeConfigSource(cfgPath, getenv)
	args := []string{}
	if src != "" {
		args = []string{"-config", src}
	}
	cfg, _, err := config.Load(args, getenv)
	if err != nil {
		return nil, src, err
	}
	return cfg, src, nil
}

// ── intake ──────────────────────────────────────────────────────────────

// intakeRecord is everything `search` keeps about one served request. It
// is the search-router observation shape the proxy's observer would have
// produced, minus the parsing: the CLI has the response in hand.
type intakeRecord struct {
	ClientRecordID string    `json:"client_record_id,omitempty"`
	V              int       `json:"v"`
	RequestID      string    `json:"request_id"`
	Host           string    `json:"host,omitempty"`
	StatusCode     int       `json:"status_code"`
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at"`
	ChosenProvider string    `json:"chosen_provider,omitempty"`
}

// observation renders the record as the observer would have: the search
// router profile, the request id as the provider event id, a complete
// outcome. promote.Build does the rest and rejects anything it should.
func (r intakeRecord) observation() *observe.Observation {
	return &observe.Observation{
		Profile:       observe.ProfileSearchRouter,
		GenerationID:  r.RequestID,
		ResolvedModel: r.ChosenProvider,
		StatusCode:    r.StatusCode,
		StartedAt:     r.StartedAt,
		FinishedAt:    r.FinishedAt,
		SawDone:       true,
		Outcome:       observe.Complete(observe.TerminationDone),
	}
}

func randomSuffix() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000)
	}
	return hex.EncodeToString(b[:])
}

// writeIntake durably records one served request: temp file, then rename,
// 0600, so a flush never reads a half-written record. The filename sorts
// by time so promotion keeps arrival order.
func writeIntake(dir string, rec intakeRecord) (string, error) {
	if rec.RequestID == "" {
		return "", errors.New("intake: request id is required")
	}
	if rec.ClientRecordID == "" {
		id, err := spool.NewClientRecordID()
		if err != nil {
			return "", err
		}
		rec.ClientRecordID = id
	}
	rec.V = intakeVersion
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("%020d-%s.json", rec.FinishedAt.UnixNano(), randomSuffix())
	final := filepath.Join(dir, name)
	if err := fsx.WriteFileAtomic(dir, name, data, 0o600); err != nil {
		return "", err
	}
	return final, nil
}

type intakeFile struct {
	path string
	rec  intakeRecord
}

// readIntake lists the records waiting for promotion, oldest first. A
// file that will not parse is reported by path and left alone; a flush
// must never delete what it did not understand.
func readIntake(dir string) (records []intakeFile, unreadable []string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(dir, name)
		data, rerr := os.ReadFile(path) // #nosec G304 -- our own intake dir
		if rerr != nil {
			unreadable = append(unreadable, path)
			continue
		}
		var rec intakeRecord
		if jerr := json.Unmarshal(data, &rec); jerr != nil || rec.V != intakeVersion || rec.RequestID == "" {
			unreadable = append(unreadable, path)
			continue
		}
		records = append(records, intakeFile{path: path, rec: rec})
	}
	return records, unreadable, nil
}

// ── sidecar ─────────────────────────────────────────────────────────────

// sidecar is a workspace's lineage as the hooks last saw it. Every
// identifier is already hashed; the raw host ids never reach disk.
type lineageFile struct {
	V         int            `json:"v"`
	Harness   string         `json:"harness,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
	TurnID    string         `json:"turn_id,omitempty"`
	CallID    string         `json:"call_id,omitempty"`
	Window    string         `json:"window,omitempty"`
	Seq       int            `json:"seq,omitempty"`
	History   []traceHistory `json:"history,omitempty"`
	UpdatedAt time.Time      `json:"updated_at"`
}

// lineagePath keys a sidecar on the workspace root, hashed like every
// other identifier so a listing of the sessions directory reveals no
// project paths.
func lineagePath(dir, workspace string) string {
	return filepath.Join(dir, traceHash("workspace|"+filepath.Clean(workspace))+".json")
}

func loadLineage(ops hookOps, path string) (*lineageFile, bool) {
	data, err := ops.readFile(path)
	if err != nil {
		return nil, false
	}
	var sc lineageFile
	if err := json.Unmarshal(data, &sc); err != nil || sc.V != lineageVersion {
		return nil, false
	}
	return &sc, true
}

// saveLineage writes atomically (temp + rename) and 0600, preparing
// complete history entries before they reach the workspace sidecar.
func saveLineage(ops hookOps, path string, sc *lineageFile, now time.Time) error {
	sc.V = lineageVersion
	sc.UpdatedAt = now
	for i := range sc.History {
		sc.History[i].Text = prepareTraceText(sc.History[i].Text)
	}
	if err := ops.mkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(sc)
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, ops.pid)
	if err := ops.writeFile(tmp, data, 0o600); err != nil {
		return err
	}
	return ops.rename(tmp, path)
}

// updateLineage applies one change under read-modify-write. Two hooks
// racing on one workspace lose at most one update, never the file.
func updateLineage(ops hookOps, path string, now time.Time, apply func(*lineageFile)) error {
	sc, ok := loadLineage(ops, path)
	if !ok {
		sc = &lineageFile{}
	}
	apply(sc)
	return saveLineage(ops, path, sc, now)
}

// sameHarness compares two harness names. TOKENDROP_HARNESS is written by a
// hook and may be set by a participant, so it is compared the way a name is
// rather than the way bytes are. An empty name matches nothing: a sidecar
// recording no harness cannot be shown to belong to anyone.
func sameHarness(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	return a != "" && b != "" && strings.EqualFold(a, b)
}

// lineageForCwd finds the sidecar governing a directory: the directory
// itself, then its parents, a bounded number of hops. A stale sidecar is
// treated as absent.
//
// It answers nothing unless the search can say WHOSE session it is making
// and the sidecar agrees (#97). harness is the searching host's own name, as
// TOKENDROP_HARNESS gives it.
//
// Both halves of that follow from what this walk is for. Every host that
// carries its lineage in a variable — the bridge, or TOKENDROP_LINEAGE
// naming the file outright — is already served before this is reached; the
// walk exists only to find a session's own sidecar again when that variable
// was lost, which is what happens when the search runs from a subshell. So a
// search arriving here with NOTHING naming a session is not one that lost its
// variable: it is one from a host that never had a lineage channel, or from a
// person at a terminal. Looking up the tree on its behalf cannot find its
// session, because its session wrote no sidecar — it can only find somebody
// else's. That is precisely what happened: a Cursor CLI search in a
// subdirectory, and a plain search from a terminal in the same tree, both
// reached the router as harness=claude-code carrying a Claude Code session's
// id and its assistant text, and advanced that session's seq.
//
// A mismatch stops the walk rather than climbing past it. A nearer sidecar
// belonging to someone else says this directory is theirs, and claiming a
// more distant one because its name matches would be a guess; the safe
// direction is an honest per-shell identity rather than a confident wrong
// one. Stopping is also what a stale sidecar already does.
func lineageForCwd(ops hookOps, dir, cwd, harness string, now time.Time) (*lineageFile, string) {
	if dir == "" || cwd == "" || harness == "" {
		return nil, ""
	}
	at := filepath.Clean(cwd)
	for i := 0; i <= lineageWalkUp; i++ {
		path := lineagePath(dir, at)
		if sc, ok := loadLineage(ops, path); ok {
			if now.Sub(sc.UpdatedAt) > lineageMaxAge {
				return nil, ""
			}
			if !sameHarness(sc.Harness, harness) {
				return nil, ""
			}
			return sc, path
		}
		parent := filepath.Dir(at)
		if parent == at {
			break
		}
		at = parent
	}
	return nil, ""
}

// envelope renders the sidecar as the trace `search` will send. The
// sequence counter is the sidecar's, bumped by the caller that saves it.
func (sc *lineageFile) envelope() *traceEnvelope {
	if sc == nil || sc.SessionID == "" {
		return nil
	}
	env := &traceEnvelope{
		V:         traceVersion,
		Harness:   sc.Harness,
		SessionID: sc.SessionID,
		TurnID:    sc.TurnID,
		CallID:    sc.CallID,
		Window:    sc.Window,
		Seq:       sc.Seq,
	}
	if len(sc.History) > 0 {
		env.History = append([]traceHistory(nil), sc.History...)
	}
	return env
}

// ── flush stamp ─────────────────────────────────────────────────────────

// flushStamp remembers what the last flush learned from the AS, so the
// next one within miner.flush_interval can skip the target/join round
// trip and go straight to promotion and delivery.
type flushStamp struct {
	V           int       `json:"v"`
	SlotID      uint64    `json:"slot_id"`
	TargetEpoch uint64    `json:"target_epoch"`
	LastAS      time.Time `json:"last_as"`
	LastFlush   time.Time `json:"last_flush"`
}

func minerRoot(m config.Miner) string { return filepath.Dir(m.IntakeDir) }

// flushStampPath is the stamp under mining.state_dir, which a sandboxed agent
// can write (the miner root is not). With no state directory there is no
// stamp: every read is the zero stamp and every write fails, which costs a
// target lookup per flush and nothing else. A config with an authorization
// server always has a state directory (pkg/config refuses one without).
func flushStampPath(m config.Mining) string {
	if m.StateDir == "" {
		return ""
	}
	return filepath.Join(m.StateDir, "flush.json")
}

// legacyFlushStampPath is where 0.2.9 and earlier kept the stamp. It is read
// only as a starting value while the new stamp is absent, never written, and
// removed only by a purge.
func legacyFlushStampPath(m config.Miner) string { return filepath.Join(minerRoot(m), "flush.json") }

func flushLockPath(m config.Miner) string { return filepath.Join(minerRoot(m), "flush.lock") }

// loadFlushStamp reads the stamp, falling back to the legacy one only when the
// new one does not exist.
func loadFlushStamp(path, legacy string) flushStamp {
	if path != "" {
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			return readFlushStamp(path)
		}
	}
	return readFlushStamp(legacy)
}

func readFlushStamp(path string) flushStamp {
	data, err := os.ReadFile(path) // #nosec G304 -- our own state dir
	if err != nil {
		return flushStamp{}
	}
	var st flushStamp
	if json.Unmarshal(data, &st) != nil || st.V != 1 {
		return flushStamp{}
	}
	return st
}

func writeFlushStamp(path string, st flushStamp) error {
	if path == "" {
		return errors.New("mining.state_dir is empty, so the flush stamp has nowhere to live")
	}
	st.V = 1
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ── recorded-search epoch (D's own evidence, not F's stamp) ─────────────
//
// doctor's recording check needs to tell a search that ran in the epoch
// the AS reports as current from a hook flush that touched nothing: F's
// flush stamp updates on either, so it cannot make that distinction alone.
// This is D's own file, beside the intake records a search writes but
// never counted as one: recordedEpochFile has no .json suffix, so neither
// readIntake's promotion scan nor countIntakeJSON's doctor count ever see
// it. It names the target epoch F's flush stamp held the moment a search
// was last recorded — read-only, since F owns the stamp itself — not the
// epoch a fresh AS call would report right now, so it can lag the real
// target between flushes. That lag can only ever make doctor conclude "no
// recent activity" where "activity, unresolved" was warranted, never the
// reverse, which is the direction a check that never says NO may err in.
const recordedEpochFile = "recorded_epoch"

// recordSearchEpoch is best-effort and silent: a failure here diagnoses
// nothing about the search that just ran, promotes nothing and blocks
// nothing, so it is not worth a health record or a line to the user —
// only a future `doctor` run losing one input it would rather have had.
func recordSearchEpoch(dir string, epoch uint64) {
	_ = fsx.WriteFileAtomic(dir, recordedEpochFile, []byte(strconv.FormatUint(epoch, 10)), 0o600)
}

// readSearchEpoch reads back what recordSearchEpoch wrote. Absent — no
// search has ever been recorded here, or its marker predates this
// feature — is not an error; present, ok=false means the file exists but
// could not be parsed, fed to the same undetermined path doctor's other
// unreadable inputs use, rather than silently read as "nothing happened."
func readSearchEpoch(dir string) (epoch uint64, present bool, err error) {
	data, rerr := os.ReadFile(filepath.Join(dir, recordedEpochFile)) // #nosec G304 -- this installation's own intake dir
	if errors.Is(rerr, fs.ErrNotExist) {
		return 0, false, nil
	}
	if rerr != nil {
		return 0, false, rerr
	}
	n, perr := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if perr != nil {
		return 0, false, fmt.Errorf("recorded_epoch: %w", perr)
	}
	return n, true, nil
}

// ── detached flush ──────────────────────────────────────────────────────

// startFlush launches `flush` as a detached child of this process and
// returns without waiting. Best effort by contract: a machine that cannot
// spawn keeps its intake on disk for the next search or session to flush.
func startFlush(cfgPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"flush"}
	if cfgPath != "" {
		args = append(args, "-config", cfgPath)
	}
	return spawnDetached(exe, args)
}
