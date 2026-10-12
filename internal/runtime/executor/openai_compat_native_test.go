package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

func nativeUsageCapture(t *testing.T) (context.Context, *multiProviderUsageCapture) {
	t.Helper()
	capture := &multiProviderUsageCapture{alias: t.Name(), records: make(chan coreusage.Record, 4)}
	coreusage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() { coreusage.RegisterNamedPlugin(t.Name(), multiProviderNoopUsagePlugin{}) })
	coreusage.StartDefault(context.Background())
	return coreusage.WithRequestedModelAlias(t.Context(), t.Name()), capture
}

func nativeResponsesFixture(url string) (*OpenAICompatExecutor, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) {
	executor := NewOpenAICompatExecutor("openai-compatible-nan", &config.Config{})
	auth := &cliproxyauth.Auth{Provider: "openai-compatible-nan", Attributes: map[string]string{"base_url": url + "/v1", "api_key": "fixture-key"}}
	req := cliproxyexecutor.Request{Model: "qwen3.6", Payload: []byte(`{"model":"nan/qwen3.6-native","input":"hello","instructions":"keep","max_output_tokens":256,"custom":{"keep":true}}`),
		Metadata: map[string]any{"cliproxy.resolved_api_key_model_info": &registry.ModelInfo{Operations: []string{"responses"}, Native: []string{"responses"}}}}
	return executor, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
}

func TestOpenAICompatNativeCompletions(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "JSON", true: "SSE"}[streaming], func(t *testing.T) {
			var body []byte
			first := `{"object":"text_completion","model":"qwen3.6","choices":[{"index":0,"text":"one","logprobs":{"tokens":["one"]},"finish_reason":null}],"extension":true}`
			last := `{"object":"text_completion","choices":[{"index":0,"text":"two","finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/completions" || r.Header.Get("Authorization") != "Bearer fixture-key" {
					t.Errorf("wrong native completion target or credentials")
				}
				body, _ = io.ReadAll(r.Body)
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: "+first+"\n\ndata: "+last+"\n\ndata: [DONE]\n\n")
				} else {
					_, _ = io.WriteString(w, last)
				}
			}))
			defer server.Close()
			executor, auth, req, opts := nativeResponsesFixture(server.URL)
			req.Payload = []byte(`{"model":"nan/qwen3.6-native","prompt":[[1,2],[3]],"suffix":"tail","logprobs":5,"max_tokens":32,"custom":true}`)
			req.Metadata["cliproxy.resolved_api_key_model_info"] = &registry.ModelInfo{Operations: []string{"completions"}, Native: []string{"completions"}}
			opts.SourceFormat = sdktranslator.FormatOpenAI
			opts.Stream = streaming
			opts.Metadata = map[string]any{cliproxyexecutor.RequestPathMetadataKey: "/v1/completions"}
			executor.cfg.Payload = config.PayloadConfig{
				Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "qwen3.6"}}, Params: map[string]any{"suffix": "final", "max_tokens": 19}}},
				Filter:   []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "qwen3.6"}}, Params: []string{"custom", "stream_options"}}},
			}
			ctx, capture := nativeUsageCapture(t)
			if streaming {
				stream, err := executor.ExecuteStream(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				var chunks []string
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					chunks = append(chunks, string(chunk.Payload))
				}
				if len(chunks) != 2 || chunks[0] != first || chunks[1] != last {
					t.Fatalf("native chunks changed: %v", chunks)
				}
			} else {
				response, err := executor.Execute(ctx, auth, req, opts)
				if err != nil || string(response.Payload) != last {
					t.Fatalf("native response: %s, %v", response.Payload, err)
				}
			}
			if gjson.GetBytes(body, "prompt").Raw != "[[1,2],[3]]" || gjson.GetBytes(body, "suffix").String() != "final" ||
				gjson.GetBytes(body, "logprobs").Int() != 5 || gjson.GetBytes(body, "model").String() != "qwen3.6" ||
				gjson.GetBytes(body, "max_tokens").Int() != 19 || gjson.GetBytes(body, "messages").Exists() ||
				gjson.GetBytes(body, "custom").Exists() || gjson.GetBytes(body, "stream_options").Exists() {
				t.Fatalf("native completion fields or final payload rules changed: %s", body)
			}
			if record := capture.await(t); record.Failed || record.TokensReported == nil || !*record.TokensReported || record.Detail.InputTokens != 5 || record.Detail.OutputTokens != 3 {
				t.Fatalf("native completion usage lost: %+v", record)
			}
		})
	}
}

func TestOpenAICompatNativeResponsesPreservesPayloadAndFinalRules(t *testing.T) {
	var path string
	var body []byte
	response := `{"id":"resp_native","object":"response","output":[],"custom_response":true,"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, response)
	}))
	defer server.Close()
	executor, auth, req, opts := nativeResponsesFixture(server.URL)
	executor.cfg.Payload = config.PayloadConfig{
		Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "qwen3.6"}}, Params: map[string]any{"instructions": "final rule", "max_output_tokens": 19}}},
		Filter:   []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "qwen3.6"}}, Params: []string{"custom"}}},
	}
	ctx, capture := nativeUsageCapture(t)
	result, err := executor.Execute(ctx, auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/v1/responses" || gjson.GetBytes(body, "model").String() != "qwen3.6" {
		t.Fatalf("wrong native target: %s %s", path, body)
	}
	if gjson.GetBytes(body, "instructions").String() != "final rule" || gjson.GetBytes(body, "max_output_tokens").Int() != 19 || gjson.GetBytes(body, "custom").Exists() {
		t.Fatalf("payload rules were not final: %s", body)
	}
	if gjson.GetBytes(body, "messages").Exists() || !gjson.GetBytes(body, "input").Exists() || string(result.Payload) != response {
		t.Fatalf("native protocol changed: %s", result.Payload)
	}
	if record := capture.await(t); record.Failed || record.TokensReported == nil || !*record.TokensReported || record.Detail.InputTokens != 5 || record.Detail.OutputTokens != 3 {
		t.Fatalf("native usage lost: %+v", record)
	}
}

func TestOpenAICompatNativeResponsesSSEPreservesUnknownEvents(t *testing.T) {
	frames := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n" +
		"event: provider.custom\ndata: {\"type\":\"provider.custom\",\"extension\":true}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"usage\":{\"input_tokens\":5,\"output_tokens\":3}}}\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("wrong path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, frames)
	}))
	defer server.Close()
	executor, auth, req, opts := nativeResponsesFixture(server.URL)
	ctx, capture := nativeUsageCapture(t)
	stream, err := executor.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		output.Write(chunk.Payload)
	}
	if output.String() != frames {
		t.Fatalf("native events changed: %s", output.String())
	}
	if record := capture.await(t); record.Failed || record.TokensReported == nil || !*record.TokensReported || record.Detail.InputTokens != 5 || record.Detail.OutputTokens != 3 {
		t.Fatalf("native stream usage lost: %+v", record)
	}
}

func TestOpenAICompatNativeResponsesLateFailureIsNotSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"status_code\":429,\"message\":\"fixture quota\"}}}\n\n")
	}))
	defer server.Close()
	executor, auth, req, opts := nativeResponsesFixture(server.URL)
	ctx, capture := nativeUsageCapture(t)
	stream, err := executor.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	var terminal error
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			terminal = chunk.Err
		}
	}
	var status interface{ StatusCode() int }
	if !errors.As(terminal, &status) || status.StatusCode() != 429 {
		t.Fatalf("late failure lost: %v", terminal)
	}
	if record := capture.await(t); !record.Failed {
		t.Fatal("native failure reported as success")
	}
}

func TestOpenAICompatNativeResponsesCancellationClosesUpstream(t *testing.T) {
	closed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(closed)
	}))
	defer server.Close()
	executor, auth, req, opts := nativeResponsesFixture(server.URL)
	usageCtx, capture := nativeUsageCapture(t)
	ctx, cancel := context.WithCancel(usageCtx)
	defer cancel()
	stream, err := executor.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	<-stream.Chunks
	cancel()
	for range stream.Chunks {
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("native upstream did not observe cancellation")
	}
	if record := capture.await(t); !record.Failed {
		t.Fatal("native cancellation not reported as failure")
	}
}
