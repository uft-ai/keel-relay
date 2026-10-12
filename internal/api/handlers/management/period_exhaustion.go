package management

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const periodExhaustionBodyLimit = 64 * 1024

type periodObservationRequest struct {
	ObservationID      string      `json:"observation_id"`
	BasedOnVersion     *uint64     `json:"based_on_version"`
	State              string      `json:"state"`
	AuthIndex          string      `json:"auth_index"`
	Model              string      `json:"model"`
	PeriodEnd          *strictTime `json:"period_end"`
	AccountPeriodStart *strictTime `json:"account_period_start"`
	Remaining          *int64      `json:"remaining"`
	NotBefore          strictTime  `json:"not_before"`
	PoolID             string      `json:"pool_id"`
}

func (h *Handler) GetPeriodExhaustion(c *gin.Context) {
	ledger := h.periodLedger()
	if ledger == nil {
		periodError(c, http.StatusServiceUnavailable, "ledger_unavailable")
		return
	}
	authIndex := strings.TrimSpace(c.Query("auth_index"))
	model := strings.TrimSpace(c.Query("model"))
	if !h.periodSubjectAllowed(authIndex, model) {
		periodError(c, http.StatusBadRequest, "subject_not_allowed")
		return
	}
	record, observedAt, err := ledger.Get(authIndex, model)
	if err != nil {
		periodLedgerError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"record": publicPeriodRecord(record), "observed_at": observedAt.UTC()})
}

func (h *Handler) PutPeriodExhaustion(c *gin.Context) {
	ledger := h.periodLedger()
	if ledger == nil {
		periodError(c, http.StatusServiceUnavailable, "ledger_unavailable")
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, periodExhaustionBodyLimit+1))
	if err != nil || len(body) > periodExhaustionBodyLimit {
		periodError(c, http.StatusRequestEntityTooLarge, "body_too_large")
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var request periodObservationRequest
	if err := decoder.Decode(&request); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		periodError(c, http.StatusBadRequest, "invalid_body")
		return
	}
	if request.BasedOnVersion == nil || strings.TrimSpace(request.ObservationID) == "" ||
		(request.State != "exhausted" && request.State != "available") || request.NotBefore.IsZero() ||
		request.NotBefore.After(time.Now().UTC()) ||
		(request.Remaining != nil && *request.Remaining < 0) ||
		(request.State == "available" && (request.Remaining == nil || *request.Remaining <= 0)) {
		periodError(c, http.StatusBadRequest, "invalid_body")
		return
	}
	if !h.periodSubjectAllowed(request.AuthIndex, request.Model) {
		periodError(c, http.StatusBadRequest, "subject_not_allowed")
		return
	}
	record, err := ledger.ApplyObservation(c.Request.Context(), coreauth.PeriodObservation{
		ObservationID: request.ObservationID, BasedOnVersion: *request.BasedOnVersion, State: request.State,
		AuthIndex: request.AuthIndex, Model: request.Model, PeriodEnd: optionalStrictTime(request.PeriodEnd),
		AccountPeriodStart: optionalStrictTime(request.AccountPeriodStart), Remaining: request.Remaining,
		NotBefore: request.NotBefore.Time, PoolID: request.PoolID,
	})
	if err != nil {
		periodLedgerError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"record": publicPeriodRecord(record)})
}

func (h *Handler) periodLedger() *coreauth.PeriodLedger {
	if h == nil || h.authManager == nil {
		return nil
	}
	return h.authManager.PeriodLedger()
}

func (h *Handler) periodSubjectAllowed(authIndex, model string) bool {
	authIndex = strings.ToLower(strings.TrimSpace(authIndex))
	model = strings.TrimSpace(model)
	if len(authIndex) != 16 || model == "" {
		return false
	}
	for _, provider := range h.openAICompatibilityWithAuthIndex() {
		if !nanPeriodProvider(provider) {
			continue
		}
		for _, key := range provider.APIKeyEntries {
			if !strings.EqualFold(strings.TrimSpace(key.AuthIndex), authIndex) {
				continue
			}
			for _, configured := range provider.Models {
				if strings.TrimSpace(configured.Name) == model {
					return true
				}
			}
		}
	}
	return false
}

func nanPeriodProvider(provider openAICompatibilityWithAuthIndex) bool {
	if strings.EqualFold(strings.TrimSpace(provider.Name), "nan") {
		return true
	}
	parsed, err := url.Parse(provider.BaseURL)
	return err == nil && strings.EqualFold(parsed.Hostname(), "api.nan.builders")
}

func publicPeriodRecord(record coreauth.PeriodRecord) gin.H {
	return gin.H{
		"auth_index": record.AuthIndex, "model": record.Model, "blocked": record.Blocked,
		"version": record.Version, "period_end": record.PeriodEnd, "exhausted_at": record.ExhaustedAt,
		"account_period_start": record.AccountPeriodStart, "updated_at": record.UpdatedAt,
		"reset_unknown": record.PeriodEnd == nil,
	}
}

func periodLedgerError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, coreauth.ErrPeriodPoolUnsupported):
		periodError(c, http.StatusBadRequest, "pool_unsupported")
	case errors.Is(err, coreauth.ErrPeriodObservationInvalid), errors.Is(err, coreauth.ErrPeriodInvalidInput):
		periodError(c, http.StatusBadRequest, "observation_invalid")
	case errors.Is(err, coreauth.ErrPeriodStaleObservation):
		periodError(c, http.StatusConflict, "stale_observation")
	case errors.Is(err, coreauth.ErrPeriodNotRenewed):
		periodError(c, http.StatusConflict, "not_renewed")
	case errors.Is(err, coreauth.ErrPeriodObservationConflict):
		periodError(c, http.StatusConflict, "observation_conflict")
	default:
		periodError(c, http.StatusServiceUnavailable, "ledger_unavailable")
	}
}

func periodError(c *gin.Context, status int, code string) {
	c.JSON(status, gin.H{"error": code})
}

type strictTime struct{ time.Time }

func (value *strictTime) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return err
	}
	value.Time = parsed
	return nil
}

func optionalStrictTime(value *strictTime) *time.Time {
	if value == nil || value.IsZero() {
		return nil
	}
	result := value.Time
	return &result
}
