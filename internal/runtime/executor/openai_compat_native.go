package executor

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// openAICompatNativeOperation reports whether the model entry bound to this attempt
// lists op in its native operations. Lookup uses the per-attempt entry selected by the
// client alias, never a scan by upstream model name.
func openAICompatNativeOperation(req cliproxyexecutor.Request, op string) bool {
	_, native, ok := cliproxyauth.ResolvedAPIKeyModelOperations(req)
	if !ok {
		return false
	}
	for _, candidate := range native {
		if strings.EqualFold(strings.TrimSpace(candidate), op) {
			return true
		}
	}
	return false
}

// openAICompatNativeResponsesRequested reports whether a Responses request should be
// forwarded to the upstream /responses endpoint without chat translation.
func openAICompatNativeResponsesRequested(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) bool {
	if opts.Alt != "" {
		return false
	}
	if opts.SourceFormat != sdktranslator.FormatOpenAIResponse {
		return false
	}
	if cliproxyexecutor.ResponseFormatOrSource(opts) != sdktranslator.FormatOpenAIResponse {
		return false
	}
	return openAICompatNativeOperation(req, config.OpenAICompatOperationResponses)
}

func openAICompatNativeCompletionsRequested(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) bool {
	return opts.Alt == "" && opts.SourceFormat == sdktranslator.FormatOpenAI &&
		cliproxyexecutor.ResponseFormatOrSource(opts) == sdktranslator.FormatOpenAI &&
		cliproxyauth.RequestedModelOperation(opts) == config.OpenAICompatOperationCompletions &&
		openAICompatNativeOperation(req, config.OpenAICompatOperationCompletions)
}

// prepareNativeRequest builds the upstream body for a native call.
// Only the model name, thinking settings, stream flag and payload config rules change it;
// chat-specific normalizers are not applied.
func (e *OpenAICompatExecutor) prepareNativeRequest(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, baseModel string, stream bool) ([]byte, error) {
	from := opts.SourceFormat
	to := sdktranslator.FormatOpenAIResponse
	completion := openAICompatNativeCompletionsRequested(req, opts)
	if completion {
		to = sdktranslator.FormatOpenAI
	}
	originalPayload := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayload = opts.OriginalRequest
	}
	isCompat := helps.APIKeyModelIsCompat(req)
	originalTranslated, translated, updatesChanged, err := helps.TranslateRequestPairWithAPIKeyModelCompatibilityAndUpdateIntent(ctx, opts.Headers, e.cfg, from, to, baseModel, originalPayload, req.Payload, stream, isCompat)
	if err != nil {
		return nil, err
	}
	if !completion {
		translated, err = helps.ApplyRequestThinking(translated, req, opts, from.String(), to.String(), e.Identifier(), updatesChanged)
		if err != nil {
			return nil, err
		}
	}
	if stream {
		translated = helps.SetBoolIfDifferent(translated, "stream", true)
		if completion {
			translated = helps.SetBoolIfDifferent(translated, "stream_options.include_usage", true)
		}
	} else if gjson.GetBytes(translated, "stream").Bool() {
		if updated, errSet := sjson.SetBytes(translated, "stream", false); errSet == nil {
			translated = updated
		}
	}
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	translated = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", translated, originalTranslated, requestedModel, requestPath, opts.Headers)
	return translated, nil
}

func (e *OpenAICompatExecutor) newNativeRequest(ctx context.Context, auth *cliproxyauth.Auth, opts cliproxyexecutor.Options, url, apiKey string, body []byte, stream bool) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpReq.Header.Set("User-Agent", "cli-proxy-openai-compat")
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs, opts.Headers)
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
		httpReq.Header.Set("Cache-Control", "no-cache")
	}
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})
	return httpReq, nil
}

func (e *OpenAICompatExecutor) executeNative(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return resp, err
	}
	body, err := e.prepareNativeRequest(ctx, req, opts, baseModel, false)
	if err != nil {
		return resp, err
	}
	reporter.SetTranslatedReasoningEffort(body, sdktranslator.FormatOpenAIResponse.String())
	path := "/responses"
	if openAICompatNativeCompletionsRequested(req, opts) {
		path = "/completions"
	}
	url := strings.TrimSuffix(baseURL, "/") + path
	httpReq, err := e.newNativeRequest(ctx, auth, opts, url, apiKey, body, false)
	if err != nil {
		return resp, err
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("openai compat executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		err = newOpenAICompatStatusError(httpResp.StatusCode, httpResp.Header, b)
		return resp, err
	}
	out, err := io.ReadAll(httpResp.Body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, out)
	reporter.ObserveResponseModel(out)
	reporter.Publish(ctx, helps.ParseOpenAIUsage(out))
	return cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}, nil
}

func (e *OpenAICompatExecutor) executeNativeStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return nil, err
	}
	body, err := e.prepareNativeRequest(ctx, req, opts, baseModel, true)
	if err != nil {
		return nil, err
	}
	reporter.SetTranslatedReasoningEffort(body, sdktranslator.FormatOpenAIResponse.String())
	completion := openAICompatNativeCompletionsRequested(req, opts)
	path := "/responses"
	if completion {
		path = "/completions"
	}
	url := strings.TrimSuffix(baseURL, "/") + path
	httpReq, err := e.newNativeRequest(ctx, auth, opts, url, apiKey, body, true)
	if err != nil {
		return nil, err
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClientRoundTripOnly(httpClient)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("openai compat executor: close response body error: %v", errClose)
		}
		err = newOpenAICompatStatusError(httpResp.StatusCode, httpResp.Header, b)
		return nil, err
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		var streamUsage helps.StreamUsageBuffer
		defer func() {
			if ctx.Err() != nil {
				streamUsage.PublishFailure(ctx, reporter, ctx.Err())
			} else {
				streamUsage.Publish(ctx, reporter)
				reporter.EnsurePublished(ctx)
			}
		}()
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("openai compat executor: close response body error: %v", errClose)
			}
		}()
		emitted := 0
		sendErr := func(streamErr error) {
			helps.RecordAPIResponseError(ctx, e.cfg, streamErr)
			streamUsage.PublishFailure(ctx, reporter, streamErr)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: streamErr}:
			case <-ctx.Done():
			}
		}
		var frame [][]byte
		var eventName string
		var data []byte
		frameBytes := 0
		// Keep native event fields; errors still enter the shared failure/accounting path.
		flush := func() bool {
			lines, name, payload := frame, eventName, data
			frame, eventName, data = nil, "", nil
			frameBytes = 0
			if len(lines) == 0 {
				return true
			}
			if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
				if completion {
					if emitted == 0 {
						sendErr(statusErr{code: http.StatusBadGateway, msg: "upstream stream closed before first payload"})
					}
					return false
				}
				return true
			}
			eventType := name
			if eventType == "" {
				eventType = gjson.GetBytes(payload, "type").String()
			}
			if streamErr, isErr := openAICompatStreamDataError(payload, eventType); isErr {
				sendErr(streamErr)
				return false
			}
			if completion {
				if len(payload) == 0 {
					return true
				}
				if !gjson.ValidBytes(payload) {
					sendErr(statusErr{code: http.StatusBadGateway, msg: "upstream completion stream contains invalid JSON"})
					return false
				}
				streamUsage.ObserveOpenAIStream(payload)
				hasText := false
				for _, choice := range gjson.GetBytes(payload, "choices").Array() {
					hasText = hasText || choice.Get("text").String() != ""
				}
				reporter.ObserveTokenEvent(hasText)
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: bytes.Clone(payload)}:
					emitted++
					return true
				case <-ctx.Done():
					return false
				}
			}
			helps.ObserveResponsesTokenEvent(reporter, payload)
			terminal := false
			switch eventType {
			case "response.completed", "response.incomplete", "response.done", "response.failed":
				terminal = true
				if detail, ok := helps.ParseCodexUsage(payload); ok {
					reporter.Publish(ctx, detail)
				} else {
					reporter.EnsurePublished(ctx)
				}
			}
			chunk := append(bytes.Join(lines, []byte("\n")), '\n', '\n')
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: chunk}:
				emitted++
			case <-ctx.Done():
				return false
			}
			return !terminal
		}

		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800) // 50MB
		for scanner.Scan() {
			line := bytes.TrimRight(scanner.Bytes(), "\r")
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			if len(line) == 0 {
				if !flush() {
					return
				}
				continue
			}
			if line[0] == ':' {
				continue
			}
			frameBytes += len(line) + 1
			if frameBytes > 52_428_800 {
				sendErr(statusErr{code: http.StatusBadGateway, msg: "upstream SSE frame exceeds 50 MiB"})
				return
			}
			lineCopy := bytes.Clone(line)
			switch {
			case bytes.HasPrefix(lineCopy, []byte("event:")):
				eventName = strings.TrimSpace(string(lineCopy[len("event:"):]))
			case bytes.HasPrefix(lineCopy, []byte("data:")):
				value := bytes.TrimPrefix(lineCopy[len("data:"):], []byte(" "))
				if len(data) > 0 {
					data = append(data, '\n')
				}
				data = append(data, value...)
			}
			frame = append(frame, lineCopy)
		}
		if errScan := scanner.Err(); errScan != nil {
			if ctx.Err() != nil {
				return
			}
			sendErr(statusErr{code: http.StatusBadGateway, msg: "upstream stream read failed: " + errScan.Error()})
			return
		}
		if !flush() {
			return
		}
		if emitted == 0 {
			sendErr(statusErr{code: http.StatusBadGateway, msg: "upstream stream closed before first payload"})
			return
		}
		sendErr(statusErr{code: http.StatusBadGateway, msg: "upstream stream closed before a terminal response event"})
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}
