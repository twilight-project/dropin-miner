// Package spool is the proxy's durable observation queue (PX-15). Its
// whole reason to exist: an observation must survive a crash between
// "the inference finished" and "the AS durably accepted it".
//
// Ordering is load-bearing and deliberately strict:
//
//	promotion → stable client_record_id → DURABLE SPOOL WRITE →
//	best-effort wakeup → collector → ACK → removal
//
// A record is removed ONLY on ACCEPTED or ALREADY_ACCEPTED. Anything
// else leaves it on disk for the restart scan to find.
//
// Nothing here runs on a request-reachable goroutine: the mining plane
// owns this queue, so the file I/O can never touch inference latency.
package spool

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/twilight-project/dropin-miner/pkg/fsx"
)

// Record is one spooled observation plus the delivery context needed to
// obtain the right capability for it.
type Record struct {
	ClientRecordID string          `json:"client_record_id"`
	SlotID         uint64          `json:"slot_id"`
	TargetEpoch    uint64          `json:"target_epoch"`
	Observation    json.RawMessage `json:"observation"`
	SpooledAt      time.Time       `json:"spooled_at"`
	// Attempts and NextAttemptAt are the durable retry authority.
	Attempts       int       `json:"attempts"`
	NextAttemptAt  time.Time `json:"next_attempt_at,omitempty"`
	TerminalReason string    `json:"terminal_reason,omitempty"`
	RemovalPending bool      `json:"removal_pending,omitempty"`
}

// Spool is a directory of durable records.
type Spool struct {
	dir        string
	quarantine string

	mu                sync.Mutex
	locations         map[string]string
	quarantineUnknown int
	move              func(string, string) error
	remove            func(string) error
	write             func(string, string, []byte, fs.FileMode) error
}

// Open prepares the spool directories (0700: records carry no secrets,
// but they are participant activity metadata).
func Open(dir string) (*Spool, error) {
	if dir == "" {
		return nil, errors.New("spool: directory is empty")
	}
	quarantine := filepath.Join(dir, "quarantine")
	for _, d := range []string{dir, quarantine} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("spool: create %s: %w", d, err)
		}
	}
	s := &Spool{dir: dir, quarantine: quarantine, locations: make(map[string]string), move: fsx.MoveFileDurable, remove: fsx.RemoveFileDurable, write: fsx.WriteFileAtomic}
	if err := s.reconstruct(); err != nil {
		return nil, err
	}
	return s, nil
}

// OpenExisting opens a spool for non-mutating inspection. Unlike Open it
// never creates the queue or its quarantine directory; diagnostics must not
// change the participant's durable state merely to count records.
func OpenExisting(dir string) (*Spool, error) {
	if dir == "" {
		return nil, errors.New("spool: directory is empty")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("spool: stat %s: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("spool: %s is not a directory", dir)
	}
	return &Spool{dir: dir, quarantine: filepath.Join(dir, "quarantine")}, nil
}

// NewClientRecordID mints the stable transport identity (contract §49):
// UUIDv7 — sortable, collision-resistant, carrying no PII and no
// credential. Generated ONCE per observation and never regenerated on
// retry, which is what makes AS-side idempotency work.
func NewClientRecordID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("spool: generate record id: %w", err)
	}
	// UUIDv7 layout: the low 48 bits of the Unix-millisecond timestamp,
	// big-endian, then random bits with the version/variant nibbles.
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(time.Now().UnixMilli()))
	copy(b[0:6], ts[2:8])
	b[6] = (b[6] & 0x0F) | 0x70 // version 7
	b[8] = (b[8] & 0x3F) | 0x80 // RFC 4122 variant
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

// filename encodes epoch and id so the restart scan can group by target
// without opening every file.
func (s *Spool) filename(rec *Record) string {
	return fmt.Sprintf("%d-%d-%s.json", rec.SlotID, rec.TargetEpoch, rec.ClientRecordID)
}

// ErrEvidenceConflict means a stable identity has incompatible evidence or context.
var ErrEvidenceConflict = errors.New("spool: evidence identity conflict")
var ErrDuplicateIdentity = errors.New("spool: identity at multiple active locations")

// reconstruct is the single startup index scan. Files remain authoritative.
// This mutex/index is not a cross-process protocol; CLI flush owns its lock.
func (s *Spool) reconstruct() error {
	for _, dir := range []string{s.dir, s.quarantine} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || strings.HasPrefix(entry.Name(), ".tmp-") || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			name := entry.Name()
			if dir == s.quarantine {
				name = filepath.Join("quarantine", name)
			}
			rec, err := s.read(name)
			if err != nil {
				if dir == s.quarantine {
					s.quarantineUnknown++
				}
				continue
			}
			if _, exists := s.locations[rec.ClientRecordID]; exists {
				return fmt.Errorf("%w: %s", ErrDuplicateIdentity, rec.ClientRecordID)
			}
			s.locations[rec.ClientRecordID] = name
		}
	}
	return nil
}

func quarantined(name string) bool { return filepath.Dir(name) == "quarantine" }

// State describes durable custody, including terminal active records and
// quarantined evidence. It never moves files or creates state.
type State struct {
	RetryBacklog   int
	Queued         int
	Terminal       int
	Quarantined    int
	RemovalPending int
}

func (s *Spool) State() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := State{Quarantined: s.quarantineUnknown}
	for _, name := range s.locations {
		if quarantined(name) {
			state.Quarantined++
			continue
		}
		rec, err := s.read(name)
		if err != nil {
			return state, err
		}
		state.Queued++
		if rec.TerminalReason == "" && !rec.RemovalPending && (rec.Attempts > 0 || !rec.NextAttemptAt.IsZero()) {
			state.RetryBacklog++
		}
		if rec.TerminalReason != "" {
			state.Terminal++
		}
		if rec.RemovalPending {
			state.RemovalPending++
		}
	}
	return state, nil
}

func sameEvidence(a, b json.RawMessage) bool {
	decode := func(raw json.RawMessage) (any, error) {
		var v any
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		if err := d.Decode(&v); err != nil {
			return nil, err
		}
		if !json.Valid(raw) {
			return nil, errors.New("invalid observation")
		}
		return v, nil
	}
	av, ae := decode(a)
	bv, be := decode(b)
	return ae == nil && be == nil && reflect.DeepEqual(av, bv)
}

// Enqueue publishes new evidence or recognizes an identical stable-ID replay.
// A newly resolved target is delivery context, never an evidence conflict.
func (s *Spool) Enqueue(rec *Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.ClientRecordID == "" || strings.ContainsAny(rec.ClientRecordID, `/\\`) {
		return fmt.Errorf("spool: invalid client_record_id: %w", fs.ErrInvalid)
	}
	if s.locations == nil {
		return fmt.Errorf("spool: inspection-only handle: %w", fs.ErrPermission)
	}
	if name, ok := s.locations[rec.ClientRecordID]; ok {
		existing, err := s.read(name)
		if err != nil {
			return err
		}
		if !sameEvidence(existing.Observation, rec.Observation) {
			return ErrEvidenceConflict
		}
		// Retry a directory durability barrier after uncertain publication. Do
		// not rewrite the original evidence, delivery context, or retry state.
		if quarantined(name) {
			// The quarantine file is already the durable authority. A replay
			// confirms custody by identity and payload only; it must not try
			// to move the record back to the active namespace.
			return nil
		}
		return s.syncDir()
	}
	if rec.SpooledAt.IsZero() {
		rec.SpooledAt = time.Now().UTC()
	}
	return s.persist(rec, s.filename(rec), true)
}

// Rewrite only replaces delivery state at the existing durable location.
func (s *Spool) Rewrite(rec *Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	name, ok := s.locations[rec.ClientRecordID]
	if !ok {
		return fmt.Errorf("spool: rewrite absent record: %w", fs.ErrNotExist)
	}
	previous, err := s.read(name)
	if err != nil {
		return err
	}
	if quarantined(name) || (previous.TerminalReason != "" && rec.TerminalReason != previous.TerminalReason) || (previous.RemovalPending && !rec.RemovalPending) || (rec.TerminalReason != "" && rec.RemovalPending) || previous.SlotID != rec.SlotID || previous.TargetEpoch != rec.TargetEpoch || !sameEvidence(previous.Observation, rec.Observation) || !previous.SpooledAt.Equal(rec.SpooledAt) {
		return ErrEvidenceConflict
	}
	return s.persist(rec, name, false)
}

func (s *Spool) persist(rec *Record, name string, exclusive bool) error {
	payload, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if exclusive {
		err = fsx.WriteFileExclusive(s.dir, name, payload, 0o600)
	} else {
		err = s.write(s.dir, name, payload, 0o600)
	}
	var stage *fsx.StageError
	if err == nil || (errors.As(err, &stage) && stage.Published) {
		s.locations[rec.ClientRecordID] = name
	}
	return err
}

func (s *Spool) syncDir() error {
	err := fsx.SyncDirectory(s.dir)
	if errors.Is(err, fsx.ErrDirectorySyncUnsupported) {
		return nil
	} // Windows publication uses write-through.
	return err
}

// Pending lists spooled records oldest-first (UUIDv7 sorts by time, and
// the filename carries it). This IS the restart scan: it reads whatever
// is on disk, with no in-memory state required.
func (s *Spool) Pending() ([]*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("spool: scan: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".tmp-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	records := make([]*Record, 0, len(names))
	for _, name := range names {
		rec, err := s.read(name)
		if err != nil {
			// A corrupt record must not wedge the queue; quarantine it
			// and keep going (it is evidence, not a crash).
			destination := filepath.Join("quarantine", name)
			moveErr := s.move(filepath.Join(s.dir, name), filepath.Join(s.dir, destination))
			var stage *fsx.StageError
			if moveErr == nil || (errors.As(moveErr, &stage) && stage.Published) {
				s.quarantineUnknown++
				for id, location := range s.locations {
					if location == name {
						s.locations[id] = destination
						s.quarantineUnknown--
					}
				}
			}
			if moveErr != nil {
				return nil, moveErr
			}
			continue
		}
		records = append(records, rec)
	}
	return records, nil
}

func (s *Spool) read(name string) (*Record, error) {
	raw, err := os.ReadFile(filepath.Join(s.dir, name)) // #nosec G304 -- name comes from our own directory listing
	if err != nil {
		return nil, err
	}
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, err
	}
	if rec.ClientRecordID == "" {
		return nil, errors.New("spool: record has no client_record_id")
	}
	return &rec, nil
}

// Remove deletes a delivered record. Called ONLY after ACCEPTED or
// ALREADY_ACCEPTED (§58 spool rule).
func (s *Spool) Remove(rec *Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	name, ok := s.locations[rec.ClientRecordID]
	if !ok {
		return nil
	}
	if quarantined(name) {
		return ErrEvidenceConflict
	}
	err := s.remove(filepath.Join(s.dir, name))
	if err == nil {
		delete(s.locations, rec.ClientRecordID)
		return nil
	}
	var stage *fsx.StageError
	if errors.As(err, &stage) && stage.Published {
		// Restore the complete stable record after uncertain local removal. The
		// ACK marker permits a fresh collector to retry cleanup without Submit.
		payload, encodeErr := json.Marshal(rec)
		if encodeErr != nil {
			return errors.Join(err, encodeErr)
		}
		restoreErr := s.write(s.dir, name, payload, 0o600)
		if _, statErr := os.Stat(filepath.Join(s.dir, name)); errors.Is(statErr, fs.ErrNotExist) {
			delete(s.locations, rec.ClientRecordID)
		}
		return errors.Join(err, restoreErr)
	}
	return err
}

// Quarantine moves terminal evidence and keeps its identity indexed. A
// published-but-unsynced move updates the visible index but returns an error.
func (s *Spool) Quarantine(rec *Record, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	name, ok := s.locations[rec.ClientRecordID]
	if !ok {
		return fs.ErrNotExist
	}
	if quarantined(name) {
		// Quarantined evidence is already terminal custody. A retrying
		// collector only needs the stable-ID index to keep it out of the
		// submission path.
		return nil
	}
	target := filepath.Join("quarantine", sanitize(reason)+"-"+name)
	err := s.move(filepath.Join(s.dir, name), filepath.Join(s.dir, target))
	var stage *fsx.StageError
	if err == nil || (errors.As(err, &stage) && stage.Published) {
		s.locations[rec.ClientRecordID] = target
	}
	return err
}

// Touch rewrites a record with an incremented attempt count so backoff
// state survives a restart.
func (s *Spool) Touch(rec *Record) error {
	updated := *rec
	updated.Attempts++
	if err := s.Rewrite(&updated); err != nil {
		return err
	}
	*rec = updated
	return nil
}

// Len reports the queue depth (diagnostics).
// Count returns how many records are queued, without reading or moving any
// of them.
//
// Len goes through Pending, and Pending QUARANTINES a record it cannot parse
// — which is right for the collector that owns the queue and wrong for anyone
// else. A second process asking "how deep is the backlog" must not be able to
// move another process's files, so this counts names and stops there.
//
// The cost is that it counts a corrupt record the collector would quarantine.
// That is the safer error: it over-reports a backlog by the number of files
// that are already broken, rather than mutating a queue it does not own.
func (s *Spool) Count() (int, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, fmt.Errorf("spool: scan: %w", err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".tmp-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		n++
	}
	return n, nil
}

func (s *Spool) Len() (int, error) {
	recs, err := s.Pending()
	if err != nil {
		return 0, err
	}
	return len(recs), nil
}

func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	if len(out) > 32 {
		out = out[:32]
	}
	return string(out)
}
