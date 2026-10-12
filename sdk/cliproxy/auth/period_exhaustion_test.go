package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type memoryPeriodStore struct {
	records []PeriodRecord
	err     error
}

func (s *memoryPeriodStore) Load(context.Context) ([]PeriodRecord, error) {
	return append([]PeriodRecord(nil), s.records...), s.err
}

func (s *memoryPeriodStore) Save(context.Context, PeriodRecord) error { return s.err }

func fixedNow() func() time.Time {
	now := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	return func() time.Time { return now }
}

func TestPeriodLedgerHealthyObservationKeepsSubjectWithoutCreatingExhaustion(t *testing.T) {
	ledger, err := NewPeriodLedger(t.Context(), &memoryPeriodStore{}, fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	remaining := int64(10)
	record, err := ledger.ApplyObservation(t.Context(), PeriodObservation{ObservationID: "healthy", AuthIndex: "1111111111111111", Model: "qwen3.8-flash", State: "available", Remaining: &remaining, NotBefore: fixedNow()()})
	if err != nil || record.AuthIndex != "1111111111111111" || record.Model != "qwen3.8-flash" || record.Blocked || record.Version != 0 || len(ledger.List()) != 0 {
		t.Fatalf("healthy subject lost or exhaustion invented: %+v, %v", record, err)
	}
}

func TestPeriodLedgerExhaustionPersistsAndSurvivesClear(t *testing.T) {
	store := &memoryPeriodStore{}
	ledger, err := NewPeriodLedger(context.Background(), store, fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	exhaustedAt := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	blocked, err := ledger.RecordProviderExhaustion(context.Background(), "0123456789ABCDEF", "Qwen3.6", exhaustedAt)
	if err != nil || !blocked.Blocked || blocked.Version != 1 || blocked.Model != "Qwen3.6" {
		t.Fatalf("record = %+v err=%v", blocked, err)
	}
	if !ledger.IsBlocked("0123456789abcdef", "Qwen3.6") || ledger.IsBlocked("0123456789abcdef", "qwen3.6") {
		t.Fatal("model case was not preserved")
	}
	anchor := exhaustedAt.Add(time.Hour)
	if _, err := ledger.ApplyObservation(context.Background(), PeriodObservation{
		ObservationID: "anchor", BasedOnVersion: 1, State: "exhausted", AuthIndex: "0123456789abcdef", Model: "Qwen3.6", PeriodEnd: &anchor, NotBefore: fixedNow()(),
	}); err != nil {
		t.Fatal(err)
	}
	end := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	remaining := int64(1)
	cleared, err := ledger.ApplyObservation(context.Background(), PeriodObservation{
		ObservationID: "clear-1", BasedOnVersion: 1, State: "available", AuthIndex: "0123456789abcdef", Model: "Qwen3.6",
		PeriodEnd: &end, Remaining: &remaining, NotBefore: fixedNow()(),
	})
	if err != nil || cleared.Blocked || cleared.Version != 1 {
		t.Fatalf("clear = %+v err=%v", cleared, err)
	}
	again, err := ledger.RecordProviderExhaustion(context.Background(), "0123456789abcdef", "Qwen3.6", end.Add(time.Hour))
	if err != nil || again.Version != 2 || again.PeriodEnd != nil {
		t.Fatalf("new exhaustion reused old period: %+v err=%v", again, err)
	}
}

func TestPeriodLedgerSaveFailuresAreClosed(t *testing.T) {
	store := &memoryPeriodStore{}
	ledger, err := NewPeriodLedger(context.Background(), store, fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	store.err = errors.New("disk")
	record, err := ledger.RecordProviderExhaustion(context.Background(), "0123456789abcdef", "m", time.Time{})
	if !errors.Is(err, ErrPeriodSave) || !record.Blocked || !ledger.IsBlocked("0123456789abcdef", "m") {
		t.Fatalf("402 save failure did not retain block: %+v err=%v", record, err)
	}
	store.err = nil
	anchor := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	observation := exhaustedObservation("anchor", 1, &anchor)
	observation.NotBefore = fixedNow()()
	if _, err := ledger.ApplyObservation(context.Background(), observation); err != nil {
		t.Fatal(err)
	}
	store.err = errors.New("disk")
	end := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	remaining := int64(5)
	current, err := ledger.ApplyObservation(context.Background(), PeriodObservation{
		ObservationID: "clear", BasedOnVersion: 1, State: "available", AuthIndex: "0123456789abcdef", Model: "m",
		PeriodEnd: &end, Remaining: &remaining, NotBefore: anchor.Add(time.Second),
	})
	if !errors.Is(err, ErrPeriodSave) || !current.Blocked {
		t.Fatalf("failed clear changed block: %+v err=%v", current, err)
	}
}

func TestPeriodLedgerRejectsStaleAndRegressingObservations(t *testing.T) {
	store := &memoryPeriodStore{}
	ledger, err := NewPeriodLedger(context.Background(), store, fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if _, err := ledger.RecordProviderExhaustion(context.Background(), "0123456789abcdef", "m", first.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	anchorObservation := exhaustedObservation("anchor", 1, &first)
	anchorObservation.NotBefore = fixedNow()()
	if _, err := ledger.ApplyObservation(context.Background(), anchorObservation); err != nil {
		t.Fatal(err)
	}
	older := first.Add(-time.Hour)
	regressing := exhaustedObservation("old", 1, &older)
	regressing.NotBefore = fixedNow()()
	if _, err := ledger.ApplyObservation(context.Background(), regressing); !errors.Is(err, ErrPeriodObservationInvalid) {
		t.Fatalf("regressing period err=%v", err)
	}
	if _, err := ledger.ApplyObservation(context.Background(), exhaustedObservation("old-version", 0, &first)); !errors.Is(err, ErrPeriodStaleObservation) {
		t.Fatalf("stale version err=%v", err)
	}
}

func exhaustedObservation(id string, version uint64, end *time.Time) PeriodObservation {
	return PeriodObservation{ObservationID: id, BasedOnVersion: version, State: "exhausted", AuthIndex: "0123456789abcdef", Model: "m", PeriodEnd: end}
}

func TestFilePeriodStoreHashesAndRejectsCorruptFiles(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFilePeriodStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	end := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if err := store.Save(context.Background(), PeriodRecord{AuthIndex: "0123456789abcdef", Model: "a_b", Blocked: true, Version: 1, PeriodEnd: &end}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), PeriodRecord{AuthIndex: "0123456789abcdef", Model: "a-b", Blocked: false, Version: 2}); err != nil {
		t.Fatal(err)
	}
	first, _ := PeriodSubjectFilename("0123456789abcdef", "a_b")
	second, _ := PeriodSubjectFilename("0123456789abcdef", "a-b")
	if first == second {
		t.Fatal("different models collided")
	}
	records, err := store.Load(context.Background())
	if err != nil || len(records) != 2 {
		t.Fatalf("records=%d err=%v", len(records), err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background()); err == nil {
		t.Fatal("corrupt ledger loaded")
	}
}

func TestPeriodLedgerAnchorsUnknownAndRejectsConflicts(t *testing.T) {
	store := &memoryPeriodStore{}
	ledger, err := NewPeriodLedger(context.Background(), store, fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	remaining := int64(5)
	end := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	healthy, err := ledger.ApplyObservation(context.Background(), PeriodObservation{
		ObservationID: "healthy", BasedOnVersion: 0, State: "available", AuthIndex: "0123456789abcdef", Model: "m",
		PeriodEnd: &end, Remaining: &remaining,
	})
	if err != nil || healthy.Version != 0 || ledger.IsBlocked("0123456789abcdef", "m") {
		t.Fatalf("healthy = %+v err=%v", healthy, err)
	}
	if _, err := ledger.RecordProviderExhaustion(context.Background(), "0123456789abcdef", "m", end.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	anchored, err := ledger.ApplyObservation(context.Background(), PeriodObservation{
		ObservationID: "first", BasedOnVersion: 1, State: "available", AuthIndex: "0123456789abcdef", Model: "m",
		PeriodEnd: &end, Remaining: &remaining, NotBefore: end.Add(-time.Minute),
	})
	if err != nil || !anchored.Blocked || anchored.Version != 1 || anchored.PeriodEnd == nil {
		t.Fatalf("anchor = %+v err=%v", anchored, err)
	}
	replay, err := ledger.ApplyObservation(context.Background(), PeriodObservation{
		ObservationID: "first", BasedOnVersion: 1, State: "available", AuthIndex: "0123456789abcdef", Model: "m",
		PeriodEnd: &end, Remaining: &remaining, NotBefore: end.Add(-time.Minute),
	})
	if err != nil || replay.Version != anchored.Version {
		t.Fatalf("replay = %+v err=%v", replay, err)
	}
	changed := remaining + 1
	if _, err := ledger.ApplyObservation(context.Background(), PeriodObservation{
		ObservationID: "first", BasedOnVersion: 1, State: "available", AuthIndex: "0123456789abcdef", Model: "m",
		PeriodEnd: &end, Remaining: &changed, NotBefore: end.Add(-time.Minute),
	}); !errors.Is(err, ErrPeriodObservationConflict) {
		t.Fatalf("conflict err=%v", err)
	}
	if _, err := ledger.ApplyObservation(context.Background(), PeriodObservation{
		ObservationID: "pool", BasedOnVersion: 1, State: "available", AuthIndex: "0123456789abcdef", Model: "m", PoolID: "shared",
	}); !errors.Is(err, ErrPeriodPoolUnsupported) {
		t.Fatalf("pool err=%v", err)
	}
	got, _, err := ledger.Get("0123456789abcdef", "m")
	if err != nil || got.PeriodEnd == nil {
		t.Fatal(err)
	}
	*got.PeriodEnd = got.PeriodEnd.Add(time.Hour)
	again, _, _ := ledger.Get("0123456789abcdef", "m")
	if again.PeriodEnd.Equal(*got.PeriodEnd) {
		t.Fatal("returned pointer aliased ledger state")
	}
}

func TestPeriodLedgerClonesReturnedRecordsAndRejectsOlderUpdates(t *testing.T) {
	store := &memoryPeriodStore{}
	ledger, err := NewPeriodLedger(context.Background(), store, fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	record, err := ledger.RecordProviderExhaustion(context.Background(), "0123456789abcdef", "m", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	moved := record.ExhaustedAt.Add(time.Hour)
	record.ExhaustedAt = moved
	stored, _, _ := ledger.Get("0123456789abcdef", "m")
	if stored.ExhaustedAt.Equal(moved) {
		t.Fatal("returned exhaustion record aliased internal state")
	}
	stale := stored.UpdatedAt.Add(-time.Second)
	end := stored.ExhaustedAt.Add(time.Hour)
	_, err = ledger.ApplyObservation(context.Background(), PeriodObservation{
		ObservationID: "older", BasedOnVersion: stored.Version, State: "exhausted", AuthIndex: "0123456789abcdef", Model: "m",
		PeriodEnd: &end, NotBefore: stale,
	})
	if !errors.Is(err, ErrPeriodStaleObservation) {
		t.Fatalf("older observation err=%v", err)
	}
	expired := stored.ExhaustedAt.Add(-time.Second)
	_, err = ledger.ApplyObservation(context.Background(), PeriodObservation{
		ObservationID: "expired-anchor", BasedOnVersion: stored.Version, State: "exhausted", AuthIndex: "0123456789abcdef", Model: "m",
		PeriodEnd: &expired, NotBefore: stored.UpdatedAt,
	})
	if !errors.Is(err, ErrPeriodObservationInvalid) {
		t.Fatalf("expired exhausted anchor err=%v", err)
	}
}

func TestPeriodLedgerRejectsStaleUnknownAnchor(t *testing.T) {
	store := &memoryPeriodStore{}
	ledger, err := NewPeriodLedger(context.Background(), store, fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	exhaustedAt := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	if _, err := ledger.RecordProviderExhaustion(context.Background(), "0123456789abcdef", "m", time.Time{}); err != nil {
		t.Fatal(err)
	}
	stale := exhaustedAt.Add(-time.Second)
	remaining := int64(1)
	_, err = ledger.ApplyObservation(context.Background(), PeriodObservation{
		ObservationID: "stale-anchor", BasedOnVersion: 1, State: "available", AuthIndex: "0123456789abcdef", Model: "m",
		PeriodEnd: &stale, Remaining: &remaining, NotBefore: exhaustedAt,
	})
	if !errors.Is(err, ErrPeriodObservationInvalid) || !ledger.IsBlocked("0123456789abcdef", "m") {
		t.Fatalf("stale anchor err=%v", err)
	}
}

func TestFilePeriodStoreHonorsCancellation(t *testing.T) {
	store, err := NewFilePeriodStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Save(ctx, PeriodRecord{AuthIndex: "0123456789abcdef", Model: "m", Version: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}
