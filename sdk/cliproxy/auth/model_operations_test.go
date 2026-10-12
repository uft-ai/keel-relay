package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

type operationRetryExecutor struct {
	selectedCapabilityCaptureExecutor
	fail bool
}

func (e *operationRetryExecutor) Execute(_ context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.requests = append(e.requests, req)
	if e.fail {
		return cliproxyexecutor.Response{}, &Error{Code: "upstream_error", Message: "fixture failure", HTTPStatus: 502}
	}
	return cliproxyexecutor.Response{Payload: []byte(`{}`)}, nil
}

func operationPool(t *testing.T, first, second internalconfig.OpenAICompatibilityModel) (*Manager, *operationRetryExecutor, *operationRetryExecutor) {
	t.Helper()
	m := NewManager(nil, nil, nil)
	models := []internalconfig.OpenAICompatibilityModel{first, second}
	providers := []string{"high", "low"}
	var configs []internalconfig.OpenAICompatibility
	for index, provider := range providers {
		configs = append(configs, internalconfig.OpenAICompatibility{Name: provider, Prefix: "nan", BaseURL: "https://example.invalid/v1",
			APIKeyEntries: []internalconfig.OpenAICompatibilityAPIKey{{APIKey: provider}}, Models: []internalconfig.OpenAICompatibilityModel{models[index]}})
	}
	// SDK callers can replace routing snapshots directly, independently of config ingress validation.
	m.SetConfig(&internalconfig.Config{OpenAICompatibility: configs})
	var executors []*operationRetryExecutor
	for index, provider := range providers {
		auth := &Auth{ID: provider, Provider: "openai-compatible-" + provider, Prefix: "nan", Attributes: map[string]string{
			AttributeAuthKind: AuthKindAPIKey, AttributeAPIKey: provider, AttributeSource: "config:" + provider,
			"compat_name": provider, "provider_key": provider, "priority": "0",
		}}
		if index == 0 {
			auth.Attributes["priority"] = "100"
		}
		executor := &operationRetryExecutor{selectedCapabilityCaptureExecutor: selectedCapabilityCaptureExecutor{provider: auth.Provider}}
		m.RegisterExecutor(executor)
		registerCapabilityTestAuth(t, m, auth)
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "shared"}, {ID: "nan/shared"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
		m.RefreshSchedulerEntry(auth.ID)
		executors = append(executors, executor)
	}
	return m, executors[0], executors[1]
}

func TestOperationSelectionSkipsIneligibleHigherPriorityCredential(t *testing.T) {
	m, denied, allowed := operationPool(t,
		internalconfig.OpenAICompatibilityModel{Name: "first", Alias: "shared", Operations: []string{"rerank"}},
		internalconfig.OpenAICompatibilityModel{Name: "second", Alias: "shared", Operations: []string{"chat"}})
	_, err := m.Execute(t.Context(), []string{denied.provider, allowed.provider}, cliproxyexecutor.Request{Model: "nan/shared"}, cliproxyexecutor.Options{})
	if err != nil || len(denied.requests) != 0 || len(allowed.requests) != 1 {
		t.Fatalf("eligibility: denied=%d allowed=%d err=%v", len(denied.requests), len(allowed.requests), err)
	}
	stored, _ := m.GetByID("high")
	if stored.Unavailable || stored.LastError != nil || len(stored.ModelStates) != 0 {
		t.Fatalf("ineligible credential mutated: %+v", stored)
	}
}

func TestCompletionsConversionFollowsSelectedModelAndInterceptor(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy_chat", true: "native"}[native], func(t *testing.T) {
			model := internalconfig.OpenAICompatibilityModel{Name: "qwen", Alias: "shared", Operations: []string{"completions"}}
			if native {
				model.Native = []string{"completions"}
			}
			m, selected, _ := operationPool(t, model, model)
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Metadata: map[string]any{cliproxyexecutor.RequestPathMetadataKey: "/v1/completions"}}
			opts.RequestAfterAuthInterceptor = func(_ context.Context, in cliproxyexecutor.RequestAfterAuthInterceptRequest) cliproxyexecutor.RequestAfterAuthInterceptResponse {
				if gjson.GetBytes(in.Body, "prompt").String() != "original" || gjson.GetBytes(in.Body, "messages").Exists() {
					t.Fatalf("raw completions lost before interceptor: %s", in.Body)
				}
				return cliproxyexecutor.RequestAfterAuthInterceptResponse{Body: []byte(`{"model":"nan/shared","prompt":"intercepted","suffix":"tail","logprobs":5}`)}
			}
			_, err := m.Execute(t.Context(), []string{selected.provider}, cliproxyexecutor.Request{Model: "nan/shared", Payload: []byte(`{"model":"nan/shared","prompt":"original"}`)}, opts)
			if err != nil || len(selected.requests) != 1 {
				t.Fatalf("attempts=%d error=%v", len(selected.requests), err)
			}
			body := selected.requests[0].Payload
			if native {
				if gjson.GetBytes(body, "prompt").String() != "intercepted" || gjson.GetBytes(body, "suffix").String() != "tail" || gjson.GetBytes(body, "logprobs").Int() != 5 {
					t.Fatalf("native fields lost: %s", body)
				}
			} else if gjson.GetBytes(body, "messages.0.content").String() != "intercepted" || gjson.GetBytes(body, "prompt").Exists() {
				t.Fatalf("legacy chat changed: %s", body)
			}
		})
	}
}

func TestOperationDispatchFreezesNativeModeAcrossCredentialFailover(t *testing.T) {
	for _, matching := range []bool{false, true} {
		t.Run(map[bool]string{false: "different_mode_is_excluded", true: "same_mode_can_fail_over"}[matching], func(t *testing.T) {
			first := internalconfig.OpenAICompatibilityModel{Name: "first", Alias: "shared", Operations: []string{"responses"}, Native: []string{"responses"}}
			second := internalconfig.OpenAICompatibilityModel{Name: "second", Alias: "shared", Operations: []string{"responses"}}
			if matching {
				second.Native = []string{"responses"}
			}
			m, primary, fallback := operationPool(t, first, second)
			primary.fail = true
			_, err := m.Execute(t.Context(), []string{primary.provider, fallback.provider}, cliproxyexecutor.Request{Model: "nan/shared"}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
			if len(primary.requests) != 1 {
				t.Fatalf("primary attempts=%d", len(primary.requests))
			}
			if matching && (err != nil || len(fallback.requests) != 1) {
				t.Fatalf("same-mode fallback: %d, %v", len(fallback.requests), err)
			}
			if !matching && (err == nil || len(fallback.requests) != 0) {
				t.Fatalf("protocol mode changed: %d, %v", len(fallback.requests), err)
			}
		})
	}
}

func TestRequestedModelOperation(t *testing.T) {
	tests := []struct {
		name string
		opts cliproxyexecutor.Options
		want string
	}{
		{name: "chat default", opts: cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai"), Metadata: map[string]any{cliproxyexecutor.RequestPathMetadataKey: "/v1/chat/completions"}}, want: "chat"},
		{name: "legacy completions", opts: cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai"), Metadata: map[string]any{cliproxyexecutor.RequestPathMetadataKey: "/v1/completions"}}, want: "completions"},
		{name: "responses", opts: cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}, want: "responses"},
		{name: "image generation", opts: cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-image"), Metadata: map[string]any{cliproxyexecutor.RequestPathMetadataKey: "/v1/images/generations"}}, want: "image-generation"},
		{name: "image edit", opts: cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-image"), Metadata: map[string]any{cliproxyexecutor.RequestPathMetadataKey: "/v1/images/edits"}}, want: "image-edit"},
		{name: "explicit override", opts: cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai"), Metadata: map[string]any{cliproxyexecutor.RequestedOperationMetadataKey: " Rerank "}}, want: "rerank"},
		{name: "claude source", opts: cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}, want: "chat"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RequestedModelOperation(tc.opts); got != tc.want {
				t.Fatalf("RequestedModelOperation() = %q, want %q", got, tc.want)
			}
		})
	}
}

func newOperationsFenceManager(t *testing.T, models []internalconfig.OpenAICompatibilityModel, registered []string) (*Manager, *selectedCapabilityCaptureExecutor, *Auth) {
	t.Helper()
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{OpenAICompatibility: []internalconfig.OpenAICompatibility{{
		Name:          "nan",
		Prefix:        "nan",
		BaseURL:       "https://api.nan.builders/v1",
		APIKeyEntries: []internalconfig.OpenAICompatibilityAPIKey{{APIKey: "key-nan"}},
		Models:        models,
	}}})
	executor := &selectedCapabilityCaptureExecutor{provider: "openai-compatible-nan"}
	manager.RegisterExecutor(executor)
	auth := operationsTestAuth()
	registerCapabilityTestAuth(t, manager, auth)
	infos := make([]*registry.ModelInfo, 0, len(registered))
	for _, id := range registered {
		infos = append(infos, &registry.ModelInfo{ID: id})
		if bare := strings.TrimPrefix(id, "nan/"); bare != id {
			infos = append(infos, &registry.ModelInfo{ID: bare})
		}
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, auth.Provider, infos)
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	manager.RefreshSchedulerEntry(auth.ID)
	return manager, executor, auth
}

func assertUnsupportedOperation(t *testing.T, err error, operation string) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil, want unsupported_operation")
	}
	if !IsUnsupportedOperationError(err) {
		t.Fatalf("error = %T %v, want unsupported operation error", err, err)
	}
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest {
		t.Fatalf("error status = %v, want 400", err)
	}
	if !strings.Contains(err.Error(), `"code":"unsupported_operation"`) || !strings.Contains(err.Error(), operation) {
		t.Fatalf("error body = %s, want unsupported_operation for %q", err.Error(), operation)
	}
	if !isRequestInvalidError(err) {
		t.Fatal("unsupported operation must be request-scoped")
	}
}

func TestOperationFenceRejectsUndeclaredOperationWithoutUpstreamOrCooldown(t *testing.T) {
	models := []internalconfig.OpenAICompatibilityModel{
		{Name: "qwen3.6", Alias: "qwen3.6"},
		{Name: "rerank", Alias: "rerank", Operations: []string{"rerank"}},
	}
	manager, executor, auth := newOperationsFenceManager(t, models, []string{"nan/qwen3.6", "nan/rerank"})
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Metadata: map[string]any{
			cliproxyexecutor.RequestPathMetadataKey:        "/v1/chat/completions",
			cliproxyexecutor.AuthSelectionModelMetadataKey: "nan/rerank",
		},
	}
	request := cliproxyexecutor.Request{Model: "nan/rerank"}

	_, errExecute := manager.Execute(t.Context(), []string{auth.Provider}, request, opts)
	assertUnsupportedOperation(t, errExecute, "chat")
	_, errStream := manager.ExecuteStream(t.Context(), []string{auth.Provider}, request, opts)
	assertUnsupportedOperation(t, errStream, "chat")
	_, errCount := manager.ExecuteCount(t.Context(), []string{auth.Provider}, request, opts)
	assertUnsupportedOperation(t, errCount, "chat")

	if len(executor.requests) != 0 {
		t.Fatalf("executor requests = %d, want 0", len(executor.requests))
	}
	current, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("auth missing after fence")
	}
	if current.Unavailable || len(current.ModelStates) != 0 || current.LastError != nil {
		t.Fatalf("fence mutated auth state: unavailable=%t states=%v lastErr=%v", current.Unavailable, current.ModelStates, current.LastError)
	}

	allowed := opts
	allowed.Metadata = map[string]any{
		cliproxyexecutor.RequestedOperationMetadataKey: "rerank",
		cliproxyexecutor.AuthSelectionModelMetadataKey: "nan/rerank",
	}
	if _, errAllowed := manager.Execute(t.Context(), []string{auth.Provider}, request, allowed); errAllowed != nil {
		t.Fatalf("declared operation error = %v", errAllowed)
	}
	if len(executor.requests) != 1 {
		t.Fatalf("executor requests after declared operation = %d, want 1", len(executor.requests))
	}
}

func TestOperationFenceKeepsLegacyEntriesUnrestricted(t *testing.T) {
	models := []internalconfig.OpenAICompatibilityModel{{Name: "qwen3.6", Alias: "qwen3.6"}}
	manager, executor, auth := newOperationsFenceManager(t, models, []string{"nan/qwen3.6"})
	for _, op := range []string{"chat", "completions", "responses", "rerank"} {
		opts := cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("openai"),
			Metadata: map[string]any{
				cliproxyexecutor.RequestedOperationMetadataKey: op,
				cliproxyexecutor.AuthSelectionModelMetadataKey: "nan/qwen3.6",
			},
		}
		if _, err := manager.Execute(t.Context(), []string{auth.Provider}, cliproxyexecutor.Request{Model: "nan/qwen3.6"}, opts); err != nil {
			t.Fatalf("legacy %s error = %v", op, err)
		}
	}
	if len(executor.requests) != 4 {
		t.Fatalf("executor requests = %d, want 4", len(executor.requests))
	}
}

func TestOperationFenceRejectsHomeOptionsWithDispatchMetadata(t *testing.T) {
	req := cliproxyexecutor.Request{Model: "qwen3.6", Metadata: map[string]any{
		resolvedHomeModelOptionsMetadataKey: internalconfig.OpenAICompatibilityModel{Name: "qwen3.6", Native: []string{"responses"}},
	}}
	err := checkModelOperation(req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	assertUnsupportedOperation(t, err, "Home mode")

	legacy := cliproxyexecutor.Request{Model: "qwen3.6", Metadata: map[string]any{
		resolvedHomeModelOptionsMetadataKey: internalconfig.OpenAICompatibilityModel{Name: "qwen3.6"},
	}}
	if errLegacy := checkModelOperation(legacy, cliproxyexecutor.Options{}); errLegacy != nil {
		t.Fatalf("legacy Home options error = %v", errLegacy)
	}
}
