package openai

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestConfiguredSpeechHandlerPreservesProviderBytesAndMIME(t *testing.T) {
	gin.SetMode(gin.TestMode)
	want := []byte{'R', 'I', 'F', 'F', 0, 1, 255, 2, 'W', 'A', 'V', 'E'}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/audio/speech" || gjson.GetBytes(body, "model").String() != "kokoro" || gjson.GetBytes(body, "voice").String() != "af_heart" {
			t.Errorf("wrong speech payload %s %s", r.URL.Path, body)
		}
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(want)
	}))
	defer upstream.Close()
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{Name: "nan", Prefix: "nan", BaseURL: upstream.URL + "/v1", APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "fixture"}}, Models: []config.OpenAICompatibilityModel{{Name: "kokoro", Alias: "kokoro", Operations: []string{"speech"}}}}}}
	m := coreauth.NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	e := runtimeexecutor.NewOpenAICompatExecutor("openai-compatible-nan", cfg)
	m.RegisterExecutor(e)
	auth := &coreauth.Auth{ID: t.Name(), Provider: e.Identifier(), Prefix: "nan", Attributes: map[string]string{"api_key": "fixture", "base_url": upstream.URL + "/v1", "compat_name": "nan", "provider_key": "nan", coreauth.AttributeAuthKind: coreauth.AuthKindAPIKey, coreauth.AttributeSource: "config:nan"}}
	if _, err := m.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "kokoro", Operations: []string{"speech"}}, {ID: "nan/kokoro", Operations: []string{"speech"}}})
	defer registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	m.RefreshSchedulerEntry(auth.ID)
	h := NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, m))
	response := performSpeechRoutingRequest(t, "/v1/audio/speech", `{"model":"nan/kokoro","input":"hello","voice":"af_heart","response_format":"wav"}`, h.AudioSpeech)
	if response.Code != 200 || !bytes.Equal(response.Body.Bytes(), want) || response.Header().Get("Content-Type") != "audio/wav" {
		t.Fatalf("audio response=%d MIME=%s bytes=%v", response.Code, response.Header().Get("Content-Type"), response.Body.Bytes())
	}
	// xAI's 1 MB speech cap does not apply to a compatible speech model.
	large := `{"model":"nan/kokoro","input":"` + strings.Repeat("a", maxXAISpeechBody) + `","voice":"af_heart"}`
	if response := performSpeechRoutingRequest(t, "/v1/audio/speech", large, h.AudioSpeech); response.Code != 200 {
		t.Fatalf("large compatible speech status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestTranscriptionHandlerValidatesMultipartBeforeExecution(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name           string
		duplicateModel bool
		stream         bool
	}{{name: "missing file"}, {name: "duplicate model", duplicateModel: true}, {name: "streaming", stream: true}} {
		t.Run(test.name, func(t *testing.T) {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			_ = writer.WriteField("model", "nan/whisper")
			if test.duplicateModel {
				_ = writer.WriteField("model", "nan/other")
			}
			if test.stream {
				_ = writer.WriteField("stream", "true")
			}
			if test.stream || test.duplicateModel {
				part, _ := writer.CreateFormFile("file", "test.wav")
				_, _ = part.Write([]byte("fixture"))
			}
			_ = writer.Close()
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
			ctx.Request.Header.Set("Content-Type", writer.FormDataContentType())
			(&OpenAIAPIHandler{}).AudioTranscriptions(ctx)
			if recorder.Code != 400 {
				t.Fatalf("invalid multipart status=%d", recorder.Code)
			}
		})
	}
}

func TestTranscriptionHandlerEnforcesTheGatewayRequestLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const limit = 64
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(bytes.Repeat([]byte("x"), limit+1)))
	request.ContentLength = -1
	request.Body = http.MaxBytesReader(recorder, request.Body, limit)
	request.Header.Set("Content-Type", "multipart/form-data; boundary=fixture")
	ctx.Request = request
	ctx.Set(handlers.MaxRequestBodyBytesContextKey, int64(limit))
	(&OpenAIAPIHandler{}).AudioTranscriptions(ctx)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload status=%d", recorder.Code)
	}
}

type failingTranscriptionBody struct{}

func (failingTranscriptionBody) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestTranscriptionHandlerReportsUnreadableBodyAsBadRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", failingTranscriptionBody{})
	ctx.Request.Header.Set("Content-Type", "multipart/form-data; boundary=fixture")
	(&OpenAIAPIHandler{}).AudioTranscriptions(ctx)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unreadable upload status=%d", recorder.Code)
	}
}
