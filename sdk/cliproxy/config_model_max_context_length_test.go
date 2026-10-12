package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestBuildConfigModelsPropagatesMaxContextLength(t *testing.T) {
	const want = 1048576

	tests := []struct {
		name string
		got  func() *ModelInfo
	}{
		{
			name: "codex",
			got: func() *ModelInfo {
				return buildCodexConfigModels(&config.CodexKey{
					Models: []config.CodexModel{{
						Name: "codex-upstream", Alias: "codex-alias", MaxContextLength: want,
					}},
				})[0]
			},
		},
		{
			name: "claude",
			got: func() *ModelInfo {
				return buildClaudeConfigModels(&config.ClaudeKey{
					Models: []config.ClaudeModel{{
						Name: "claude-upstream", Alias: "claude-alias", MaxContextLength: want,
					}},
				})[0]
			},
		},
		{
			name: "gemini",
			got: func() *ModelInfo {
				return buildGeminiConfigModels(&config.GeminiKey{
					Models: []config.GeminiModel{{
						Name: "gemini-upstream", Alias: "gemini-alias", MaxContextLength: want,
					}},
				})[0]
			},
		},
		{
			name: "interactions",
			got: func() *ModelInfo {
				return buildGeminiConfigModels(&config.GeminiKey{
					Models: []config.GeminiModel{{
						Name: "interactions-upstream", Alias: "interactions-alias", MaxContextLength: want,
					}},
				})[0]
			},
		},
		{
			name: "xai",
			got: func() *ModelInfo {
				return buildXAIConfigModels(&config.XAIKey{
					Models: []config.XAIModel{{
						Name: "xai-upstream", Alias: "xai-alias", MaxContextLength: want,
					}},
				})[0]
			},
		},
		{
			name: "openai compatibility",
			got: func() *ModelInfo {
				return buildOpenAICompatibilityConfigModels(&config.OpenAICompatibility{
					Models: []config.OpenAICompatibilityModel{{
						Name: "compat-upstream", Alias: "compat-alias", MaxContextLength: want,
					}},
				})[0]
			},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			model := testCase.got()
			if model == nil {
				t.Fatal("model = nil")
			}
			if model.ContextLength != want {
				t.Errorf("context length = %d, want %d", model.ContextLength, want)
			}
			if model.MaxContextLength != want {
				t.Errorf("max context length = %d, want %d", model.MaxContextLength, want)
			}
		})
	}
}

func TestBuildOpenAICompatibilityConfigModelsCarriesOperationMetadata(t *testing.T) {
	models := buildOpenAICompatibilityConfigModels(&config.OpenAICompatibility{
		Name: "nan",
		Models: []config.OpenAICompatibilityModel{
			{Name: "qwen3.6", Alias: "qwen3.6"},
			{Name: "rerank", Alias: "rerank", Operations: []string{"rerank"}, MaxOutputTokens: 0},
			{Name: "qwen3.6", Alias: "qwen3.6-native", Operations: []string{"chat", "responses"}, Native: []string{"responses"}, MaxOutputTokens: 8192},
		},
	})
	if len(models) != 3 {
		t.Fatalf("models = %d, want 3", len(models))
	}
	if models[0].Operations != nil || models[0].MaxCompletionTokens != 0 {
		t.Fatalf("legacy model gained metadata: %+v", models[0])
	}
	if len(models[1].Operations) != 1 || models[1].Operations[0] != "rerank" {
		t.Fatalf("rerank operations = %v", models[1].Operations)
	}
	if models[2].MaxCompletionTokens != 8192 {
		t.Fatalf("max completion tokens = %d, want 8192", models[2].MaxCompletionTokens)
	}
	for _, model := range models {
		if model.Native != nil {
			t.Fatalf("catalog model %s carries native %v; native is per-attempt only", model.ID, model.Native)
		}
	}
}
