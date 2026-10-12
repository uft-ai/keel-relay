package cliproxy

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestPeriodLedgerStartupRestoresIndependentlyOfCooldown(t *testing.T) {
	dir := t.TempDir()
	s := &Service{cfg: &config.Config{SaveCooldownStatus: false}, configPath: filepath.Join(dir, "config.yaml"), coreManager: coreauth.NewManager(nil, nil, nil)}
	if err := s.initializePeriodLedger(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.coreManager.PeriodLedger().RecordProviderExhaustion(t.Context(), "0123456789abcdef", "qwen3.6", time.Time{}); err != nil {
		t.Fatal(err)
	}
	s.coreManager = coreauth.NewManager(nil, nil, nil)
	if err := s.initializePeriodLedger(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !s.coreManager.PeriodLedger().IsBlocked("0123456789abcdef", "qwen3.6") {
		t.Fatal("startup lost billing block while cooling disabled")
	}
	if err := os.WriteFile(filepath.Join(dir, "period-exhaustion", "broken.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	s.coreManager = coreauth.NewManager(nil, nil, nil)
	if err := s.initializePeriodLedger(t.Context()); err == nil {
		t.Fatal("corrupt ledger must prevent startup")
	}
}
