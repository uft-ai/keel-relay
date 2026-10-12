package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// PostgresPeriodStore persists period records without deleting cleared versions.
// It is intentionally separate from PostgresStore. The integrator must construct
// it with an open DB, schema-qualified table name, and call EnsurePeriodSchema
// during store initialization before NewPeriodLedger.
type PostgresPeriodStore struct {
	db    *sql.DB
	table string
}

func NewPostgresPeriodStore(db *sql.DB, table string) *PostgresPeriodStore {
	return &PostgresPeriodStore{db: db, table: table}
}

func (s *PostgresPeriodStore) EnsurePeriodSchema(ctx context.Context) error {
	if s == nil || s.db == nil || s.table == "" {
		return fmt.Errorf("postgres period store: not initialized")
	}
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			auth_index TEXT NOT NULL,
			model TEXT NOT NULL,
			content JSONB NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (auth_index, model)
		)
	`, s.table))
	if err != nil {
		return fmt.Errorf("postgres period store: create table: %w", err)
	}
	return nil
}

func (s *PostgresPeriodStore) Load(ctx context.Context) ([]cliproxyauth.PeriodRecord, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf("SELECT auth_index, model, content FROM %s", s.table))
	if err != nil {
		return nil, fmt.Errorf("postgres period store: load: %w", err)
	}
	defer rows.Close()
	records := []cliproxyauth.PeriodRecord{}
	for rows.Next() {
		var authIndex, model string
		var content []byte
		if err := rows.Scan(&authIndex, &model, &content); err != nil {
			return nil, err
		}
		var record cliproxyauth.PeriodRecord
		if err := json.Unmarshal(content, &record); err != nil {
			return nil, fmt.Errorf("postgres period store: corrupt record: %w", err)
		}
		if record.AuthIndex != authIndex || record.Model != model || record.Version == 0 {
			return nil, fmt.Errorf("postgres period store: subject mismatch or corrupt record")
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *PostgresPeriodStore) Save(ctx context.Context, record cliproxyauth.PeriodRecord) error {
	content, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %s (auth_index, model, content, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (auth_index, model) DO UPDATE SET
			content = EXCLUDED.content,
			updated_at = NOW()
	`, s.table), record.AuthIndex, record.Model, content)
	return err
}
