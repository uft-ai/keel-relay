package auth

import (
	"context"
	"slices"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func disabledRowAuth() *Auth {
	return &Auth{
		ID:       "disabled-row-auth",
		Provider: openAICompatPoolProviderKey,
		Attributes: map[string]string{
			"api_key":      "test-key",
			"compat_name":  "pool",
			"provider_key": openAICompatPoolProviderKey,
		},
	}
}

func disabledRowConfig(models ...internalconfig.OpenAICompatibilityModel) *internalconfig.Config {
	return &internalconfig.Config{OpenAICompatibility: []internalconfig.OpenAICompatibility{{Name: "pool", Models: models}}}
}

func TestDisabledRowLeavesTheRestOfItsPool(t *testing.T) {
	alias := "shared-alias"
	executor := &openAICompatPoolExecutor{id: openAICompatPoolProviderKey}
	m := newOpenAICompatPoolTestManager(t, alias, []internalconfig.OpenAICompatibilityModel{
		{Name: "first-upstream", Alias: alias, Disabled: true},
		{Name: "second-upstream", Alias: alias},
	}, executor)

	for i := 0; i < 2; i++ {
		if _, err := m.Execute(context.Background(), []string{openAICompatPoolProviderKey}, cliproxyexecutor.Request{Model: alias}, cliproxyexecutor.Options{}); err != nil {
			t.Fatalf("execute %d: %v", i, err)
		}
	}
	if got := executor.ExecuteModels(); !slices.Equal(got, []string{"second-upstream", "second-upstream"}) {
		t.Fatalf("execute models = %v, want only the enabled pool member", got)
	}
}

func TestDisabledRowDoesNotShadowAnEnabledRow(t *testing.T) {
	cfg := disabledRowConfig(
		internalconfig.OpenAICompatibilityModel{Name: "hidden-upstream", Alias: "public", Disabled: true},
		internalconfig.OpenAICompatibilityModel{Name: "served-upstream", Alias: "public"},
	)
	auth := disabledRowAuth()
	if got := resolveUpstreamModelForOpenAICompatAPIKey(cfg, auth, "public"); got != "served-upstream" {
		t.Fatalf("upstream = %q, want served-upstream", got)
	}
	if got := resolveModelAliasPoolFromConfigModels("public", asModelAliasEntries(internalconfig.EnabledOpenAICompatModels(cfg.OpenAICompatibility[0].Models))); !slices.Equal(got, []string{"served-upstream"}) {
		t.Fatalf("pool = %v, want [served-upstream]", got)
	}
}

func TestDisabledAliasIsRefusedInEveryForm(t *testing.T) {
	cfg := disabledRowConfig(
		internalconfig.OpenAICompatibilityModel{Name: "upstream", Alias: "off-alias", Disabled: true},
		internalconfig.OpenAICompatibilityModel{Name: "upstream", Alias: "on-alias", ForceMapping: true},
	)
	cfg.OpenAICompatibility[0].Prefix = "tenant"
	auth := disabledRowAuth()
	auth.Prefix = "tenant"
	for _, requested := range []string{"off-alias", "tenant/off-alias", "off-alias(high)"} {
		if got := resolveUpstreamModelForOpenAICompatAPIKey(cfg, auth, requested); got != "" {
			t.Fatalf("%s resolved to %q through a disabled row", requested, got)
		}
	}
	if got := resolveUpstreamModelForOpenAICompatAPIKey(cfg, auth, "on-alias(high)"); got != "upstream(high)" {
		t.Fatalf("enabled alias resolved to %q, want upstream(high)", got)
	}

	capabilities := compileAPIKeyModelCapabilitiesForAuth(cfg, auth)
	if _, ok := capabilities["off-alias"]; ok {
		t.Fatal("disabled alias kept a capability route")
	}
	if routes := capabilities["on-alias"]; len(routes) != 1 || routes[0].upstreamModel != "upstream" {
		t.Fatalf("enabled alias routes = %+v", routes)
	}
	if entries := configuredModelAliasEntries(cfg, auth); len(entries) != 1 || entries[0].GetAlias() != "on-alias" || !entries[0].GetForceMapping() {
		t.Fatalf("alias entries = %+v, want only the enabled force-mapped alias", entries)
	}
}

func TestDisabledRowFollowsAConfigReload(t *testing.T) {
	m := NewManager(nil, nil, nil)
	auth := disabledRowAuth()
	auth.Status = StatusActive
	m.SetConfig(disabledRowConfig(internalconfig.OpenAICompatibilityModel{Name: "upstream", Alias: "public"}))
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	if got := m.applyAPIKeyModelAlias(auth, "public"); got != "upstream" {
		t.Fatalf("enabled alias = %q, want upstream", got)
	}

	m.SetConfig(disabledRowConfig(internalconfig.OpenAICompatibilityModel{Name: "upstream", Alias: "public", Disabled: true}))
	if got := m.applyAPIKeyModelAlias(auth, "public"); got == "upstream" {
		t.Fatal("alias still resolves after the row was disabled")
	}

	m.SetConfig(disabledRowConfig(internalconfig.OpenAICompatibilityModel{Name: "upstream", Alias: "public"}))
	if got := m.applyAPIKeyModelAlias(auth, "public"); got != "upstream" {
		t.Fatalf("re-enabled alias = %q, want upstream", got)
	}
}
