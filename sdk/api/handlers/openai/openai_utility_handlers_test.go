package openai

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestUtilityHandlerRejectsStreamAndNonStringModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewOpenAIAPIHandler(&handlers.BaseAPIHandler{Cfg: &sdkconfig.SDKConfig{}})
	for _, body := range []string{`{"model":1,"input":"a"}`, `{"model":"m","input":"a","stream":true}`} {
		response := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(response)
		context.Request = httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body))
		handler.Embeddings(context)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("body %s status=%d", body, response.Code)
		}
	}
}

func TestUtilityHandlerReportsConfiguredBodyLimitAsTooLarge(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewOpenAIAPIHandler(&handlers.BaseAPIHandler{Cfg: &sdkconfig.SDKConfig{}})
	const limit = 16
	body := `{"model":"m","query":"q","documents":["a"]}`
	for _, route := range []struct {
		path string
		call func(*gin.Context)
	}{
		{path: "/v1/embeddings", call: handler.Embeddings},
		{path: "/v1/rerank", call: handler.Rerank},
	} {
		response := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(response)
		request := httptest.NewRequest(http.MethodPost, route.path, strings.NewReader(body))
		request.ContentLength = -1
		request.Body = http.MaxBytesReader(response, request.Body, limit)
		context.Request = request
		context.Set(handlers.MaxRequestBodyBytesContextKey, int64(limit))
		route.call(context)
		if response.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("%s status=%d body=%s", route.path, response.Code, response.Body.String())
		}
	}
}
