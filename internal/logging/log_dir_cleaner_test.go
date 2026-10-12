package logging

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestEnforceLogDirSizeLimitDeletesOldest(t *testing.T) {
	dir := t.TempDir()

	writeLogFile(t, filepath.Join(dir, "old.log"), 60, time.Unix(1, 0))
	writeLogFile(t, filepath.Join(dir, "mid.log"), 60, time.Unix(2, 0))
	protected := filepath.Join(dir, "main.log")
	writeLogFile(t, protected, 60, time.Unix(3, 0))

	deleted, err := enforceLogDirSizeLimit(dir, 120, protected)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected 1 deleted file, got %d", deleted)
	}

	if _, err := os.Stat(filepath.Join(dir, "old.log")); !os.IsNotExist(err) {
		t.Fatalf("expected old.log to be removed, stat error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mid.log")); err != nil {
		t.Fatalf("expected mid.log to remain, stat error: %v", err)
	}
	if _, err := os.Stat(protected); err != nil {
		t.Fatalf("expected protected main.log to remain, stat error: %v", err)
	}
}

func TestEnforceLogDirSizeLimitSkipsProtected(t *testing.T) {
	dir := t.TempDir()

	protected := filepath.Join(dir, "main.log")
	writeLogFile(t, protected, 200, time.Unix(1, 0))
	writeLogFile(t, filepath.Join(dir, "other.log"), 50, time.Unix(2, 0))

	deleted, err := enforceLogDirSizeLimit(dir, 100, protected)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected 1 deleted file, got %d", deleted)
	}

	if _, err := os.Stat(protected); err != nil {
		t.Fatalf("expected protected main.log to remain, stat error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "other.log")); !os.IsNotExist(err) {
		t.Fatalf("expected other.log to be removed, stat error: %v", err)
	}
}

func writeLogFile(t *testing.T, path string, size int, modTime time.Time) {
	t.Helper()

	data := make([]byte, size)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("set times: %v", err)
	}
}

func TestRequestLogAgeExpiresCompletedCapturesOnly(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	old := now.Add(-25 * time.Hour)
	for _, name := range []string{"v1-chat-completions-2026-10-11T110000-deadbeef.log", "v1-images-2026-10-11T110000_1-client-id.log.gz", "error-v1-chat-2026-10-11T110000-deadbeef.log", "main.log", "notes.log", "response-body-old.tmp", "period-state.json"} {
		writeLogFile(t, filepath.Join(dir, name), 16, old)
	}
	fresh := filepath.Join(dir, "v1-chat-2026-10-12T120000-feedbeef.log")
	writeLogFile(t, fresh, 16, now)
	active, path, err := createUniqueLogFile(dir, "v1-chat-2026-10-11T110000-cafebabe.log")
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	if err = os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	deleted, err := enforceRequestLogMaxAge(dir, 24*time.Hour, filepath.Join(dir, "main.log"), now)
	if err != nil || deleted != 3 {
		t.Fatalf("age deletion count=%d error=%v", deleted, err)
	}
	for _, name := range []string{filepath.Base(path), filepath.Base(fresh), "main.log", "notes.log", "response-body-old.tmp", "period-state.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("protected/unrelated file %s removed: %v", name, err)
		}
	}
	if err = active.Close(); err != nil {
		t.Fatal(err)
	}
	deleted, err = enforceRequestLogMaxAge(dir, 24*time.Hour, "", now)
	if err != nil || deleted != 1 {
		t.Fatalf("closed capture did not become eligible: %d %v", deleted, err)
	}
}

func TestSizeCleanerCannotDeleteActiveFinalization(t *testing.T) {
	dir := t.TempDir()
	file, path, err := createUniqueLogFile(dir, "v1-chat-2026-10-11T110000-cafebabe.log")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteString("active payload larger than the budget"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 30; i++ {
			deleted, err := enforceLogDirSizeLimit(dir, 1, "")
			if err != nil || deleted != 0 {
				t.Errorf("active file removed: %d %v", deleted, err)
				return
			}
		}
	}()
	for i := 0; i < 30; i++ {
		if _, err = file.WriteString("more"); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	if _, err = os.Stat(path); err != nil {
		t.Fatal("active finalization was unlinked")
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	deleted, err := enforceLogDirSizeLimit(dir, 1, "")
	if err != nil || deleted != 1 {
		t.Fatalf("closed capture did not become eligible: %d %v", deleted, err)
	}
}

func TestErrorLogCountCleanerProtectsActiveFinalization(t *testing.T) {
	dir := t.TempDir()
	active, path, err := createUniqueLogFile(dir, "error-v1-chat-2026-10-11T110000-cafebabe.log")
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	old := time.Date(2026, 10, 11, 11, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	writeLogFile(t, filepath.Join(dir, "error-v1-chat-2026-10-12T110000-deadbeef.log"), 16, old.Add(24*time.Hour))
	logger := &FileRequestLogger{logsDir: dir, errorLogsMaxFiles: 1}
	if err := logger.cleanupOldErrorLogs(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("active error capture was removed")
	}
	if err := active.Close(); err != nil {
		t.Fatal(err)
	}
	if err := logger.cleanupOldErrorLogs(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("closed error capture was not removed: %v", err)
	}
}
