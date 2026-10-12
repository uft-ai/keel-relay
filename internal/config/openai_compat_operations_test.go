package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestValidateOpenAICompatModels(t *testing.T) {
	tests := []struct {
		name    string
		models  []OpenAICompatibilityModel
		wantErr string
	}{
		{name: "legacy entry without metadata", models: []OpenAICompatibilityModel{{Name: "qwen3.6"}}},
		{name: "legacy pool keeps loading", models: []OpenAICompatibilityModel{{Name: "a", Alias: "pool"}, {Name: "b", Alias: "pool"}}},
		{
			name: "native alias beside translated alias",
			models: []OpenAICompatibilityModel{
				{Name: "qwen3.6", Alias: "qwen3.6"},
				{Name: "qwen3.6", Alias: "qwen3.6-native", Operations: []string{"chat", "responses", "completions"}, Native: []string{"responses", "completions"}},
			},
		},
		{name: "native without operations", models: []OpenAICompatibilityModel{{Name: "deepseek", Native: []string{"responses"}}}},
		{name: "image operations", models: []OpenAICompatibilityModel{{Name: "flux", Image: true, Operations: []string{"image-generation", "image-edit"}}}},
		{name: "utility operation", models: []OpenAICompatibilityModel{{Name: "rerank", Operations: []string{"rerank"}}}},
		{name: "empty operations", models: []OpenAICompatibilityModel{{Name: "m", Operations: []string{}}}, wantErr: "operations must not be empty"},
		{name: "empty native", models: []OpenAICompatibilityModel{{Name: "m", Native: []string{}}}, wantErr: "native must not be empty"},
		{name: "unknown operation", models: []OpenAICompatibilityModel{{Name: "m", Operations: []string{"video"}}}, wantErr: `unknown operation "video"`},
		{name: "unsupported native", models: []OpenAICompatibilityModel{{Name: "m", Native: []string{"chat"}}}, wantErr: `unsupported native operation "chat"`},
		{name: "native outside operations", models: []OpenAICompatibilityModel{{Name: "m", Operations: []string{"chat"}, Native: []string{"responses"}}}, wantErr: `"responses" is not listed in operations`},
		{name: "image op without image flag", models: []OpenAICompatibilityModel{{Name: "m", Operations: []string{"image-generation"}}}, wantErr: "requires image: true"},
		{name: "image model with chat", models: []OpenAICompatibilityModel{{Name: "m", Image: true, Operations: []string{"chat"}}}, wantErr: "image models may only declare image operations"},
		{name: "negative output", models: []OpenAICompatibilityModel{{Name: "m", MaxOutputTokens: -1}}, wantErr: "max-output-tokens must not be negative"},
		{
			name: "shared alias with different metadata",
			models: []OpenAICompatibilityModel{
				{Name: "a", Alias: "pool"},
				{Name: "b", Alias: "POOL", Native: []string{"responses"}},
			},
			wantErr: "must declare identical operations and native lists",
		},
		{
			name: "shared alias with different output limit",
			models: []OpenAICompatibilityModel{
				{Name: "a", Alias: "pool"},
				{Name: "b", Alias: "pool", MaxOutputTokens: 8192},
			},
			wantErr: "must declare identical operations, native and max-output-tokens",
		},
		{
			name: "shared alias image flag is left to the image fence",
			models: []OpenAICompatibilityModel{
				{Name: "a", Alias: "pool", MaxOutputTokens: 4096},
				{Name: "b", Alias: "pool", Image: true, MaxOutputTokens: 4096},
			},
		},
		{
			name: "shared alias modalities may differ",
			models: []OpenAICompatibilityModel{
				{Name: "a", Alias: "pool", Operations: []string{"chat"}, InputModalities: []string{"text"}},
				{Name: "b", Alias: "pool", Operations: []string{"chat"}, InputModalities: []string{"text", "image"}},
			},
		},
		{
			name: "shared alias targets may spell max tokens differently",
			models: []OpenAICompatibilityModel{
				{Name: "a", Alias: "pool", Operations: []string{"chat"}, MaxOutputTokens: 4096},
				{Name: "b", Alias: "pool", Operations: []string{"chat"}, MaxOutputTokens: 4096, UseMaxCompletionTokens: true},
			},
		},
		{
			name: "shared alias native differs",
			models: []OpenAICompatibilityModel{
				{Name: "a", Alias: "pool", Operations: []string{"chat", "responses"}},
				{Name: "b", Alias: "pool", Operations: []string{"chat", "responses"}, Native: []string{"responses"}},
			},
			wantErr: "must declare identical operations and native lists",
		},
		{
			name:   "legacy pool with differing image flag is unchanged",
			models: []OpenAICompatibilityModel{{Name: "a", Alias: "pool"}, {Name: "b", Alias: "pool", Image: true}},
		},
		{
			name: "shared alias with identical metadata",
			models: []OpenAICompatibilityModel{
				{Name: "a", Alias: "pool", Operations: []string{"chat", "responses"}},
				{Name: "b", Alias: "pool", Operations: []string{"responses", "chat"}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			NormalizeOpenAICompatModels(tt.models)
			err := ValidateOpenAICompatModels(tt.models)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestOpenAICompatModelOperationHelpers(t *testing.T) {
	legacy := OpenAICompatibilityModel{Name: "m"}
	if !legacy.AllowsOperation(OpenAICompatOperationEmbeddings) || legacy.IsNative(OpenAICompatOperationResponses) {
		t.Fatal("unset metadata must keep legacy behavior")
	}
	fenced := OpenAICompatibilityModel{Name: "m", Operations: []string{"rerank"}, Native: nil}
	if fenced.AllowsOperation(OpenAICompatOperationChat) || !fenced.AllowsOperation(OpenAICompatOperationRerank) {
		t.Fatal("operations must fence other operations")
	}
}

func TestOpenAICompatOperationsLoadAndRoundTrip(t *testing.T) {
	raw := `api-keys:
  openai-compatibility:
    - name: nan
      prefix: nan
      base-url: https://api.nan.builders/v1
      keys: [{api-key: test-key}]
      models:
        - {name: qwen3.6, alias: qwen3.6}
        - {name: qwen3.6, alias: qwen3.6-native, operations: [Chat, responses, completions], native: [responses, completions], max-output-tokens: 8192}
        - {name: rerank, alias: rerank, operations: [rerank]}
`
	if err := ValidateV8Config([]byte(raw)); err != nil {
		t.Fatalf("ValidateV8Config: %v", err)
	}
	cfg, err := ParseConfigBytes([]byte(raw))
	if err != nil {
		t.Fatalf("ParseConfigBytes: %v", err)
	}
	if len(cfg.OpenAICompatibility) != 1 || len(cfg.OpenAICompatibility[0].Models) != 3 {
		t.Fatalf("unexpected config: %+v", cfg.OpenAICompatibility)
	}
	models := cfg.OpenAICompatibility[0].Models
	if models[0].Operations != nil || models[0].Native != nil || models[0].MaxOutputTokens != 0 {
		t.Fatalf("legacy entry gained metadata: %+v", models[0])
	}
	native := models[1]
	if strings.Join(native.Operations, ",") != "chat,responses,completions" || !native.IsNative("completions") || native.MaxOutputTokens != 8192 {
		t.Fatalf("native entry not normalized: %+v", native)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err = os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !loaded.OpenAICompatibility[0].Models[2].AllowsOperation("rerank") || loaded.OpenAICompatibility[0].Models[2].AllowsOperation("chat") {
		t.Fatal("loaded rerank entry lost its operations")
	}

	out, err := yaml.Marshal(loaded.OpenAICompatibility[0].Models)
	if err != nil {
		t.Fatal(err)
	}
	var back []OpenAICompatibilityModel
	if err = yaml.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if back[0].Operations != nil || strings.Join(back[1].Native, ",") != "responses,completions" || back[1].MaxOutputTokens != 8192 {
		t.Fatalf("round trip changed metadata: %+v", back)
	}
}

func TestOpenAICompatOperationsRejectedOnLoad(t *testing.T) {
	for name, models := range map[string]string{
		"empty list":    `[{name: m, operations: []}]`,
		"unknown value": `[{name: m, operations: [video]}]`,
	} {
		t.Run(name, func(t *testing.T) {
			raw := "openai-compatibility:\n  - name: nan\n    base-url: https://api.nan.builders/v1\n    models: " + models + "\n"
			if _, err := ParseConfigBytes([]byte(raw)); err == nil {
				t.Fatal("expected config rejection")
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(path); err == nil {
				t.Fatal("expected LoadConfig rejection")
			}
		})
	}
}

func TestValidateOpenAICompatProvidersAcrossPrefix(t *testing.T) {
	native := OpenAICompatibilityModel{Name: "qwen3.6", Alias: "qwen3.6", Operations: []string{"chat", "responses"}, Native: []string{"responses"}}
	legacy := OpenAICompatibilityModel{Name: "qwen3.6", Alias: "qwen3.6"}
	tests := []struct {
		name      string
		providers []OpenAICompatibility
		wantErr   string
	}{
		{
			name: "same prefix and alias with different metadata",
			providers: []OpenAICompatibility{
				{Name: "nan-a", Prefix: "nan", Models: []OpenAICompatibilityModel{legacy}},
				{Name: "nan-b", Prefix: "nan/", Models: []OpenAICompatibilityModel{native}},
			},
			wantErr: "with different routing metadata",
		},
		{
			name: "same prefix and alias with identical metadata",
			providers: []OpenAICompatibility{
				{Name: "nan-a", Prefix: "nan", Models: []OpenAICompatibilityModel{native}},
				{Name: "nan-b", Prefix: "NAN", Models: []OpenAICompatibilityModel{native}},
			},
		},
		{
			name: "different prefixes are separate identities",
			providers: []OpenAICompatibility{
				{Name: "nan", Prefix: "nan", Models: []OpenAICompatibilityModel{native}},
				{Name: "other", Prefix: "other", Models: []OpenAICompatibilityModel{legacy}},
			},
		},
		{
			name: "legacy providers sharing an alias are unchanged",
			providers: []OpenAICompatibility{
				{Name: "a", Models: []OpenAICompatibilityModel{legacy}},
				{Name: "b", Models: []OpenAICompatibilityModel{{Name: "other", Alias: "qwen3.6", Image: true}}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateOpenAICompatProviders(tt.providers)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
