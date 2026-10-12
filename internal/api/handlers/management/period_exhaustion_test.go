package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestPeriodExhaustionRequiresLedgerAndAllowedSubject(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandlerWithoutConfigFilePath(&config.Config{}, coreauth.NewManager(nil, nil, nil))
	response := periodRequest(handler.GetPeriodExhaustion, http.MethodGet, "/availability/period-exhaustion?auth_index=0123456789abcdef&model=qwen3.6", "")
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "ledger_unavailable") {
		t.Fatalf("missing ledger response=%d %s", response.Code, response.Body)
	}
	ledger, err := coreauth.NewPeriodLedger(context.Background(), &memoryPeriodStore{}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	handler.authManager.SetPeriodLedger(ledger)
	handler.cfg.OpenAICompatibility = []config.OpenAICompatibility{{
		Name: "nan", BaseURL: "https://api.nan.builders/v1",
		APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "secret-value"}},
		Models:        []config.OpenAICompatibilityModel{{Name: "Qwen3.6"}},
	}}
	response = periodRequest(handler.PutPeriodExhaustion, http.MethodPut, "/availability/period-exhaustion", `{"unexpected":true}`)
	if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), "secret-value") {
		t.Fatalf("unknown field response=%d %s", response.Code, response.Body)
	}
	response = periodRequest(handler.PutPeriodExhaustion, http.MethodPut, "/availability/period-exhaustion", `{"observation_id":"missing-version","state":"exhausted","auth_index":"0123456789abcdef","model":"Qwen3.6","not_before":"2026-10-12T01:00:00Z"}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_body") {
		t.Fatalf("omitted version response=%d %s", response.Code, response.Body)
	}
	future := time.Now().UTC().Add(time.Second).Format(time.RFC3339)
	response = periodRequest(handler.PutPeriodExhaustion, http.MethodPut, "/availability/period-exhaustion", `{"observation_id":"future","based_on_version":0,"state":"exhausted","auth_index":"0123456789abcdef","model":"Qwen3.6","not_before":"`+future+`"}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_body") {
		t.Fatalf("future not_before response=%d %s", response.Code, response.Body)
	}
	response = periodRequest(handler.PutPeriodExhaustion, http.MethodPut, "/availability/period-exhaustion", `{"observation_id":"negative","based_on_version":0,"state":"exhausted","auth_index":"0123456789abcdef","model":"Qwen3.6","remaining":-1,"not_before":"2026-10-12T01:00:00Z"}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("negative remaining response=%d %s", response.Code, response.Body)
	}
	response = periodRequest(handler.PutPeriodExhaustion, http.MethodPut, "/availability/period-exhaustion", `{"observation_id":"zero","based_on_version":0,"state":"available","auth_index":"0123456789abcdef","model":"Qwen3.6","remaining":0,"not_before":"2026-10-12T01:00:00Z"}{}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("zero available/trailing response=%d %s", response.Code, response.Body)
	}
	response = periodRequest(handler.PutPeriodExhaustion, http.MethodPut, "/availability/period-exhaustion", `{"observation_id":"case","based_on_version":0,"state":"exhausted","auth_index":"0123456789abcdef","model":"qwen3.6","not_before":"2026-10-12T01:00:00Z"}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "subject_not_allowed") {
		t.Fatalf("model case response=%d %s", response.Code, response.Body)
	}
}

type memoryPeriodStore struct{}

func (memoryPeriodStore) Load(context.Context) ([]coreauth.PeriodRecord, error) {
	return nil, nil
}
func (memoryPeriodStore) Save(context.Context, coreauth.PeriodRecord) error { return nil }

func periodRequest(handler gin.HandlerFunc, method, target, body string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = httptest.NewRequest(method, target, strings.NewReader(body))
	handler(context)
	return response
}
