package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

type operationDispatchContextKey struct{}

type operationDispatchState struct {
	mu        sync.Mutex
	routing   *apiKeyModelRoutingSnapshot
	model     string
	operation string
	fixed     bool
	native    bool
	rejected  bool
}

func (m *Manager) withOperationDispatch(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) context.Context {
	if m.HomeEnabled() {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, operationDispatchContextKey{}, &operationDispatchState{
		routing: m.loadAPIKeyModelRouting(), model: authSelectionModelFromOptions(opts, req.Model), operation: RequestedModelOperation(opts),
	})
}

func operationDispatchFromContext(ctx context.Context) *operationDispatchState {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(operationDispatchContextKey{}).(*operationDispatchState)
	return state
}

func operationListed(operations []string, operation string) bool {
	for _, candidate := range operations {
		if strings.EqualFold(strings.TrimSpace(candidate), operation) {
			return true
		}
	}
	return false
}

// Selection reads the immutable routing snapshot without acquiring Manager locks.
func (s *operationDispatchState) allows(auth *Auth) bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	byRoute := s.routing.capabilities[auth.ID]
	_, candidates := modelAliasLookupCandidates(rewriteModelForAuth(s.model, auth))
	found := false
	for _, candidate := range candidates {
		for _, route := range byRoute[strings.ToLower(strings.TrimSpace(candidate))] {
			if route.modelInfo == nil {
				continue
			}
			found = true
			info := route.modelInfo
			if info.Operations != nil && !operationListed(info.Operations, s.operation) {
				continue
			}
			if !s.fixed || s.native == operationListed(info.Native, s.operation) {
				return true
			}
		}
	}
	if !found && (!s.fixed || !s.native) {
		return true
	}
	s.rejected = true
	return false
}

func checkOperationDispatch(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) error {
	if err := checkModelOperation(req, opts); err != nil {
		return err
	}
	state := operationDispatchFromContext(ctx)
	if state == nil {
		return nil
	}
	_, native, _ := ResolvedAPIKeyModelOperations(req)
	mode := operationListed(native, RequestedModelOperation(opts))
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.fixed && state.native != mode {
		return newUnsupportedOperationError(state.model, state.operation)
	}
	state.fixed, state.native = true, mode
	return nil
}

func operationSelectionError(ctx context.Context, err error) error {
	state := operationDispatchFromContext(ctx)
	if state == nil {
		return err
	}
	var selection *Error
	if !errors.As(err, &selection) || (selection.Code != "auth_not_found" && selection.Code != "auth_unavailable") {
		return err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.rejected && !state.fixed {
		return newUnsupportedOperationError(state.model, state.operation)
	}
	return err
}

const (
	openAIImageSourceFormat    = "openai-image"
	openAIResponseSourceFormat = "openai-response"
)

// unsupportedOperationError rejects a request whose bound model entry declares an
// operations list that does not contain the requested operation. It is request-scoped,
// so it never marks the credential, cools the model down, or fails over.
type unsupportedOperationError struct {
	model     string
	operation string
	message   string
	code      string
}

func newUnsupportedOperationError(model, operation string) *unsupportedOperationError {
	return &unsupportedOperationError{
		model:     model,
		operation: operation,
		code:      "unsupported_operation",
		message:   fmt.Sprintf("model %q does not support operation %q", model, operation),
	}
}

func newHomeOperationMetadataError(model string) *unsupportedOperationError {
	return &unsupportedOperationError{
		model:   model,
		code:    "unsupported_operation",
		message: fmt.Sprintf("model %q declares operations or native metadata, which Home mode does not support", model),
	}
}

func (e *unsupportedOperationError) body() []byte {
	payload, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": e.message,
			"type":    "invalid_request_error",
			"code":    e.code,
		},
	})
	return payload
}

func (e *unsupportedOperationError) Error() string         { return string(e.body()) }
func (e *unsupportedOperationError) StatusCode() int       { return http.StatusBadRequest }
func (e *unsupportedOperationError) IsRequestScoped() bool { return true }
func (e *unsupportedOperationError) DirectResponse() bool  { return true }
func (e *unsupportedOperationError) ResponseBody() []byte  { return e.body() }

// IsUnsupportedOperationError reports whether err is an operation fence rejection.
func IsUnsupportedOperationError(err error) bool {
	var target *unsupportedOperationError
	return errors.As(err, &target) && target != nil
}

// RequestedModelOperation derives the model operation a request performs.
func RequestedModelOperation(opts cliproxyexecutor.Options) string {
	if opts.Metadata != nil {
		if raw, ok := opts.Metadata[cliproxyexecutor.RequestedOperationMetadataKey].(string); ok {
			if op := strings.ToLower(strings.TrimSpace(raw)); op != "" {
				return op
			}
		}
	}
	path := ""
	if opts.Metadata != nil {
		if raw, ok := opts.Metadata[cliproxyexecutor.RequestPathMetadataKey].(string); ok {
			path = strings.ToLower(strings.TrimRight(strings.TrimSpace(raw), "/"))
		}
	}
	switch opts.SourceFormat.String() {
	case openAIImageSourceFormat:
		if strings.HasSuffix(path, "/images/edits") {
			return internalconfig.OpenAICompatOperationImageEdit
		}
		return internalconfig.OpenAICompatOperationImageGeneration
	case openAIResponseSourceFormat:
		return internalconfig.OpenAICompatOperationResponses
	}
	if strings.HasSuffix(path, "/completions") && !strings.HasSuffix(path, "/chat/completions") {
		return internalconfig.OpenAICompatOperationCompletions
	}
	return internalconfig.OpenAICompatOperationChat
}

// Conversion belongs to the selected attempt: a native alias needs the original prompt fields.
func prepareCompletionsExecution(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Request, cliproxyexecutor.Options) {
	if opts.SourceFormat != sdktranslator.FormatOpenAI || RequestedModelOperation(opts) != internalconfig.OpenAICompatOperationCompletions {
		return req, opts
	}
	_, native, _ := ResolvedAPIKeyModelOperations(req)
	if operationListed(native, internalconfig.OpenAICompatOperationCompletions) || gjson.GetBytes(req.Payload, "messages").Exists() {
		return req, opts
	}
	req.Payload = sdktranslator.CompletionsRequestToChat(req.Payload)
	if len(opts.OriginalRequest) > 0 {
		opts.OriginalRequest = sdktranslator.CompletionsRequestToChat(opts.OriginalRequest)
	}
	return req, opts
}

// checkModelOperation enforces the bound model entry's operations list for one attempt.
// Entries without an operations list keep legacy behavior and always pass.
func checkModelOperation(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) error {
	if options, ok := ResolvedHomeModelOptions(req); ok && options.HasDispatchMetadata() {
		return newHomeOperationMetadataError(requestedModelAliasFromOptions(opts, req.Model))
	}
	operations, _, ok := ResolvedAPIKeyModelOperations(req)
	if !ok || operations == nil {
		return nil
	}
	operation := RequestedModelOperation(opts)
	for _, allowed := range operations {
		if strings.EqualFold(strings.TrimSpace(allowed), operation) {
			return nil
		}
	}
	return newUnsupportedOperationError(requestedModelAliasFromOptions(opts, req.Model), operation)
}
