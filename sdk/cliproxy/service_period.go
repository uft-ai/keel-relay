package cliproxy

import (
	"context"
	"path/filepath"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func (s *Service) initializePeriodLedger(ctx context.Context) error {
	if s.coreManager == nil || s.coreManager.PeriodLedger() != nil || (s.cfg != nil && s.cfg.Home.Enabled) {
		return nil
	}
	store := s.periodStore
	if store == nil {
		// Keep ledger JSON outside the recursively scanned authentication directory.
		dir, err := filepath.Abs(filepath.Join(filepath.Dir(s.configPath), "period-exhaustion"))
		if err != nil {
			return err
		}
		store, err = coreauth.NewFilePeriodStore(dir)
		if err != nil {
			return err
		}
	}
	ledger, err := coreauth.NewPeriodLedger(ctx, store, nil)
	if err != nil {
		return err
	}
	s.coreManager.SetPeriodLedger(ledger)
	return nil
}
