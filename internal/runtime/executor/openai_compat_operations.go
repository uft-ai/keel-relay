package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func (e *OpenAICompatExecutor) ExecuteEmbeddings(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.executeUtility(ctx, auth, req, opts, "/embeddings", helps.ParseOpenAIUsage)
}

func (e *OpenAICompatExecutor) ExecuteRerank(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.executeUtility(ctx, auth, req, opts, "/rerank", parseRerankUsage)
}

func (e *OpenAICompatExecutor) ExecuteSpeech(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.executeUtility(ctx, auth, req, opts, "/audio/speech", helps.ParseOpenAIUsage)
}

func (e *OpenAICompatExecutor) ExecuteTranscription(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.executeUtility(ctx, auth, req, opts, "/audio/transcriptions", helps.ParseOpenAIUsage)
}

func (e *OpenAICompatExecutor) executeUtility(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, endpoint string, parse func([]byte) usage.Detail) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	reporter.TrackTokenPresence()
	defer reporter.TrackFailure(ctx, &err)
	baseURL, apiKey := e.resolveCredentials(auth)
	if baseURL == "" {
		return resp, statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
	}
	var payload []byte
	contentType := "application/json"
	if endpoint == "/audio/transcriptions" {
		payload, contentType, err = prepareOpenAICompatImagesPayload(req.Payload, baseModel, opts.Headers.Get("Content-Type"), false)
		if err == nil {
			payload, contentType, err = helps.ApplyMediaPayloadConfig(e.cfg, e.Identifier(), baseModel, "openai", payload, contentType, req, opts)
		}
	} else {
		payload, err = sjson.SetBytes(req.Payload, "model", baseModel)
		if err == nil {
			requestedModel := helps.PayloadRequestedModel(opts, req.Model)
			payload = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, "openai", opts.SourceFormat.String(), "", payload, req.Payload, requestedModel, helps.PayloadRequestPath(opts), opts.Headers)
		}
	}
	if err != nil {
		return resp, err
	}
	url := strings.TrimSuffix(baseURL, "/") + endpoint
	httpReq, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if errRequest != nil {
		return resp, errRequest
	}
	httpReq.Header.Set("Content-Type", contentType)
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs, opts.Headers)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID, authLabel, authType, authValue = auth.ID, auth.Label, "", ""
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{URL: url, Method: http.MethodPost, Headers: httpReq.Header.Clone(), Body: payload, Provider: e.Identifier(), AuthID: authID, AuthLabel: authLabel, AuthType: authType, AuthValue: authValue})
	httpClient := reporter.TrackHTTPClient(helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0))
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		return resp, errDo
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("openai compat utility executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	body, errRead := io.ReadAll(httpResp.Body)
	if errRead != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errRead)
		return resp, errRead
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, body)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return resp, newOpenAICompatStatusError(httpResp.StatusCode, httpResp.Header, body)
	}
	reporter.ObserveResponseModel(body)
	reporter.Publish(ctx, parse(body))
	return cliproxyexecutor.Response{Payload: body, Headers: httpResp.Header.Clone()}, nil
}

func parseRerankUsage(body []byte) usage.Detail {
	usageBody := []byte(`{}`)
	set := func(path, source, name string) {
		if value, ok := integerField(gjson.GetBytes(body, source), name); ok {
			usageBody, _ = sjson.SetBytes(usageBody, path, value)
		}
	}
	set("usage.prompt_tokens", "meta.tokens", "input_tokens")
	set("usage.completion_tokens", "meta.tokens", "output_tokens")
	set("usage.total_tokens", "meta.billed_units", "total_tokens")
	if !gjson.GetBytes(usageBody, "usage").Exists() {
		return helps.ParseOpenAIUsage(body)
	}
	return helps.ParseOpenAIUsage(usageBody)
}

func integerField(parent gjson.Result, name string) (int64, bool) {
	value := parent.Get(name)
	if value.Type != gjson.Number {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value.Raw, 10, 64)
	return parsed, err == nil && parsed >= 0
}
