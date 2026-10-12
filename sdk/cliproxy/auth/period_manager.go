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

// IsNanCompatibleAuth recognizes NaN independently of a renamed provider group.
func IsNanCompatibleAuth(auth *Auth) bool {
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
	if ledger == nil || result.Success || !IsNanCompatibleAuth(auth) || !isMonthlyCapResult(result.Error) || result.UpstreamModel == "" {
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
	if ledger == nil || !IsNanCompatibleAuth(auth) {
		return nil
	}
	model = strings.TrimSpace(thinking.ParseSuffix(model).ModelName)
	models := []string{model}
	if model == "flux-2-klein" || model == "qwen-image-2.1" {
		// NaN documents one image allowance for this pair; a different key's membership is unverified.
		models = []string{"flux-2-klein", "qwen-image-2.1"}
	}
	var resetAt *time.Time
	blocked, unknownReset := false, false
	for _, subject := range models {
		record, _, err := ledger.Get(auth.Index, subject)
		if err != nil || !record.Blocked {
			continue
		}
		blocked = true
		if record.PeriodEnd == nil {
			unknownReset = true
		} else if resetAt == nil || record.PeriodEnd.After(*resetAt) {
			resetAt = record.PeriodEnd
		}
	}
	if !blocked {
		return nil
	}
	if unknownReset {
		resetAt = nil
	}
	return &periodExhaustedError{resetAt: resetAt}
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
