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
	"strings"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/config"
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

// loadConfig is resolveListen's whole-config sibling: the same resolution
// order (flag, TOKENDROP_CONFIG, ./tokendrop.toml, defaults) returning the
// full config so the miner can read [miner] and [mining].
func loadConfig(cfgPath string, getenv func(string) string) (*config.Config, string, error) {
	args := []string{}
	if cfgPath != "" {
		args = []string{"-config", cfgPath}
	}
	cfg, _, err := config.Load(args, getenv)
	if err != nil {
		return nil, describeConfigSource(cfgPath, getenv), err
	}
	return cfg, describeConfigSource(cfgPath, getenv), nil
}

// ── intake ──────────────────────────────────────────────────────────────

// intakeRecord is everything `search` keeps about one served request. It
// is the search-router observation shape the proxy's observer would have
// produced, minus the parsing: the CLI has the response in hand.
type intakeRecord struct {
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
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
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

// saveLineage writes atomically (temp + rename) and 0600. The history is
// capped by the same rule the envelope obeys, so a sidecar can never grow
// past what one envelope may carry.
func saveLineage(ops hookOps, path string, sc *lineageFile, now time.Time) error {
	sc.V = lineageVersion
	sc.UpdatedAt = now
	for i := range sc.History {
		if len(sc.History[i].Text) > traceHistoryCap {
			sc.History[i].Text = sc.History[i].Text[len(sc.History[i].Text)-traceHistoryCap:]
		}
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

// lineageForCwd finds the sidecar governing a directory: the directory
// itself, then its parents, a bounded number of hops. A stale sidecar is
// treated as absent.
func lineageForCwd(ops hookOps, dir, cwd string, now time.Time) (*lineageFile, string) {
	if dir == "" || cwd == "" {
		return nil, ""
	}
	at := filepath.Clean(cwd)
	for i := 0; i <= lineageWalkUp; i++ {
		path := lineagePath(dir, at)
		if sc, ok := loadLineage(ops, path); ok {
			if now.Sub(sc.UpdatedAt) > lineageMaxAge {
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

func flushStampPath(m config.Miner) string { return filepath.Join(minerRoot(m), "flush.json") }
func flushLockPath(m config.Miner) string  { return filepath.Join(minerRoot(m), "flush.lock") }

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
