package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const periodSubjectSeparator = "\x00"

var (
	ErrPeriodInvalidInput        = errors.New("period ledger: invalid input")
	ErrPeriodObservationInvalid  = errors.New("period ledger: observation invalid")
	ErrPeriodStaleObservation    = errors.New("period ledger: stale observation")
	ErrPeriodNotRenewed          = errors.New("period ledger: not renewed")
	ErrPeriodObservationConflict = errors.New("period ledger: observation conflict")
	ErrPeriodPoolUnsupported     = errors.New("period ledger: pool unsupported")
	ErrPeriodSave                = errors.New("period ledger: save failed")
)

// PeriodRecord is one durable credential/model billing-period state.
// A cleared row remains so its version cannot restart at zero.
type PeriodRecord struct {
	AuthIndex          string
	Model              string
	Blocked            bool
	Version            uint64
	PeriodEnd          *time.Time
	ExhaustedAt        time.Time
	ObservationID      string
	ObservationHash    string
	PoolID             string
	AccountPeriodStart *time.Time
	UpdatedAt          time.Time
}

// PeriodObservation is one Deck quota observation.
type PeriodObservation struct {
	ObservationID      string
	BasedOnVersion     uint64
	State              string
	AuthIndex          string
	Model              string
	PeriodEnd          *time.Time
	Remaining          *int64
	NotBefore          time.Time
	PoolID             string
	AccountPeriodStart *time.Time
}

// PeriodStore persists period records independently of cooldown state.
type PeriodStore interface {
	Load(context.Context) ([]PeriodRecord, error)
	Save(context.Context, PeriodRecord) error
}

// PeriodLedger owns period exhaustion decisions and failure-closed persistence.
type PeriodLedger struct {
	store PeriodStore
	now   func() time.Time
	mu    sync.RWMutex
	rows  map[string]PeriodRecord
}

// NewPeriodLedger loads durable state. A corrupt or cancelled load fails.
func NewPeriodLedger(ctx context.Context, store PeriodStore, now func() time.Time) (*PeriodLedger, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: store is required", ErrPeriodInvalidInput)
	}
	if now == nil {
		now = time.Now
	}
	records, err := store.Load(ctx)
	if err != nil {
		return nil, err
	}
	rows := make(map[string]PeriodRecord, len(records))
	for _, record := range records {
		key, errKey := periodSubjectKey(record.AuthIndex, record.Model)
		if errKey != nil {
			return nil, errKey
		}
		if _, exists := rows[key]; exists {
			return nil, fmt.Errorf("%w: duplicate subject", ErrPeriodInvalidInput)
		}
		rows[key] = record
	}
	return &PeriodLedger{store: store, now: now, rows: rows}, nil
}

// List returns cloned retained rows, including cleared rows.
func (l *PeriodLedger) List() []PeriodRecord {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	records := make([]PeriodRecord, 0, len(l.rows))
	for _, record := range l.rows {
		records = append(records, clonePeriodRecord(record))
	}
	return records
}

// Get returns the retained record and the ledger clock used by Deck as not_before.
func (l *PeriodLedger) Get(authIndex, model string) (PeriodRecord, time.Time, error) {
	key, err := periodSubjectKey(authIndex, model)
	if err != nil {
		return PeriodRecord{}, time.Time{}, err
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	record := clonePeriodRecord(l.rows[key])
	if record.Version == 0 {
		record.AuthIndex = canonicalAuthIndex(authIndex)
		record.Model = strings.TrimSpace(model)
	}
	return record, l.now().UTC(), nil
}

// IsBlocked is the per-attempt check. Missing and cleared rows are not blocked.
func (l *PeriodLedger) IsBlocked(authIndex, model string) bool {
	key, err := periodSubjectKey(authIndex, model)
	if err != nil || l == nil {
		return false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	record := l.rows[key]
	return record.Blocked && record.Version > 0
}

// RecordProviderExhaustion records Relay's own monthly exhaustion and bumps version.
// If persistence fails, the volatile block remains and the error is returned.
func (l *PeriodLedger) RecordProviderExhaustion(ctx context.Context, authIndex, model string, exhaustedAt time.Time) (PeriodRecord, error) {
	key, err := periodSubjectKey(authIndex, model)
	if err != nil {
		return PeriodRecord{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	current := l.rows[key]
	current.AuthIndex = canonicalAuthIndex(authIndex)
	current.Model = strings.TrimSpace(model)
	wasBlocked := current.Blocked
	current.Blocked = true
	current.Version++
	if !wasBlocked {
		current.PeriodEnd = nil
	}
	if exhaustedAt.IsZero() {
		exhaustedAt = l.now()
	}
	current.ExhaustedAt = exhaustedAt.UTC()
	current.ObservationID = ""
	current.ObservationHash = ""
	current.UpdatedAt = l.now().UTC()
	if errSave := l.store.Save(ctx, current); errSave != nil {
		l.rows[key] = current
		return current, fmt.Errorf("%w: %w", ErrPeriodSave, errSave)
	}
	l.rows[key] = current
	return current, nil
}

// ApplyObservation applies one quota observation using version CAS.
func (l *PeriodLedger) ApplyObservation(ctx context.Context, observation PeriodObservation) (PeriodRecord, error) {
	if strings.TrimSpace(observation.PoolID) != "" {
		return PeriodRecord{}, ErrPeriodPoolUnsupported
	}
	key, err := periodSubjectKey(observation.AuthIndex, observation.Model)
	if err != nil {
		return PeriodRecord{}, err
	}
	hash, errHash := observationHash(observation)
	if errHash != nil {
		return PeriodRecord{}, errHash
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	current := l.rows[key]
	if current.ObservationID != "" && current.ObservationID == observation.ObservationID {
		if current.ObservationHash == hash {
			return current, nil
		}
		return current, ErrPeriodObservationConflict
	}
	if observation.BasedOnVersion != current.Version {
		return current, ErrPeriodStaleObservation
	}
	next, errDecision := decidePeriodObservation(current, observation, hash)
	if errDecision != nil {
		return current, errDecision
	}
	if samePeriodRecord(current, next) {
		return current, nil
	}
	if errSave := l.store.Save(ctx, next); errSave != nil {
		return current, fmt.Errorf("%w: %w", ErrPeriodSave, errSave)
	}
	l.rows[key] = next
	return next, nil
}

func decidePeriodObservation(current PeriodRecord, observation PeriodObservation, hash string) (PeriodRecord, error) {
	next := current
	next.AuthIndex = canonicalAuthIndex(observation.AuthIndex)
	next.Model = strings.TrimSpace(observation.Model)
	switch observation.State {
	case "exhausted":
		if current.PeriodEnd != nil && observation.PeriodEnd != nil && observation.PeriodEnd.UTC().Before(current.PeriodEnd.UTC()) {
			return current, ErrPeriodObservationInvalid
		}
		if next.Version == 0 || !next.Blocked || laterPeriod(observation.PeriodEnd, next.PeriodEnd) {
			next.Version++
			next.Blocked = true
			next.ExhaustedAt = observation.NotBefore.UTC()
		}
		if observation.PeriodEnd != nil {
			next.PeriodEnd = cloneTime(observation.PeriodEnd)
		}
		next.ObservationID = observation.ObservationID
		next.ObservationHash = hash
		next.AccountPeriodStart = cloneTime(observation.AccountPeriodStart)
		next.UpdatedAt = observation.NotBefore.UTC()
		return next, nil
	case "available":
		if next.Version == 0 {
			return current, nil
		}
		if next.Blocked && next.PeriodEnd == nil && observation.PeriodEnd != nil {
			if !observation.PeriodEnd.UTC().After(next.ExhaustedAt.UTC()) {
				return current, ErrPeriodObservationInvalid
			}
			next.PeriodEnd = cloneTime(observation.PeriodEnd)
			next.ObservationID = observation.ObservationID
			next.ObservationHash = hash
			next.AccountPeriodStart = cloneTime(observation.AccountPeriodStart)
			next.UpdatedAt = observation.NotBefore.UTC()
			return next, nil
		}
		if !canRenew(next, observation) {
			return current, ErrPeriodNotRenewed
		}
		next.Blocked = false
		next.PeriodEnd = cloneTime(observation.PeriodEnd)
		next.ObservationID = observation.ObservationID
		next.ObservationHash = hash
		next.AccountPeriodStart = cloneTime(observation.AccountPeriodStart)
		next.UpdatedAt = observation.NotBefore.UTC()
		return next, nil
	default:
		return current, fmt.Errorf("%w: state", ErrPeriodObservationInvalid)
	}
}

func canRenew(current PeriodRecord, observation PeriodObservation) bool {
	if current.Version == 0 || current.PeriodEnd == nil || observation.PeriodEnd == nil || observation.Remaining == nil || *observation.Remaining <= 0 {
		return false
	}
	end := observation.PeriodEnd.UTC()
	return end.After(current.PeriodEnd.UTC()) && end.After(current.ExhaustedAt.UTC()) && !observation.NotBefore.Before(current.PeriodEnd.UTC())
}

func laterPeriod(next, current *time.Time) bool {
	return next != nil && current != nil && next.UTC().After(current.UTC())
}

func samePeriodRecord(left, right PeriodRecord) bool {
	return left.Blocked == right.Blocked && left.Version == right.Version && sameTime(left.PeriodEnd, right.PeriodEnd) && left.ObservationID == right.ObservationID
}

func sameTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.UTC().Equal(right.UTC())
}

func clonePeriodRecord(record PeriodRecord) PeriodRecord {
	record.PeriodEnd = cloneTime(record.PeriodEnd)
	record.AccountPeriodStart = cloneTime(record.AccountPeriodStart)
	return record
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}

func periodSubjectKey(authIndex, model string) (string, error) {
	index := canonicalAuthIndex(authIndex)
	model = strings.TrimSpace(model)
	if len(index) != 16 || strings.ContainsAny(model, "\x00/") {
		return "", fmt.Errorf("%w: subject", ErrPeriodInvalidInput)
	}
	for _, char := range index {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return "", fmt.Errorf("%w: subject", ErrPeriodInvalidInput)
		}
	}
	if index == "" || model == "" {
		return "", fmt.Errorf("%w: subject", ErrPeriodInvalidInput)
	}
	return index + periodSubjectSeparator + model, nil
}

func canonicalAuthIndex(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func observationHash(observation PeriodObservation) (string, error) {
	if strings.TrimSpace(observation.ObservationID) == "" {
		return "", fmt.Errorf("%w: observation id", ErrPeriodObservationInvalid)
	}
	encoded, err := json.Marshal(observation)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// PeriodSubjectFilename is the collision-free file name for one subject.
func PeriodSubjectFilename(authIndex, model string) (string, error) {
	key, err := periodSubjectKey(authIndex, model)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:]) + ".json", nil
}
