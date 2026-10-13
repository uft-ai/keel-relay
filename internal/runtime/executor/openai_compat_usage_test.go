package executor

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestOpenAICompatUsageHistoryIsNotInference(t *testing.T) {
	for _, status := range []int{200, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			const query = "start_date=2026-10-01&end_date=2026-10-02&limit=1"
			const body = `{"data":[],"totals":{"total_tokens":999},"all_time":{"total_tokens":999}}`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/usage" || r.URL.RawQuery != query || r.Header.Get("Authorization") != "Bearer fixture-key" {
					t.Error("history request changed")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "Wed, 14 Oct 2026 00:00:00 GMT")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			e := NewOpenAICompatExecutor("openai-compatible-nan", &config.Config{})
			auth := &cliproxyauth.Auth{Provider: e.Identifier(), Attributes: map[string]string{"api_key": "fixture-key", "base_url": server.URL + "/v1", "compat_name": "history"}}
			ctx, capture := nativeUsageCapture(t)
			response, err := e.ReadUsage(ctx, auth, query)
			if err != nil || response.StatusCode != status || string(response.Payload) != body || response.Headers.Get("Retry-After") != "Wed, 14 Oct 2026 00:00:00 GMT" {
				t.Fatalf("history response changed: %v %+v", err, response)
			}
			record := capture.await(t)
			if usage.GenerateEnabled(record.Generate) || record.TokensReported == nil || *record.TokensReported || record.Detail.TotalTokens != 0 || record.Failed != (status == 429) {
				t.Fatalf("history counted as inference: %+v", record)
			}
		})
	}
}
