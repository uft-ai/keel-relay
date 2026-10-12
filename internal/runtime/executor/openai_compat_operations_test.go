package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestOpenAICompatUtilityOperationsPreservePayloadAndUsage(t *testing.T) {
	var path, body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		if path == "/embeddings" {
			_, _ = w.Write([]byte(`{"object":"list","data":[{"embedding":[0.1,1]}],"usage":{"prompt_tokens":2,"total_tokens":2}}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"index":1,"relevance_score":0.9}],"meta":{"tokens":{"input_tokens":4,"output_tokens":1}}}`))
	}))
	defer server.Close()
	executor := NewOpenAICompatExecutor("openai-compatible-nan", &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "qwen3-embedding", Protocol: "openai"}}, Params: map[string]any{"dimensions": 4096}}}, Filter: []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "qwen3-embedding", Protocol: "openai"}}, Params: []string{"drop_me"}}}}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL, "api_key": "secret"}}
	response, err := executor.ExecuteEmbeddings(context.Background(), auth, cliproxyexecutor.Request{Model: "qwen3-embedding", Payload: []byte(`{"model":"alias","input":["a"],"encoding_format":"float","drop_me":true}`)}, cliproxyexecutor.Options{})
	if err != nil || path != "/embeddings" || !strings.Contains(body, `"model":"qwen3-embedding"`) || !strings.Contains(body, `"dimensions":4096`) || strings.Contains(body, "drop_me") || !strings.Contains(string(response.Payload), `[0.1,1]`) {
		t.Fatalf("embeddings path=%s body=%s response=%s err=%v", path, body, response.Payload, err)
	}
	response, err = executor.ExecuteRerank(context.Background(), auth, cliproxyexecutor.Request{Model: "qwen3-reranker", Payload: []byte(`{"model":"alias","query":"q","documents":["a","b"]}`)}, cliproxyexecutor.Options{})
	if err != nil || path != "/rerank" || !strings.Contains(string(response.Payload), `"index":1`) {
		t.Fatalf("rerank path=%s response=%s err=%v", path, response.Payload, err)
	}
	detail := parseRerankUsage([]byte(`{"meta":{"billed_units":{"total_tokens":7}}}`))
	if detail.TokensReported == nil || !*detail.TokensReported || detail.TotalTokens != 7 {
		t.Fatalf("billed usage=%+v", detail)
	}
	for _, sample := range []string{`{"meta":{"tokens":{"input_tokens":-1}}}`, `{"meta":{"tokens":{"input_tokens":9223372036854775808}}}`} {
		rejected := parseRerankUsage([]byte(sample))
		if rejected.TokensReported == nil || *rejected.TokensReported {
			t.Fatalf("invalid usage accepted: %s", sample)
		}
	}
	billedOnly := parseRerankUsage([]byte(`{"meta":{"billed_units":{"total_tokens":7}}}`))
	if billedOnly.InputTokens != 0 || billedOnly.OutputTokens != 0 || billedOnly.TotalTokens != 7 {
		t.Fatalf("billed-only buckets=%+v", billedOnly)
	}
	malformed := parseRerankUsage([]byte(`{"meta":{"tokens":{"input_tokens":null}}}`))
	if malformed.TokensReported == nil || *malformed.TokensReported {
		t.Fatalf("malformed usage reported=%v", malformed.TokensReported)
	}
}
