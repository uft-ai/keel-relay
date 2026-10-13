package executor

import (
	"bytes"
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestNanImagesRejectMaskAndStreamingBeforeUpstream(t *testing.T) {
	var hits atomic.Int32
	// NaN rules follow the official host. The context transport answers for it, so no request leaves the test.
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", roundTripperFunc(func(*http.Request) (*http.Response, error) {
		hits.Add(1)
		return nil, errors.New("unexpected upstream request")
	}))
	e := NewOpenAICompatExecutor("openai-compatible-renamed", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://api.nan.builders/v1", "compat_name": "renamed"}}
	opts := cliproxyexecutor.Options{SourceFormat: "openai-image", Metadata: map[string]any{cliproxyexecutor.RequestPathMetadataKey: "/v1/images/edits"}}
	req := cliproxyexecutor.Request{Model: "flux-2-klein", Payload: []byte(`{"model":"flux-2-klein","prompt":"edit","mask":"unsupported"}`)}
	_, err := e.Execute(ctx, auth, req, opts)
	if err == nil || statusCodeForTest(err) != 400 {
		t.Fatalf("mask error=%v", err)
	}
	_, err = e.ExecuteStream(ctx, auth, req, opts)
	if err == nil || statusCodeForTest(err) != 400 {
		t.Fatalf("stream error=%v", err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("model", "flux-2-klein")
	_ = writer.WriteField("prompt", "edit")
	part, _ := writer.CreateFormFile("mask", "mask.png")
	_, _ = part.Write([]byte("fixture"))
	_ = writer.Close()
	req.Payload = body.Bytes()
	opts.Headers = http.Header{"Content-Type": []string{writer.FormDataContentType()}}
	_, err = e.Execute(t.Context(), auth, req, opts)
	if err == nil || statusCodeForTest(err) != 400 || hits.Load() != 0 {
		t.Fatalf("multipart mask or attempt fence: err=%v hits=%d", err, hits.Load())
	}
}

// An entry named nan on another host is a generic provider: NaN's image limits do not apply.
func TestNanNamedEntryOnOtherHostGetsGenericImages(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"AA=="}]}`))
	}))
	defer server.Close()
	e := NewOpenAICompatExecutor("openai-compatible-nan", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL, "compat_name": "nan"}}
	opts := cliproxyexecutor.Options{SourceFormat: "openai-image", Metadata: map[string]any{cliproxyexecutor.RequestPathMetadataKey: "/v1/images/edits"}}
	req := cliproxyexecutor.Request{Model: "flux-2-klein", Payload: []byte(`{"model":"flux-2-klein","prompt":"edit","mask":"generic"}`)}
	if _, err := e.Execute(t.Context(), auth, req, opts); err != nil || hits.Load() != 1 {
		t.Fatalf("generic image edit: err=%v hits=%d", err, hits.Load())
	}
}

func statusCodeForTest(err error) int {
	if value, ok := err.(interface{ StatusCode() int }); ok {
		return value.StatusCode()
	}
	return 0
}
