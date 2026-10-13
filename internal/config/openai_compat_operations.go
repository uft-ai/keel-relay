package config

import (
	"fmt"
	"slices"
	"strings"
)

// Client operations an openai-compatibility model entry may declare in `operations`.
const (
	OpenAICompatOperationChat            = "chat"
	OpenAICompatOperationCompletions     = "completions"
	OpenAICompatOperationResponses       = "responses"
	OpenAICompatOperationEmbeddings      = "embeddings"
	OpenAICompatOperationRerank          = "rerank"
	OpenAICompatOperationSpeech          = "speech"
	OpenAICompatOperationTranscription   = "transcription"
	OpenAICompatOperationImageGeneration = "image-generation"
	OpenAICompatOperationImageEdit       = "image-edit"
)

var openAICompatOperations = []string{
	OpenAICompatOperationChat,
	OpenAICompatOperationCompletions,
	OpenAICompatOperationResponses,
	OpenAICompatOperationEmbeddings,
	OpenAICompatOperationRerank,
	OpenAICompatOperationSpeech,
	OpenAICompatOperationTranscription,
	OpenAICompatOperationImageGeneration,
	OpenAICompatOperationImageEdit,
}

var openAICompatNativeOperations = []string{
	OpenAICompatOperationResponses,
	OpenAICompatOperationCompletions,
}

func isOpenAICompatImageOperation(op string) bool {
	return op == OpenAICompatOperationImageGeneration || op == OpenAICompatOperationImageEdit
}

// AllowsOperation reports whether the entry may serve op. An entry without an
// operations list keeps the legacy behavior and is not restricted here.
func (m OpenAICompatibilityModel) AllowsOperation(op string) bool {
	if m.Operations == nil {
		return true
	}
	return slices.Contains(m.Operations, op)
}

// HasDispatchMetadata reports whether the entry sets operations or native.
// Home dispatch does not implement these fields, so callers reject them there.
func (m OpenAICompatibilityModel) HasDispatchMetadata() bool {
	return m.Operations != nil || m.Native != nil
}

// IsNative reports whether op is forwarded natively for this entry.
func (m OpenAICompatibilityModel) IsNative(op string) bool {
	return slices.Contains(m.Native, op)
}

// normalizeOperationList trims, lowercases and de-duplicates values. A nil list
// stays nil (unset); a present list stays non-nil even when it ends up empty so
// validation can reject it.
func normalizeOperationList(values []string) []string {
	if values == nil {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" || slices.Contains(out, value) {
			continue
		}
		out = append(out, value)
	}
	return out
}

// EnabledOpenAICompatModels returns the rows that may serve requests. Callers
// that select catalog, alias, pool, capability or executor options use it so a
// disabled row can neither serve nor shadow an enabled row.
func EnabledOpenAICompatModels(models []OpenAICompatibilityModel) []OpenAICompatibilityModel {
	disabled := func(model OpenAICompatibilityModel) bool { return model.Disabled }
	if !slices.ContainsFunc(models, disabled) {
		return models
	}
	return slices.DeleteFunc(slices.Clone(models), disabled)
}

// NormalizeOpenAICompatModels normalizes operation lists in place.
func NormalizeOpenAICompatModels(models []OpenAICompatibilityModel) {
	for i := range models {
		models[i].Operations = normalizeOperationList(models[i].Operations)
		models[i].Native = normalizeOperationList(models[i].Native)
	}
}

// ValidateOpenAICompatibilityModels rejects model entries whose operation
// metadata would be ambiguous or would silently widen access.
func (cfg *Config) ValidateOpenAICompatibilityModels() error {
	if cfg == nil {
		return nil
	}
	return ValidateOpenAICompatProviders(cfg.OpenAICompatibility)
}

// ValidateOpenAICompatProviders validates every provider's model list and then
// the client-visible identity prefix + alias across providers: entries that
// publish the same identity must agree on routing metadata whenever any of them
// sets operation metadata. Groups made only of legacy entries are unchanged.
// Unprefixed exposure of a prefixed model is not an identity here; dispatch
// checks the entry bound to each attempt.
func ValidateOpenAICompatProviders(providers []OpenAICompatibility) error {
	type member struct {
		provider, model int
	}
	groups := make(map[string][]member)
	order := make([]string, 0)
	for i := range providers {
		if errValidate := ValidateOpenAICompatModels(providers[i].Models); errValidate != nil {
			return fmt.Errorf("openai-compatibility[%d] (%s): %w", i, providers[i].Name, errValidate)
		}
		prefix := strings.ToLower(strings.Trim(strings.TrimSpace(providers[i].Prefix), "/"))
		for j := range providers[i].Models {
			alias := openAICompatAliasKey(providers[i].Models[j])
			if alias == "" {
				continue
			}
			key := prefix + "/" + alias
			if _, seen := groups[key]; !seen {
				order = append(order, key)
			}
			groups[key] = append(groups[key], member{provider: i, model: j})
		}
	}
	for _, key := range order {
		members := groups[key]
		if len(members) < 2 {
			continue
		}
		restricted := false
		for _, m := range members {
			if hasOpenAICompatOperationMetadata(providers[m.provider].Models[m.model]) {
				restricted = true
				break
			}
		}
		if !restricted {
			continue
		}
		first := providers[members[0].provider].Models[members[0].model]
		for _, m := range members[1:] {
			if m.provider == members[0].provider {
				continue
			}
			if !sameRoutingMetadata(first, providers[m.provider].Models[m.model]) {
				return fmt.Errorf("openai-compatibility[%d] (%s): model %q publishes %q like openai-compatibility[%d] (%s) with different routing metadata",
					m.provider, providers[m.provider].Name, providers[m.provider].Models[m.model].Name, strings.TrimPrefix(key, "/"),
					members[0].provider, providers[members[0].provider].Name)
			}
		}
	}
	return nil
}

func openAICompatAliasKey(model OpenAICompatibilityModel) string {
	alias := strings.ToLower(strings.TrimSpace(model.Alias))
	if alias == "" {
		alias = strings.ToLower(strings.TrimSpace(model.Name))
	}
	return alias
}

func hasOpenAICompatOperationMetadata(model OpenAICompatibilityModel) bool {
	return model.Operations != nil || model.Native != nil || model.MaxOutputTokens != 0
}

// ValidateOpenAICompatModels validates one provider's normalized model list.
func ValidateOpenAICompatModels(models []OpenAICompatibilityModel) error {
	byAlias := make(map[string]int, len(models))
	for i := range models {
		model := models[i]
		field := fmt.Sprintf("models[%d]", i)
		if model.MaxOutputTokens < 0 {
			return fmt.Errorf("%s.max-output-tokens must not be negative", field)
		}
		if model.Operations != nil {
			if len(model.Operations) == 0 {
				return fmt.Errorf("%s.operations must not be empty; omit it to keep the default behavior", field)
			}
			for _, op := range model.Operations {
				if !slices.Contains(openAICompatOperations, op) {
					return fmt.Errorf("%s.operations: unknown operation %q", field, op)
				}
				if isOpenAICompatImageOperation(op) != model.Image {
					if model.Image {
						return fmt.Errorf("%s.operations: image models may only declare image operations, got %q", field, op)
					}
					return fmt.Errorf("%s.operations: %q requires image: true", field, op)
				}
			}
		}
		if model.Native != nil {
			if len(model.Native) == 0 {
				return fmt.Errorf("%s.native must not be empty; omit it to keep translation", field)
			}
			for _, op := range model.Native {
				if !slices.Contains(openAICompatNativeOperations, op) {
					return fmt.Errorf("%s.native: unsupported native operation %q", field, op)
				}
				if !model.AllowsOperation(op) {
					return fmt.Errorf("%s.native: %q is not listed in operations", field, op)
				}
			}
		}

		alias := openAICompatAliasKey(model)
		if alias == "" {
			continue
		}
		first, seen := byAlias[alias]
		if !seen {
			byAlias[alias] = i
			continue
		}
		if !sameOperationMetadata(models[first], model) {
			return fmt.Errorf("%s: entries sharing alias %q must declare identical operations and native lists (see models[%d])", field, alias, first)
		}
		if (hasOpenAICompatOperationMetadata(models[first]) || hasOpenAICompatOperationMetadata(model)) && !sameRoutingMetadata(models[first], model) {
			return fmt.Errorf("%s: entries sharing alias %q must declare identical operations, native and max-output-tokens (see models[%d])", field, alias, first)
		}
	}
	return nil
}

func sameOperationMetadata(a, b OpenAICompatibilityModel) bool {
	return sameOperationList(a.Operations, b.Operations) && sameOperationList(a.Native, b.Native)
}

// sameRoutingMetadata compares the contract a client alias advertises: operations, native
// protocols and output limit. Per-target payload spelling (use-max-completion-tokens) and the
// image/modality fields keep their existing per-entry behavior.
func sameRoutingMetadata(a, b OpenAICompatibilityModel) bool {
	return sameOperationMetadata(a, b) && a.MaxOutputTokens == b.MaxOutputTokens
}

func sameOperationList(a, b []string) bool {
	if (a == nil) != (b == nil) || len(a) != len(b) {
		return false
	}
	for _, value := range a {
		if !slices.Contains(b, value) {
			return false
		}
	}
	return true
}
