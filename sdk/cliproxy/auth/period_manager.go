package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// SetPeriodLedger installs the independently persisted billing-period state.
func (m *Manager) SetPeriodLedger(ledger *PeriodLedger) {
	m.periodLedger.Store(ledger)
}

func (m *Manager) PeriodLedger() *PeriodLedger {
	if m == nil {
		return nil
	}
	return m.periodLedger.Load()
}

func isNanPeriodAuth(auth *Auth) bool {
	if auth == nil {
		return false
	}
	u, err := url.Parse(auth.Attributes["base_url"])
	return (err == nil && strings.EqualFold(u.Hostname(), "api.nan.builders")) ||
		strings.EqualFold(strings.TrimSpace(auth.Attributes["compat_name"]), "nan")
}

func isMonthlyCapResult(err *Error) bool {
	return err != nil && err.HTTPStatus == http.StatusPaymentRequired &&
		(err.Code == "monthly_cap_reached" || gjson.Get(err.Message, "error.code").String() == "monthly_cap_reached")
}

func (m *Manager) observePeriodResult(ctx context.Context, auth *Auth, result Result) {
	ledger := m.PeriodLedger()
	if ledger == nil || result.Success || !isNanPeriodAuth(auth) || !isMonthlyCapResult(result.Error) || result.UpstreamModel == "" {
		return
	}
	model := strings.TrimSpace(thinking.ParseSuffix(result.UpstreamModel).ModelName)
	if _, err := ledger.RecordProviderExhaustion(context.WithoutCancel(ctx), auth.Index, model, time.Time{}); err != nil {
		log.WithError(err).Error("failed to persist monthly quota exhaustion; credential/model remains blocked in memory")
	}
}

func (m *Manager) periodBlock(auth *Auth, model string) *periodExhaustedError {
	return periodBlock(m.PeriodLedger(), auth, model)
}

func periodBlock(ledger *PeriodLedger, auth *Auth, model string) *periodExhaustedError {
	if ledger == nil || !isNanPeriodAuth(auth) {
		return nil
	}
	model = strings.TrimSpace(thinking.ParseSuffix(model).ModelName)
	if !ledger.IsBlocked(auth.Index, model) {
		return nil
	}
	record, _, err := ledger.Get(auth.Index, model)
	if err != nil || !record.Blocked {
		return nil
	}
	return &periodExhaustedError{resetAt: record.PeriodEnd}
}

type periodExhaustedError struct{ resetAt *time.Time }

func (e *periodExhaustedError) ResponseBody() []byte {
	body, _ := json.Marshal(map[string]any{"error": map[string]any{
		"code": "quota_exhausted", "type": "insufficient_quota",
		"message":  "Monthly allowance exhausted; waiting for verified billing-period renewal.",
		"reset_at": e.resetAt, "reset_unknown": e.resetAt == nil,
	}})
	return body
}
func (e *periodExhaustedError) Error() string         { return string(e.ResponseBody()) }
func (e *periodExhaustedError) StatusCode() int       { return http.StatusPaymentRequired }
func (e *periodExhaustedError) IsRequestScoped() bool { return true }
func (e *periodExhaustedError) DirectResponse() bool  { return true }
