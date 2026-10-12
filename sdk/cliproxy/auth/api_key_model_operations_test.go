package auth

import (
	"slices"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func operationsTestAuth() *Auth {
	return &Auth{
		ID:       "auth-nan",
		Provider: "openai-compatible-nan",
		Prefix:   "nan",
		Attributes: map[string]string{
			AttributeAuthKind: AuthKindAPIKey,
			AttributeAPIKey:   "key-nan",
			AttributeSource:   "config:nan[0]",
			"compat_name":     "nan",
			"provider_key":    "nan",
		},
	}
}

func TestAttachResolvedAPIKeyModelOperationsIsIndependentOfConfigOrder(t *testing.T) {
	legacy := internalconfig.OpenAICompatibilityModel{Name: "qwen3.6", Alias: "qwen3.6"}
	native := internalconfig.OpenAICompatibilityModel{
		Name:       "qwen3.6",
		Alias:      "qwen3.6-native",
		Operations: []string{"chat", "responses", "completions"},
		Native:     []string{"responses", "completions"},
	}
	orders := map[string][]internalconfig.OpenAICompatibilityModel{
		"legacy first": {legacy, native},
		"native first": {native, legacy},
	}
	for name, models := range orders {
		t.Run(name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			manager.SetConfig(&internalconfig.Config{OpenAICompatibility: []internalconfig.OpenAICompatibility{{
				Name:          "nan",
				Prefix:        "nan",
				BaseURL:       "https://api.nan.builders/v1",
				APIKeyEntries: []internalconfig.OpenAICompatibilityAPIKey{{APIKey: "key-nan"}},
				Models:        models,
			}}})
			auth := operationsTestAuth()
			registerCapabilityTestAuth(t, manager, auth)

			for _, tc := range []struct {
				route      string
				operations []string
				native     []string
			}{
				{route: "nan/qwen3.6"},
				{route: "nan/qwen3.6-native", operations: []string{"chat", "responses", "completions"}, native: []string{"responses", "completions"}},
			} {
				upstreams, _, _, routing := manager.executionModelCandidatesWithAlias(auth, tc.route)
				if len(upstreams) != 1 || upstreams[0] != "qwen3.6" {
					t.Fatalf("%s upstream models = %v, want [qwen3.6]", tc.route, upstreams)
				}
				req := attachResolvedAPIKeyModelInfo(routing, cliproxyexecutor.Request{}, auth, tc.route, upstreams[0])
				operations, nativeOps, ok := ResolvedAPIKeyModelOperations(req)
				if !ok {
					t.Fatalf("%s: no resolved model info", tc.route)
				}
				if !slices.Equal(operations, tc.operations) || (operations == nil) != (tc.operations == nil) {
					t.Fatalf("%s operations = %#v, want %#v", tc.route, operations, tc.operations)
				}
				if !slices.Equal(nativeOps, tc.native) || (nativeOps == nil) != (tc.native == nil) {
					t.Fatalf("%s native = %#v, want %#v", tc.route, nativeOps, tc.native)
				}
			}
		})
	}
}

func TestAttachResolvedAPIKeyModelOperationsKeepsLegacyEntryUnrestricted(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{OpenAICompatibility: []internalconfig.OpenAICompatibility{{
		Name:          "nan",
		Prefix:        "nan",
		BaseURL:       "https://api.nan.builders/v1",
		APIKeyEntries: []internalconfig.OpenAICompatibilityAPIKey{{APIKey: "key-nan"}},
		Models: []internalconfig.OpenAICompatibilityModel{
			{Name: "rerank", Alias: "rerank", Operations: []string{"rerank"}},
			{Name: "glm5.3-flash"},
		},
	}}})
	auth := operationsTestAuth()
	registerCapabilityTestAuth(t, manager, auth)

	upstreams, _, _, routing := manager.executionModelCandidatesWithAlias(auth, "nan/glm5.3-flash")
	if len(upstreams) != 1 {
		t.Fatalf("upstream models = %v, want one", upstreams)
	}
	req := attachResolvedAPIKeyModelInfo(routing, cliproxyexecutor.Request{}, auth, "nan/glm5.3-flash", upstreams[0])
	operations, nativeOps, ok := ResolvedAPIKeyModelOperations(req)
	if !ok || operations != nil || nativeOps != nil {
		t.Fatalf("legacy entry = (%#v, %#v, %t), want nil lists", operations, nativeOps, ok)
	}

	upstreams, _, _, routing = manager.executionModelCandidatesWithAlias(auth, "nan/rerank")
	req = attachResolvedAPIKeyModelInfo(routing, cliproxyexecutor.Request{}, auth, "nan/rerank", upstreams[0])
	operations, _, _ = ResolvedAPIKeyModelOperations(req)
	if !slices.Equal(operations, []string{"rerank"}) {
		t.Fatalf("rerank operations = %#v, want [rerank]", operations)
	}
}
