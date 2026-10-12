package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type historyCaptureExecutor struct {
	speechCaptureExecutor
	authID, query string
	called        int
}

func (e *historyCaptureExecutor) Identifier() string { return "openai-compatible-nan" }
func (e *historyCaptureExecutor) ReadUsage(_ context.Context, auth *coreauth.Auth, query string) (coreexecutor.Response, error) {
	e.authID, e.query = auth.ID, query
	e.called++
	return coreexecutor.Response{StatusCode: 429, Payload: []byte(`{"error":{"code":"rate_limited"}}`), Headers: http.Header{"Retry-After": []string{"7"}, "Content-Type": []string{"application/json"}}}, nil
}

func TestUsageHistorySelectsCredentialWithoutMutatingModelAvailability(t *testing.T) {
	gin.SetMode(gin.TestMode)
	m := coreauth.NewManager(nil, nil, nil)
	e := &historyCaptureExecutor{}
	m.RegisterExecutor(e)
	h := NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, m))
	router := gin.New()
	router.Use(logging.GinLogrusLogger(), logging.CPATraceIDMiddleware())
	router.GET("/v1/usage", h.Usage)
	call := func(query string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/usage"+query, nil))
		return recorder
	}
	if got := call(""); got.Code != 404 {
		t.Fatalf("empty provider status=%d", got.Code)
	}
	first, err := m.Register(t.Context(), &coreauth.Auth{ID: "one", Provider: e.Identifier(), Attributes: map[string]string{"api_key": "fixture-one", "base_url": "https://api.nan.builders/v1", "compat_name": "renamed"}, ModelStates: map[string]*coreauth.ModelState{"qwen": {Unavailable: true}}})
	if err != nil {
		t.Fatal(err)
	}
	got := call("?start_date=2026-10-01&end_date=2026-10-02&limit=1")
	if got.Code != 429 || got.Header().Get("Retry-After") != "7" || e.authID != "one" || e.query != "start_date=2026-10-01&end_date=2026-10-02&limit=1" {
		t.Fatalf("history routing failed: %d %+v", got.Code, e)
	}
	if !strings.Contains(got.Header().Get(logging.CPATraceIDHeader), "-"+first.Index+"-") {
		t.Fatal("history response lost credential/request correlation")
	}
	_, err = m.Register(t.Context(), &coreauth.Auth{ID: "two", Provider: e.Identifier(), Attributes: map[string]string{"api_key": "fixture-two", "base_url": "https://api.nan.builders/v1", "compat_name": "nan"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := call(""); got.Code != 400 || !strings.Contains(got.Body.String(), "credential_required") {
		t.Fatalf("ambiguous key status=%d", got.Code)
	}
	if got := call("?auth_index=missing"); got.Code != 404 {
		t.Fatalf("unknown credential status=%d", got.Code)
	}
	got = call("?start_date=2026-10-01&auth_index=" + first.Index + "&limit=1")
	if got.Code != 429 || e.authID != "one" || e.query != "start_date=2026-10-01&limit=1" || e.called != 2 {
		t.Fatalf("selected history query=%s calls=%d", e.query, e.called)
	}
	stored, _ := m.GetByID(first.ID)
	if !stored.ModelStates["qwen"].Unavailable || stored.Failed != 0 || stored.LastError != nil {
		t.Fatal("history updated model availability")
	}
	stored.Disabled = true
	_, err = m.Update(t.Context(), stored)
	if err != nil {
		t.Fatal(err)
	}
	if got := call("?auth_index=" + first.Index); got.Code != 404 {
		t.Fatalf("disabled key used: %d", got.Code)
	}
}
