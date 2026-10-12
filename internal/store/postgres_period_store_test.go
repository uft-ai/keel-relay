package store

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestPostgresPeriodStoreRetainsClearedVersion(t *testing.T) {
	dsn := os.Getenv("CPA_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CPA_TEST_POSTGRES_DSN is not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewPostgresPeriodStore(db, "period_ledger_test")
	ctx := context.Background()
	if err := store.EnsurePeriodSchema(ctx); err != nil {
		t.Fatal(err)
	}
	defer db.ExecContext(ctx, "DROP TABLE period_ledger_test")
	end := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	record := cliproxyauth.PeriodRecord{AuthIndex: "0123456789abcdef", Model: "Qwen3.6", Blocked: true, Version: 2, PeriodEnd: &end}
	if err := store.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	record.Blocked = false
	if err := store.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(ctx)
	if err != nil || len(loaded) != 1 || loaded[0].Blocked || loaded[0].Version != 2 {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE period_ledger_test SET model = 'other' WHERE model = 'Qwen3.6'"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx); err == nil {
		t.Fatal("accepted mismatched subject")
	}
}
