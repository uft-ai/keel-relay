package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestGetOpenAICompatIncludesDisableCooling(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	requestRetry := 0
	disableCooling := true
	h := NewHandlerWithoutConfigFilePath(&config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:    "Mimo CN",
				BaseURL: "https://token-plan-cn.xiaomimimo.com/v1",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "test-key"},
				},
				Models: []config.OpenAICompatibilityModel{
					{Name: "mimo-v2.5", Alias: ""},
				},
				SupportPromptCacheKey: true,
				DisableCooling:        &disableCooling,
				RequestRetry:          &requestRetry,
			},
		},
	}, nil)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/openai-compatibility", nil)
	h.GetOpenAICompat(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var body struct {
		OpenAICompatibility []struct {
			SupportPromptCacheKey *bool `json:"support-prompt-cache-key"`
			DisableCooling        *bool `json:"disable-cooling"`
			RequestRetry          *int  `json:"request-retry"`
		} `json:"openai-compatibility"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(body.OpenAICompatibility) != 1 {
		t.Fatalf("expected 1 openai-compatibility entry, got %d", len(body.OpenAICompatibility))
	}
	if body.OpenAICompatibility[0].SupportPromptCacheKey == nil || !*body.OpenAICompatibility[0].SupportPromptCacheKey {
		t.Fatalf("expected support-prompt-cache-key to be present and true, got %#v", body.OpenAICompatibility[0].SupportPromptCacheKey)
	}
	if body.OpenAICompatibility[0].DisableCooling == nil || !*body.OpenAICompatibility[0].DisableCooling {
		t.Fatalf("expected disable-cooling to be present and true, got %#v", body.OpenAICompatibility[0].DisableCooling)
	}
	if body.OpenAICompatibility[0].RequestRetry == nil || *body.OpenAICompatibility[0].RequestRetry != 0 {
		t.Fatalf("expected request-retry to be present and 0, got %#v", body.OpenAICompatibility[0].RequestRetry)
	}
}

func TestOpenAICompatOperationMetadataManagementWrites(t *testing.T) {
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
		Name:    "nan",
		BaseURL: "https://api.nan.builders/v1",
		Models:  []config.OpenAICompatibilityModel{{Name: "qwen3.6", Alias: "qwen3.6"}},
	}}}
	h := &Handler{cfg: cfg, configFilePath: writeTestConfigFile(t)}

	call := func(method, body string, fn func(*gin.Context)) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(method, "/v0/management/openai-compatibility", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		fn(ctx)
		return rec
	}

	rec := call(http.MethodPatch, `{"index":0,"value":{"models":[{"name":"qwen3.6","alias":"qwen3.6"},{"name":"qwen3.6","alias":"qwen3.6-native","operations":["chat","Responses"],"native":["responses"],"max-output-tokens":8192}]}}`, h.PatchOpenAICompat)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body=%s", rec.Code, rec.Body.String())
	}
	native := cfg.OpenAICompatibility[0].Models[1]
	if !native.IsNative("responses") || !native.AllowsOperation("responses") || native.MaxOutputTokens != 8192 {
		t.Fatalf("native entry not stored: %+v", native)
	}

	rec = call(http.MethodGet, "", h.GetOpenAICompat)
	var body struct {
		OpenAICompatibility []struct {
			Models []config.OpenAICompatibilityModel `json:"models"`
		} `json:"openai-compatibility"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	got := body.OpenAICompatibility[0].Models
	if got[0].Operations != nil || got[1].MaxOutputTokens != 8192 || !got[1].IsNative("responses") {
		t.Fatalf("GET did not round-trip metadata: %+v", got)
	}

	for name, payload := range map[string]string{
		"patch empty operations": `{"index":0,"value":{"models":[{"name":"m","operations":[]}]}}`,
		"patch native outside":   `{"index":0,"value":{"models":[{"name":"m","operations":["chat"],"native":["responses"]}]}}`,
	} {
		before := len(cfg.OpenAICompatibility[0].Models)
		if rec = call(http.MethodPatch, payload, h.PatchOpenAICompat); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", name, rec.Code)
		}
		if len(cfg.OpenAICompatibility[0].Models) != before {
			t.Fatalf("%s: rejected patch changed config", name)
		}
	}
	for name, payload := range map[string]string{
		"put conflicting alias": `[{"name":"nan","base-url":"https://api.nan.builders/v1","models":[{"name":"m","alias":"a"},{"name":"n","alias":"a","native":["responses"]}]}]`,
		"put empty operations":  `[{"name":"nan","base-url":"https://api.nan.builders/v1","models":[{"name":"m","operations":[]}]}]`,
		"put empty native":      `[{"name":"nan","base-url":"https://api.nan.builders/v1","models":[{"name":"m","native":[]}]}]`,
		"put cross-provider identity conflict": `[{"name":"nan-a","prefix":"nan","base-url":"https://a.example/v1","models":[{"name":"qwen3.6"}]},` +
			`{"name":"nan-b","prefix":"nan","base-url":"https://b.example/v1","models":[{"name":"qwen3.6","native":["responses"]}]}]`,
	} {
		if rec = call(http.MethodPut, payload, h.PutOpenAICompat); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400, body=%s", name, rec.Code, rec.Body.String())
		}
	}
	if rec = call(http.MethodPatch, `{"index":0,"value":{"models":[{"name":"m","native":[]}]}}`, h.PatchOpenAICompat); rec.Code != http.StatusBadRequest {
		t.Fatalf("patch empty native: status = %d, want 400", rec.Code)
	}

	cfg.OpenAICompatibility = append(cfg.OpenAICompatibility, config.OpenAICompatibility{
		Name: "nan-b", Prefix: "nan", BaseURL: "https://b.example/v1",
		Models: []config.OpenAICompatibilityModel{{Name: "glm", Alias: "glm"}},
	})
	cfg.OpenAICompatibility[0].Prefix = "nan"
	if rec = call(http.MethodPatch, `{"index":1,"value":{"models":[{"name":"qwen3.6","alias":"qwen3.6-native","operations":["chat"]}]}}`, h.PatchOpenAICompat); rec.Code != http.StatusBadRequest {
		t.Fatalf("patch cross-provider identity conflict: status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	if cfg.OpenAICompatibility[1].Models[0].Name != "glm" {
		t.Fatalf("rejected cross-provider patch changed config: %+v", cfg.OpenAICompatibility[1].Models)
	}
}
