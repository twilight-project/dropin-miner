package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/twilight-project/dropin-miner/pkg/redact"
)

const (
	healthVersion   = 1
	healthDetailCap = 512
	healthDecision  = "health_decision.json"
	healthCapture   = "health_capture.json"
	healthFlush     = "health_flush.json"
)

// HealthComponent is one independently persisted source of degradation.
type HealthComponent string

const (
	HealthDecision HealthComponent = "decision"
	HealthCapture  HealthComponent = "capture"
	HealthFlush    HealthComponent = "flush"
)

// HealthReason is a machine-stable explanation for unresolved component
// degradation. Authentication failures belong to flush because that is where
// they prevent mining progress.
type HealthReason string

const (
	HealthDecisionUnreadable HealthReason = "decision_unreadable"
	HealthIntakeUnwritable   HealthReason = "intake_unwritable"
	HealthSandboxRestricted  HealthReason = "sandbox_restricted"
	HealthFlushSpawnFailed   HealthReason = "flush_spawn_failed"
	HealthAuthUnavailable    HealthReason = "auth_state_unavailable"
	HealthSubmissionFailed   HealthReason = "submission_failed"
	HealthSpoolBacklog       HealthReason = "spool_backlog"
)

// HealthRecord is the bounded persistent last-known degradation for one
// component. It is not a heartbeat or event history.
type HealthRecord struct {
	Version   int             `json:"version"`
	Component HealthComponent `json:"component"`
	Reason    HealthReason    `json:"reason"`
	Detail    string          `json:"detail,omitempty"`
	At        time.Time       `json:"at"`
}

func healthFile(component HealthComponent) (string, error) {
	switch component {
	case HealthDecision:
		return healthDecision, nil
	case HealthCapture:
		return healthCapture, nil
	case HealthFlush:
		return healthFlush, nil
	default:
		return "", fmt.Errorf("auth: unknown health component %q", component)
	}
}

func validHealthReason(reason HealthReason) bool {
	switch reason {
	case HealthDecisionUnreadable, HealthIntakeUnwritable, HealthSandboxRestricted,
		HealthFlushSpawnFailed, HealthAuthUnavailable, HealthSubmissionFailed,
		HealthSpoolBacklog:
		return true
	default:
		return false
	}
}

func boundHealthDetail(detail string) string {
	detail = redact.String(strings.Join(strings.Fields(detail), " "))
	if len(detail) > healthDetailCap {
		cut := healthDetailCap
		for cut > 0 && !utf8.ValidString(detail[:cut]) {
			cut--
		}
		return detail[:cut]
	}
	return detail
}

// MarkHealth records unresolved degradation for exactly one component. Each
// component has its own file, so a capture write cannot erase a flush record
// written concurrently by a detached process.
func (s *Store) MarkHealth(component HealthComponent, reason HealthReason, detail string) error {
	name, err := healthFile(component)
	if err != nil {
		return err
	}
	if !validHealthReason(reason) {
		return fmt.Errorf("auth: unknown health reason %q", reason)
	}
	rec := HealthRecord{
		Version:   healthVersion,
		Component: component,
		Reason:    reason,
		Detail:    boundHealthDetail(detail),
		At:        time.Now().UTC(),
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("auth: encode health: %w", err)
	}
	return s.saveStateFile(name, raw)
}

// LoadHealth returns one component's unresolved record.
func (s *Store) LoadHealth(component HealthComponent) (HealthRecord, bool, error) {
	name, err := healthFile(component)
	if err != nil {
		return HealthRecord{}, false, err
	}
	raw, err := s.readSecret(name)
	if errors.Is(err, fs.ErrNotExist) {
		return HealthRecord{}, false, nil
	}
	if err != nil {
		return HealthRecord{}, false, err
	}
	var rec HealthRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return HealthRecord{}, false, fmt.Errorf("auth: decode health: %w", err)
	}
	if rec.Version != healthVersion || rec.Component != component || !validHealthReason(rec.Reason) {
		return HealthRecord{}, false, fmt.Errorf("auth: invalid health record for %s", component)
	}
	return rec, true, nil
}

// HealthRecords reads all three components without changing any of them.
// Results are returned in the stable decision, capture, flush order.
func (s *Store) HealthRecords() ([]HealthRecord, error) {
	components := []HealthComponent{HealthDecision, HealthCapture, HealthFlush}
	records := make([]HealthRecord, 0, len(components))
	var readErrs []error
	for _, component := range components {
		rec, ok, err := s.LoadHealth(component)
		if err != nil {
			readErrs = append(readErrs, fmt.Errorf("%s: %w", component, err))
			continue
		}
		if ok {
			records = append(records, rec)
		}
	}
	return records, errors.Join(readErrs...)
}

// ClearHealth clears only the named component. It is intentionally idempotent.
func (s *Store) ClearHealth(component HealthComponent) error {
	name, err := healthFile(component)
	if err != nil {
		return err
	}
	err = removeStateFile(s, name)
	return err
}

func removeStateFile(s *Store, name string) error {
	// Keep the path fixed by healthFile's allowlist. Removing a missing health
	// record is a successful no-op so a first healthy operation is cheap.
	err := os.Remove(filepath.Join(s.dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("auth: clear %s: %w", name, err)
	}
	return nil
}

// HealthComponents is exposed for renderers and tests that need the complete
// fixed domain without duplicating it.
func HealthComponents() []HealthComponent {
	return []HealthComponent{HealthDecision, HealthCapture, HealthFlush}
}
