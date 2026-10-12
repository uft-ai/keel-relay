package executor

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestOpenAICompatAudioKeepsBinaryAndMultipartSemantics(t *testing.T) {
	audio := []byte{'R', 'I', 'F', 'F', 0, 1, 255, 2, 'W', 'A', 'V', 'E'}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-key" {
			t.Error("audio credential was lost")
		}
		switch r.URL.Path {
		case "/v1/audio/speech":
			body, _ := io.ReadAll(r.Body)
			if gjson.GetBytes(body, "model").String() != "kokoro" || gjson.GetBytes(body, "voice").String() != "final-voice" || gjson.GetBytes(body, "drop_me").Exists() {
				t.Errorf("speech final payload changed: %s", body)
			}
			w.Header().Set("Content-Type", "audio/wav")
			_, _ = w.Write(audio)
		case "/v1/audio/transcriptions":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
				return
			}
			defer r.MultipartForm.RemoveAll()
			if r.FormValue("model") != "whisper" || r.FormValue("temperature") != "0.7" {
				t.Error("multipart model or final rule lost")
			}
			values := r.MultipartForm.Value["timestamp_granularities[]"]
			if len(values) != 2 || values[0] != "word" || values[1] != "segment" {
				t.Errorf("repeated options changed: %v", values)
			}
			file, _, err := r.FormFile("file")
			if err != nil {
				t.Error(err)
				return
			}
			got, _ := io.ReadAll(file)
			_ = file.Close()
			if !bytes.Equal(got, audio) {
				t.Error("audio upload bytes changed")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"text":"fixture transcription","extension":true}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	e := NewOpenAICompatExecutor("openai-compatible-nan", &config.Config{Payload: config.PayloadConfig{
		Override: []config.PayloadRule{
			{Models: []config.PayloadModelRule{{Name: "kokoro", Protocol: "openai"}}, Params: map[string]any{"voice": "final-voice"}},
			{Models: []config.PayloadModelRule{{Name: "whisper", Protocol: "openai"}}, Params: map[string]any{"temperature": 0.7}},
		},
		Filter: []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "kokoro"}}, Params: []string{"drop_me"}}},
	}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL + "/v1", "api_key": "fixture-key"}}
	t.Run("speech", func(t *testing.T) {
		ctx, capture := nativeUsageCapture(t)
		response, err := e.Execute(ctx, auth, cliproxyexecutor.Request{Model: "kokoro", Payload: []byte(`{"model":"nan/kokoro","input":"hello","voice":"af_heart","response_format":"wav","drop_me":true}`)}, cliproxyexecutor.Options{SourceFormat: "openai-compatible-speech"})
		if err != nil || !bytes.Equal(response.Payload, audio) || response.Headers.Get("Content-Type") != "audio/wav" {
			t.Fatalf("speech bytes/MIME: %v %+v", err, response.Headers)
		}
		if record := capture.await(t); record.TokensReported == nil || *record.TokensReported || record.Failed {
			t.Fatalf("speech missing-token report: %+v", record)
		}
	})
	t.Run("transcription", func(t *testing.T) {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		_ = writer.WriteField("model", "nan/whisper")
		_ = writer.WriteField("timestamp_granularities[]", "word")
		_ = writer.WriteField("timestamp_granularities[]", "segment")
		part, _ := writer.CreateFormFile("file", "fixture.wav")
		_, _ = part.Write(audio)
		_ = writer.Close()
		ctx, capture := nativeUsageCapture(t)
		response, err := e.Execute(ctx, auth, cliproxyexecutor.Request{Model: "whisper", Payload: body.Bytes()}, cliproxyexecutor.Options{SourceFormat: "openai-transcription", Headers: http.Header{"Content-Type": []string{writer.FormDataContentType()}}})
		if err != nil || !gjson.GetBytes(response.Payload, "extension").Bool() {
			t.Fatalf("transcription response: %s, %v", response.Payload, err)
		}
		if record := capture.await(t); record.TokensReported == nil || *record.TokensReported || record.Failed {
			t.Fatalf("transcription missing-token report: %+v", record)
		}
	})
}
