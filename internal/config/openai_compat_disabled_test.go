package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDisabledModelRowLoadsAndRoundTrips(t *testing.T) {
	raw := `api-keys:
  openai-compatibility:
    - name: compat
      base-url: https://compat.example.test/v1
      keys: [{api-key: test-key}]
      models:
        - {name: kept-on, alias: kept-on}
        - {name: kept-off, alias: kept-off, disabled: true, operations: [chat, responses], native: [responses]}
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	models := loaded.OpenAICompatibility[0].Models
	if models[0].Disabled || !models[1].Disabled || !models[1].IsNative("responses") {
		t.Fatalf("loaded rows = %+v", models)
	}

	out, err := yaml.Marshal(models)
	if err != nil {
		t.Fatal(err)
	}
	var back []OpenAICompatibilityModel
	if err = yaml.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if back[0].Disabled || !back[1].Disabled || back[1].Alias != "kept-off" {
		t.Fatalf("round trip = %+v", back)
	}
	legacy, err := yaml.Marshal(OpenAICompatibilityModel{Name: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	if string(legacy) != "name: legacy\nalias: \"\"\n" {
		t.Fatalf("an enabled row writes the disabled field: %q", legacy)
	}
}

func TestDisabledModelRowIsStillValidated(t *testing.T) {
	models := []OpenAICompatibilityModel{{Name: "broken", Operations: []string{}, Disabled: true}}
	if err := ValidateOpenAICompatModels(models); err == nil {
		t.Fatal("an invalid disabled row was accepted")
	}
}

func TestEnabledOpenAICompatModelsKeepsTheStoredList(t *testing.T) {
	models := []OpenAICompatibilityModel{{Name: "a"}, {Name: "b", Disabled: true}, {Name: "c"}}
	enabled := EnabledOpenAICompatModels(models)
	if len(enabled) != 2 || enabled[0].Name != "a" || enabled[1].Name != "c" {
		t.Fatalf("enabled = %+v", enabled)
	}
	if len(models) != 3 || !models[1].Disabled {
		t.Fatalf("stored list changed: %+v", models)
	}
	all := []OpenAICompatibilityModel{{Name: "a"}}
	if got := EnabledOpenAICompatModels(all); &got[0] != &all[0] {
		t.Fatal("a list without disabled rows was copied")
	}
}
