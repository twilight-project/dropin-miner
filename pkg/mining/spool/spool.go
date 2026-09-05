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
	"crypto/rand"
	"encoding/binary"
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
	"sync"
	"time"
)

// Record is one spooled observation plus the delivery context needed to
// obtain the right capability for it.
type Record struct {
	ClientRecordID string          `json:"client_record_id"`
	SlotID         uint64          `json:"slot_id"`
	TargetEpoch    uint64          `json:"target_epoch"`
	Observation    json.RawMessage `json:"observation"`
	SpooledAt      time.Time       `json:"spooled_at"`
	// Attempts is advisory: backoff state survives restarts so a
	// permanently failing record does not hot-loop after every reboot.
	Attempts int `json:"attempts"`
}

// Spool is a directory of durable records.
type Spool struct {
	dir        string
	quarantine string

	mu sync.Mutex
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
	return &Spool{dir: dir, quarantine: quarantine}, nil
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

// Write durably persists a record: temp file, fsync, atomic rename, and
// an fsync of the directory so the rename itself survives power loss.
// It returns only after the record is genuinely on disk (PX-15).
func (s *Spool) Write(rec *Record) error {
	if rec.ClientRecordID == "" {
		return errors.New("spool: refusing to write a record without a client_record_id")
	}
	if rec.SpooledAt.IsZero() {
		rec.SpooledAt = time.Now().UTC()
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("spool: encode record: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("spool: stage record: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("spool: chmod record: %w", err)
	}
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("spool: write record: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("spool: sync record: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("spool: close record: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(s.dir, s.filename(rec))); err != nil {
		return fmt.Errorf("spool: install record: %w", err)
	}
	return s.syncDir()
}

// syncDir makes the rename durable, not just the file contents.
func (s *Spool) syncDir() error {
	d, err := os.Open(s.dir)
	if err != nil {
		return fmt.Errorf("spool: open dir: %w", err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		// Some filesystems refuse directory fsync; the rename is still
		// atomic, so this is not fatal.
		return nil //nolint:nilerr // best-effort durability of the rename
	}
	return nil
}

// Pending lists spooled records oldest-first (UUIDv7 sorts by time, and
// the filename carries it). This IS the restart scan: it reads whatever
// is on disk, with no in-memory state required.
func (s *Spool) Pending() ([]*Record, error) {
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
			_ = os.Rename(filepath.Join(s.dir, name), filepath.Join(s.quarantine, name))
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
	err := os.Remove(filepath.Join(s.dir, s.filename(rec)))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("spool: remove record: %w", err)
	}
	return nil
}

// Quarantine moves a record aside without deleting it: used for
// permanent refusals (409 evidence conflict) where retrying can never
// succeed but the evidence must remain inspectable.
func (s *Spool) Quarantine(rec *Record, reason string) error {
	name := s.filename(rec)
	stamp := strconv.FormatInt(time.Now().UTC().UnixNano(), 10)
	target := filepath.Join(s.quarantine, stamp+"-"+sanitize(reason)+"-"+name)
	if err := os.Rename(filepath.Join(s.dir, name), target); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("spool: quarantine record: %w", err)
	}
	return nil
}

// Touch rewrites a record with an incremented attempt count so backoff
// state survives a restart.
func (s *Spool) Touch(rec *Record) error {
	rec.Attempts++
	return s.Write(rec)
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
