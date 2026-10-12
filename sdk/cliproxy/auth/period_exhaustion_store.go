package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FilePeriodStore stores one period record per hashed subject file.
// It never writes cooldown .cds files and never deletes a subject implicitly.
type FilePeriodStore struct {
	dir string
}

type periodRecordFile struct {
	AuthIndex          string     `json:"auth_index"`
	Model              string     `json:"model"`
	Blocked            bool       `json:"blocked"`
	Version            uint64     `json:"version"`
	PeriodEnd          *time.Time `json:"period_end,omitempty"`
	ExhaustedAt        time.Time  `json:"exhausted_at"`
	ObservationID      string     `json:"observation_id,omitempty"`
	ObservationHash    string     `json:"observation_hash,omitempty"`
	AccountPeriodStart *time.Time `json:"account_period_start,omitempty"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

// NewFilePeriodStore creates a store rooted outside the cooldown directory.
func NewFilePeriodStore(dir string) (*FilePeriodStore, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" || !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("%w: absolute period directory", ErrPeriodInvalidInput)
	}
	return &FilePeriodStore{dir: filepath.Clean(dir)}, nil
}

// Load reads every record. A corrupt, escaped, or cancelled file fails startup.
func (s *FilePeriodStore) Load(ctx context.Context) ([]PeriodRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read period ledger: %w", err)
	}
	records := make([]PeriodRecord, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(s.dir, entry.Name())
		record, errRead := readPeriodFile(path)
		if errRead != nil {
			return nil, errRead
		}
		expected, errName := PeriodSubjectFilename(record.AuthIndex, record.Model)
		if errName != nil || expected != entry.Name() {
			return nil, fmt.Errorf("%w: subject filename mismatch", ErrPeriodInvalidInput)
		}
		records = append(records, record)
	}
	return records, nil
}

// Save atomically writes one subject without removing any other subject.
func (s *FilePeriodStore) Save(ctx context.Context, record PeriodRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	name, err := PeriodSubjectFilename(record.AuthIndex, record.Model)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create period ledger: %w", err)
	}
	target := filepath.Join(s.dir, name)
	if !withinDir(s.dir, target) {
		return fmt.Errorf("%w: escaped path", ErrPeriodInvalidInput)
	}
	payload := periodRecordFile{
		AuthIndex: record.AuthIndex, Model: record.Model, Blocked: record.Blocked,
		Version: record.Version, PeriodEnd: cloneTime(record.PeriodEnd), ExhaustedAt: record.ExhaustedAt.UTC(),
		ObservationID: record.ObservationID, ObservationHash: record.ObservationHash,
		AccountPeriodStart: cloneTime(record.AccountPeriodStart), UpdatedAt: record.UpdatedAt.UTC(),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	temporary := target + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := syncDirectory(s.dir); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, target); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return syncDirectory(s.dir)
}

func syncDirectory(dir string) error {
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func readPeriodFile(path string) (PeriodRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return PeriodRecord{}, fmt.Errorf("read period record: %w", err)
	}
	var payload periodRecordFile
	if err := json.Unmarshal(data, &payload); err != nil || payload.Version == 0 || payload.AuthIndex == "" || payload.Model == "" {
		return PeriodRecord{}, fmt.Errorf("%w: corrupt period record", ErrPeriodInvalidInput)
	}
	return PeriodRecord{
		AuthIndex: payload.AuthIndex, Model: payload.Model, Blocked: payload.Blocked,
		Version: payload.Version, PeriodEnd: cloneTime(payload.PeriodEnd), ExhaustedAt: payload.ExhaustedAt.UTC(),
		ObservationID: payload.ObservationID, ObservationHash: payload.ObservationHash,
		AccountPeriodStart: cloneTime(payload.AccountPeriodStart), UpdatedAt: payload.UpdatedAt.UTC(),
	}, nil
}

func withinDir(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// Ensure fs import remains available for tests that inspect file modes.
var _ fs.FileInfo
