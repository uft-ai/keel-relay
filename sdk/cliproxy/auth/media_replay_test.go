package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// mediaReplayExecutor answers each call with the next queued failure, then succeeds.
// It marks the exchange as sent unless unsent is set.
type mediaReplayExecutor struct {
	provider string
	unsent   bool
	mu       sync.Mutex
	failures []error
	calls    []string
	ctx      context.Context
	// streamErr, when set, ends the next opened stream after payloads.
	streamErr error
	payloads  [][]byte
}

func (e *mediaReplayExecutor) Identifier() string { return e.provider }

func (e *mediaReplayExecutor) Execute(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if !e.unsent {
		cliproxyexecutor.MarkUpstreamAttempt(ctx)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ctx = ctx
	e.calls = append(e.calls, auth.ID)
	if len(e.failures) > 0 {
		failure := e.failures[0]
		e.failures = e.failures[1:]
		return cliproxyexecutor.Response{}, failure
	}
	return cliproxyexecutor.Response{Payload: []byte(`{}`)}, nil
}

func (e *mediaReplayExecutor) ExecuteTranscription(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.Execute(ctx, auth, req, opts)
}

// ExecuteStream fails at start with the next queued failure. With streamErr set it
// opens the stream instead and sends payloads (if any) before streamErr.
func (e *mediaReplayExecutor) ExecuteStream(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if !e.unsent {
		cliproxyexecutor.MarkUpstreamAttempt(ctx)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ctx = ctx
	e.calls = append(e.calls, auth.ID)
	headers := http.Header{"X-Provider": []string{"kept"}}
	if e.streamErr != nil {
		chunks := make(chan cliproxyexecutor.StreamChunk, len(e.payloads)+1)
		for _, payload := range e.payloads {
			chunks <- cliproxyexecutor.StreamChunk{Payload: payload}
		}
		chunks <- cliproxyexecutor.StreamChunk{Err: e.streamErr}
		close(chunks)
		e.streamErr = nil
		return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: chunks}, nil
	}
	if len(e.failures) > 0 {
		failure := e.failures[0]
		e.failures = e.failures[1:]
		return nil, failure
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`data: {}`)}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: chunks}, nil
}
func (e *mediaReplayExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) { return auth, nil }
func (e *mediaReplayExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *mediaReplayExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *mediaReplayExecutor) callList() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

// mediaReplayFixture registers the given keys of one compatible entry on baseURL,
// with a transcription model and a chat model.
func mediaReplayFixture(t *testing.T, baseURL string, retry int, keys ...string) (*Manager, *mediaReplayExecutor, []*Auth) {
	t.Helper()
	if len(keys) == 0 {
		keys = []string{"synthetic-media-a", "synthetic-media-b"}
	}
	var entries []internalconfig.OpenAICompatibilityAPIKey
	for _, key := range keys {
		entries = append(entries, internalconfig.OpenAICompatibilityAPIKey{APIKey: key})
	}
	cfg := &internalconfig.Config{DisableCooling: true, RequestRetry: retry, OpenAICompatibility: []internalconfig.OpenAICompatibility{{
		Name: "media", Prefix: "media", BaseURL: baseURL,
		APIKeyEntries: entries,
		Models: []internalconfig.OpenAICompatibilityModel{
			{Name: "whisper", Alias: "whisper", Operations: []string{"transcription"}},
			{Name: "chat-model", Alias: "chat-model", Operations: []string{"chat"}},
			{Name: "image-model", Alias: "image-model", Operations: []string{"image-generation"}},
		},
	}}}
	m := NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	m.SetRetryConfig(retry, time.Second, 3)
	store, err := NewFilePeriodStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := NewPeriodLedger(t.Context(), store, nil)
	if err != nil {
		t.Fatal(err)
	}
	m.SetPeriodLedger(ledger)
	executor := &mediaReplayExecutor{provider: "openai-compatible-media"}
	m.RegisterExecutor(executor)
	var auths []*Auth
	for i, key := range keys {
		auth := &Auth{ID: fmt.Sprintf("%s-%d", t.Name(), i), Provider: executor.provider, Prefix: "media", Attributes: map[string]string{
			AttributeAuthKind: AuthKindAPIKey, AttributeAPIKey: key, AttributeSource: "config:media",
			"base_url": baseURL, "compat_name": "media", "provider_key": "media",
		}}
		registerCapabilityTestAuth(t, m, auth)
		var models []*registry.ModelInfo
		for _, name := range []string{"whisper", "chat-model", "image-model"} {
			models = append(models, &registry.ModelInfo{ID: name}, &registry.ModelInfo{ID: "media/" + name})
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, models)
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
		m.RefreshSchedulerEntry(auth.ID)
		stored, _ := m.GetByID(auth.ID)
		auths = append(auths, stored)
	}
	return m, executor, auths
}

func statusOf(err error) int {
	var status interface{ StatusCode() int }
	if errors.As(err, &status) {
		return status.StatusCode()
	}
	return 0
}

var transcription = cliproxyexecutor.Options{SourceFormat: "openai-transcription"}

// After a sent media exchange ends without a usable answer, the provider may have
// done the work, so the request ends on that credential.
func TestSentMediaFailureDoesNotMoveToAnotherCredential(t *testing.T) {
	for _, test := range []struct {
		name    string
		failure error
		status  int
	}{
		{name: "server error", failure: &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "busy"}, status: http.StatusServiceUnavailable},
		{name: "timeout", failure: &Error{HTTPStatus: http.StatusRequestTimeout, Message: "timeout"}, status: http.StatusRequestTimeout},
		{name: "gateway timeout after work", failure: &Error{HTTPStatus: 524, Message: "may have completed"}, status: 524},
		{name: "disconnect", failure: errors.New("unexpected EOF"), status: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, executor, _ := mediaReplayFixture(t, "https://gateway.example/v1", 3)
			executor.failures = []error{test.failure}
			_, err := m.Execute(t.Context(), []string{executor.provider}, cliproxyexecutor.Request{Model: "media/whisper"}, transcription)
			if err == nil || statusOf(err) != test.status || len(executor.callList()) != 1 {
				t.Fatalf("calls=%v err=%v", executor.callList(), err)
			}
		})
	}
}

// With one key and retries configured, the outer retry rounds do not send the
// stopped media request again.
func TestSentMediaFailureIsNotRetriedOnTheSameKey(t *testing.T) {
	m, executor, _ := mediaReplayFixture(t, "https://gateway.example/v1", 3, "synthetic-media-only")
	// 503 is a status the outer retry rounds normally retry.
	executor.failures = []error{&Error{HTTPStatus: http.StatusServiceUnavailable, Message: "may have completed"}}
	_, err := m.Execute(t.Context(), []string{executor.provider}, cliproxyexecutor.Request{Model: "media/whisper"}, transcription)
	if statusOf(err) != http.StatusServiceUnavailable || len(executor.callList()) != 1 {
		t.Fatalf("calls=%v err=%v", executor.callList(), err)
	}
}

// Explicit credential rejections keep CPA's normal failover, and so does a media
// failure that never reached the provider.
func TestMediaKeepsFailoverWhenNoWorkCanHaveHappened(t *testing.T) {
	for _, test := range []struct {
		name    string
		failure error
		unsent  bool
	}{
		{name: "rate limited", failure: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "slow down"}},
		{name: "unauthorized", failure: &Error{HTTPStatus: http.StatusUnauthorized, Message: "bad key"}},
		{name: "not sent", failure: &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "busy"}, unsent: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, executor, _ := mediaReplayFixture(t, "https://gateway.example/v1", 0)
			executor.failures, executor.unsent = []error{test.failure}, test.unsent
			_, err := m.Execute(t.Context(), []string{executor.provider}, cliproxyexecutor.Request{Model: "media/whisper"}, transcription)
			calls := executor.callList()
			if err != nil || len(calls) != 2 || calls[0] == calls[1] {
				t.Fatalf("calls=%v err=%v", calls, err)
			}
		})
	}
}

func TestChatServerErrorStillMovesToAnotherCredential(t *testing.T) {
	m, executor, _ := mediaReplayFixture(t, "https://gateway.example/v1", 0)
	executor.failures = []error{&Error{HTTPStatus: http.StatusServiceUnavailable, Message: "busy"}}
	_, err := m.Execute(t.Context(), []string{executor.provider}, cliproxyexecutor.Request{Model: "media/chat-model"}, cliproxyexecutor.Options{})
	if calls := executor.callList(); err != nil || len(calls) != 2 || calls[0] == calls[1] {
		t.Fatalf("calls=%v err=%v", calls, err)
	}
}

// A NaN monthly cap is recorded for its key before the request moves on, and later
// requests skip that key. Two keys are not assumed to share an allowance.
func TestNanMonthlyCapOnMediaRecordsTheKeyAndMovesOn(t *testing.T) {
	m, executor, auths := mediaReplayFixture(t, "https://api.nan.builders/v1", 0)
	executor.failures = []error{&Error{HTTPStatus: http.StatusPaymentRequired, Message: `{"error":{"code":"monthly_cap_reached"}}`}}
	if _, err := m.Execute(t.Context(), []string{executor.provider}, cliproxyexecutor.Request{Model: "media/whisper"}, transcription); err != nil {
		t.Fatalf("second key did not serve: %v", err)
	}
	calls := executor.callList()
	if len(calls) != 2 || calls[0] == calls[1] {
		t.Fatalf("calls=%v", calls)
	}
	byID := map[string]*Auth{auths[0].ID: auths[0], auths[1].ID: auths[1]}
	if !m.PeriodLedger().IsBlocked(byID[calls[0]].Index, "whisper") || m.PeriodLedger().IsBlocked(byID[calls[1]].Index, "whisper") {
		t.Fatal("the monthly cap was not recorded for exactly the first key")
	}
	for range 2 {
		if _, err := m.Execute(t.Context(), []string{executor.provider}, cliproxyexecutor.Request{Model: "media/whisper"}, transcription); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range executor.callList()[2:] {
		if id == calls[0] {
			t.Fatalf("the blocked key served a later request: %v", executor.callList())
		}
	}
}

var imageStream = cliproxyexecutor.Options{SourceFormat: "openai-image"}

// streamError opens a stream and returns its first chunk error and headers.
func streamError(t *testing.T, m *Manager, provider, model string, opts cliproxyexecutor.Options) (error, http.Header, error) {
	t.Helper()
	result, err := m.ExecuteStream(t.Context(), []string{provider}, cliproxyexecutor.Request{Model: model}, opts)
	if err != nil {
		return nil, nil, err
	}
	var chunkErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil && chunkErr == nil {
			chunkErr = chunk.Err
		}
	}
	return chunkErr, result.Headers, nil
}

func stoppedWithHeader(err error) bool {
	headers, ok := err.(interface{ Headers() http.Header })
	return isRequestStopError(err) && ok && headers.Headers().Get("X-Provider") == "kept"
}

// A streamed image request that may have reached the provider ends on that
// credential, whether it failed to start, failed before its first payload or
// failed after it.
func TestSentImageStreamFailureDoesNotMoveToAnotherCredential(t *testing.T) {
	busy := func() error { return &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "busy"} }
	t.Run("at start", func(t *testing.T) {
		m, executor, _ := mediaReplayFixture(t, "https://gateway.example/v1", 3)
		executor.failures = []error{busy()}
		_, _, err := streamError(t, m, executor.provider, "media/image-model", imageStream)
		if statusOf(err) != http.StatusServiceUnavailable || len(executor.callList()) != 1 {
			t.Fatalf("calls=%v err=%v", executor.callList(), err)
		}
	})
	for _, test := range []struct {
		name    string
		failure error
		status  int
	}{
		{name: "before the first payload", failure: busy(), status: http.StatusServiceUnavailable},
		{name: "disconnect before the first payload", failure: errors.New("unexpected EOF"), status: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, executor, _ := mediaReplayFixture(t, "https://gateway.example/v1", 3)
			executor.streamErr = test.failure
			chunkErr, headers, err := streamError(t, m, executor.provider, "media/image-model", imageStream)
			if err != nil || statusOf(chunkErr) != test.status || len(executor.callList()) != 1 {
				t.Fatalf("calls=%v err=%v chunk=%v", executor.callList(), err, chunkErr)
			}
			if !stoppedWithHeader(chunkErr) || headers.Get("X-Provider") != "kept" {
				t.Fatalf("the stop or the provider headers are missing: %#v %v", chunkErr, headers)
			}
		})
	}
	t.Run("one key with retries", func(t *testing.T) {
		m, executor, _ := mediaReplayFixture(t, "https://gateway.example/v1", 3, "synthetic-media-only")
		executor.streamErr = busy()
		chunkErr, _, err := streamError(t, m, executor.provider, "media/image-model", imageStream)
		if err != nil || statusOf(chunkErr) != http.StatusServiceUnavailable || len(executor.callList()) != 1 {
			t.Fatalf("calls=%v err=%v chunk=%v", executor.callList(), err, chunkErr)
		}
	})
}

// After the first payload, an ambiguous media error carries the stop; an explicit
// rejection does not.
func TestImageStreamErrorAfterPayloadCarriesTheStop(t *testing.T) {
	for _, test := range []struct {
		name    string
		failure error
		stopped bool
	}{
		{name: "server error", failure: &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "busy"}, stopped: true},
		{name: "rate limited", failure: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "slow down"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, executor, _ := mediaReplayFixture(t, "https://gateway.example/v1", 3)
			executor.streamErr, executor.payloads = test.failure, [][]byte{[]byte(`data: {"partial":true}`)}
			chunkErr, _, err := streamError(t, m, executor.provider, "media/image-model", imageStream)
			if err != nil || chunkErr == nil || isRequestStopError(chunkErr) != test.stopped || len(executor.callList()) != 1 {
				t.Fatalf("calls=%v err=%v chunk=%#v", executor.callList(), err, chunkErr)
			}
			if !test.stopped && chunkErr != test.failure {
				t.Fatalf("an explicit rejection changed shape: %#v", chunkErr)
			}
		})
	}
}

// Streams keep CPA's normal failover where no work can have happened, and chat
// streams keep it after a server error.
func TestStreamsKeepFailoverOtherwise(t *testing.T) {
	for _, test := range []struct {
		name    string
		model   string
		opts    cliproxyexecutor.Options
		failure error
	}{
		{name: "image rate limited", model: "media/image-model", opts: imageStream, failure: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "slow down"}},
		{name: "image unauthorized", model: "media/image-model", opts: imageStream, failure: &Error{HTTPStatus: http.StatusUnauthorized, Message: "bad key"}},
		{name: "chat server error", model: "media/chat-model", opts: cliproxyexecutor.Options{}, failure: &Error{HTTPStatus: http.StatusServiceUnavailable, Message: "busy"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, executor, _ := mediaReplayFixture(t, "https://gateway.example/v1", 0)
			executor.failures = []error{test.failure}
			chunkErr, _, err := streamError(t, m, executor.provider, test.model, test.opts)
			calls := executor.callList()
			if err != nil || chunkErr != nil || len(calls) != 2 || calls[0] == calls[1] {
				t.Fatalf("calls=%v err=%v chunk=%v", calls, err, chunkErr)
			}
		})
	}
}

func TestHomeSentImageStreamFailureEndsTheAttemptWithoutRedispatch(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}, RequestRetry: 3})
	manager.SetRetryConfig(3, time.Second, 3)
	manager.PublishHomeDispatch(homeExecutionDispatcher{}, executionregistry.New(), 1)
	executor := &mediaReplayExecutor{provider: "home-execution", failures: []error{&Error{HTTPStatus: http.StatusBadGateway, Message: "lost"}}}
	manager.RegisterExecutor(executor)
	_, _, err := streamError(t, manager, "home-execution", "image-model", imageStream)
	if statusOf(err) != http.StatusBadGateway || len(executor.callList()) != 1 {
		t.Fatalf("calls=%v err=%v", executor.callList(), err)
	}
	if executor.ctx == nil || executor.ctx.Err() == nil {
		t.Fatal("the Home attempt was not released")
	}
}

func TestHomeSentMediaFailureEndsTheAttemptWithoutRedispatch(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}, RequestRetry: 3})
	manager.SetRetryConfig(3, time.Second, 3)
	manager.PublishHomeDispatch(homeExecutionDispatcher{}, executionregistry.New(), 1)
	executor := &mediaReplayExecutor{provider: "home-execution", failures: []error{&Error{HTTPStatus: http.StatusBadGateway, Message: "lost"}}}
	manager.RegisterExecutor(executor)
	_, err := manager.Execute(t.Context(), []string{"home-execution"}, cliproxyexecutor.Request{Model: "whisper"}, transcription)
	if statusOf(err) != http.StatusBadGateway || len(executor.callList()) != 1 {
		t.Fatalf("calls=%v err=%v", executor.callList(), err)
	}
	if executor.ctx == nil || executor.ctx.Err() == nil {
		t.Fatal("the Home attempt was not released")
	}
}
