package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type periodTestExecutor struct {
	selectedCapabilityCaptureExecutor
	failure *Error
	late    bool
}

func (e *periodTestExecutor) Execute(_ context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.requests = append(e.requests, req)
	if e.failure != nil {
		return cliproxyexecutor.Response{}, e.failure
	}
	return cliproxyexecutor.Response{Payload: []byte(`{}`)}, nil
}

func (e *periodTestExecutor) ExecuteStream(_ context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.requests = append(e.requests, req)
	if !e.late && e.failure != nil {
		return nil, e.failure
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 2)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`{"object":"chat.completion.chunk"}`)}
	if e.failure != nil {
		chunks <- cliproxyexecutor.StreamChunk{Err: e.failure}
	}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func periodManagerFixture(t *testing.T, dir string) (*Manager, *periodTestExecutor, *Auth) {
	t.Helper()
	cfg := &internalconfig.Config{DisableCooling: true, RequestRetry: 3, OpenAICompatibility: []internalconfig.OpenAICompatibility{{
		Name: "nan", Prefix: "nan", BaseURL: "https://api.nan.builders/v1",
		APIKeyEntries: []internalconfig.OpenAICompatibilityAPIKey{{APIKey: "synthetic-period-key"}},
		Models: []internalconfig.OpenAICompatibilityModel{
			{Name: "qwen3.6", Alias: "qwen", Operations: []string{"chat", "responses"}},
			{Name: "qwen3.6", Alias: "qwen-native", Operations: []string{"responses"}, Native: []string{"responses"}},
			{Name: "other", Alias: "other", Operations: []string{"chat"}},
		},
	}}}
	m := NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	m.SetRetryConfig(3, time.Second, 3)
	store, err := NewFilePeriodStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := NewPeriodLedger(t.Context(), store, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.SetPeriodLedger(ledger)
	auth := &Auth{ID: t.Name(), Provider: "openai-compatible-nan", Prefix: "nan", Attributes: map[string]string{
		AttributeAuthKind: AuthKindAPIKey, AttributeAPIKey: "synthetic-period-key", AttributeSource: "config:nan",
		"base_url": "https://api.nan.builders/v1", "compat_name": "nan", "provider_key": "nan",
	}}
	executor := &periodTestExecutor{selectedCapabilityCaptureExecutor: selectedCapabilityCaptureExecutor{provider: auth.Provider}}
	m.RegisterExecutor(executor)
	registerCapabilityTestAuth(t, m, auth)
	stored, _ := m.GetByID(auth.ID)
	var models []*registry.ModelInfo
	for _, name := range []string{"qwen", "qwen-native", "other"} {
		models = append(models, &registry.ModelInfo{ID: name}, &registry.ModelInfo{ID: "nan/" + name})
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, models)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	m.RefreshSchedulerEntry(auth.ID)
	return m, executor, stored
}

func TestPeriodManagerBlocksEveryAliasAcrossRestartWithoutCooldown(t *testing.T) {
	dir := t.TempDir()
	m, executor, auth := periodManagerFixture(t, dir)
	executor.failure = &Error{HTTPStatus: 402, Message: `{"error":{"code":"monthly_cap_reached"}}`}
	_, err := m.Execute(t.Context(), []string{auth.Provider}, cliproxyexecutor.Request{Model: "nan/qwen"}, cliproxyexecutor.Options{})
	if err == nil || len(executor.requests) != 1 || !m.PeriodLedger().IsBlocked(auth.Index, "qwen3.6") {
		t.Fatalf("monthly cap not enforced: calls=%d error=%v", len(executor.requests), err)
	}
	stored, _ := m.GetByID(auth.ID)
	for _, state := range stored.ModelStates {
		if !state.NextRetryAfter.IsZero() {
			t.Fatal("monthly cap entered generic retry cooldown")
		}
	}
	// Disabling cooling, clearing generic quota, or restarting cannot release the ledger.
	m, executor, auth = periodManagerFixture(t, dir)
	for _, stream := range []bool{false, true} {
		req := cliproxyexecutor.Request{Model: "nan/qwen-native"}
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
		if stream {
			_, err = m.ExecuteStream(t.Context(), []string{auth.Provider}, req, opts)
		} else {
			_, err = m.Execute(t.Context(), []string{auth.Provider}, req, opts)
		}
		var blocked *periodExhaustedError
		if !errors.As(err, &blocked) || len(executor.requests) != 0 || blocked.StatusCode() != http.StatusPaymentRequired {
			t.Fatalf("blocked alias attempted: calls=%d error=%v", len(executor.requests), err)
		}
	}
	_, err = m.Execute(t.Context(), []string{auth.Provider}, cliproxyexecutor.Request{Model: "nan/other"}, cliproxyexecutor.Options{})
	if err != nil || len(executor.requests) != 1 || executor.requests[0].Model != "other" {
		t.Fatalf("unrelated model blocked: %v", err)
	}
}

type periodCancelingExecutor struct {
	periodTestExecutor
	cancel context.CancelFunc
}

func (e *periodCancelingExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.cancel()
	return e.periodTestExecutor.ExecuteStream(ctx, auth, req, opts)
}

func TestPeriodManagerRecordsStreamCapWhenClientCancels(t *testing.T) {
	m, base, auth := periodManagerFixture(t, t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	executor := &periodCancelingExecutor{periodTestExecutor: *base, cancel: cancel}
	executor.failure = &Error{HTTPStatus: 402, Code: "monthly_cap_reached"}
	m.RegisterExecutor(executor)
	if _, err := m.ExecuteStream(ctx, []string{auth.Provider}, cliproxyexecutor.Request{Model: "nan/qwen"}, cliproxyexecutor.Options{}); err == nil {
		t.Fatal("canceled stream succeeded")
	}
	if !m.PeriodLedger().IsBlocked(auth.Index, "qwen3.6") || len(executor.requests) != 1 {
		t.Fatalf("canceled monthly cap was lost: calls=%d", len(executor.requests))
	}
}

func TestPeriodManagerRecordsLateStreamCap(t *testing.T) {
	m, executor, auth := periodManagerFixture(t, t.TempDir())
	executor.failure = &Error{HTTPStatus: 402, Code: "monthly_cap_reached"}
	executor.late = true
	stream, err := m.ExecuteStream(t.Context(), []string{auth.Provider}, cliproxyexecutor.Request{Model: "nan/qwen"}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for range stream.Chunks {
	}
	if !m.PeriodLedger().IsBlocked(auth.Index, "qwen3.6") || len(executor.requests) != 1 {
		t.Fatal("late SSE cap was lost or retried")
	}
}

func TestPeriodManagerDoesNotGuessMonthlyExhaustion(t *testing.T) {
	for _, failure := range []*Error{{HTTPStatus: 429, Code: "monthly_cap_reached"}, {HTTPStatus: 402, Code: "payment_required"}} {
		t.Run(failure.Code+http.StatusText(failure.HTTPStatus), func(t *testing.T) {
			m, executor, auth := periodManagerFixture(t, t.TempDir())
			executor.failure = failure
			m.SetRetryConfig(0, 0, 1)
			_, _ = m.Execute(t.Context(), []string{auth.Provider}, cliproxyexecutor.Request{Model: "nan/qwen"}, cliproxyexecutor.Options{})
			if m.PeriodLedger().IsBlocked(auth.Index, "qwen3.6") {
				t.Fatal("unconfirmed monthly exhaustion created a durable block")
			}
		})
	}
}
