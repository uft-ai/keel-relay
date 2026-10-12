package executor

import (
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

// ReadUsage is credential-scoped history. It never enters model selection, cooldowns or inference retries.
func (e *OpenAICompatExecutor) ReadUsage(ctx context.Context, auth *cliproxyauth.Auth, query string) (result cliproxyexecutor.Response, err error) {
	ctx = usage.WithGenerate(ctx, false)
	reporter := helps.NewExecutorUsageReporter(ctx, e, "", auth)
	reporter.TrackTokenPresence()
	defer reporter.TrackFailure(ctx, &err)
	baseURL, _ := e.resolveCredentials(auth)
	if baseURL == "" {
		return result, statusErr{code: http.StatusServiceUnavailable, msg: "usage provider is not configured"}
	}
	target := strings.TrimSuffix(baseURL, "/") + "/usage"
	if query != "" {
		target += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return result, err
	}
	if err = e.PrepareRequest(req, auth); err != nil {
		return result, err
	}
	req.Header.Set("Accept", "application/json")
	authType, authValue := auth.AccountInfo()
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{URL: target, Method: http.MethodGet, Headers: req.Header.Clone(), Provider: e.Identifier(), AuthID: auth.ID, AuthLabel: auth.Label, AuthType: authType, AuthValue: authValue})
	client := reporter.TrackHTTPClient(helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0))
	response, err := client.Do(req)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return result, err
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			log.WithError(err).Warn("close provider usage response")
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, response.StatusCode, response.Header.Clone())
	body, err := io.ReadAll(response.Body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return result, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, body)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		reporter.PublishFailure(ctx, statusErr{code: response.StatusCode, msg: string(body)})
	} else {
		reporter.EnsurePublished(ctx)
	}
	return cliproxyexecutor.Response{StatusCode: response.StatusCode, Payload: body, Headers: response.Header.Clone()}, nil
}
