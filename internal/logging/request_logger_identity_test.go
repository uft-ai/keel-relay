package logging

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCapturedLogsKeepFullRequestIdentity(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
			dir := t.TempDir()
			logger := NewFileRequestLogger(true, dir, "", 0)
			const id = "01a12e2d-0000-7000-8000-12345678abcd"
			if stream {
				writer, err := logger.LogStreamingRequest("/v1/chat/completions", http.MethodPost, nil, []byte(`{"model":"qwen"}`), id)
				if err != nil {
					t.Fatal(err)
				}
				if err := writer.WriteStatus(200, nil); err != nil {
					t.Fatal(err)
				}
				writer.WriteChunkAsync([]byte("data: [DONE]\n\n"))
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
			} else if err := logger.LogRequest("/v1/chat/completions", http.MethodPost, nil, []byte(`{"model":"qwen"}`), 200, nil, []byte(`{"choices":[]}`), nil, nil, nil, nil, nil, id, time.Now(), time.Now()); err != nil {
				t.Fatal(err)
			}
			paths, err := filepath.Glob(filepath.Join(dir, "*.log"))
			if err != nil || len(paths) != 1 {
				t.Fatalf("capture files: %v, %v", paths, err)
			}
			data, err := os.ReadFile(paths[0])
			if err != nil {
				t.Fatal(err)
			}
			info, _, _ := strings.Cut(string(data), "=== HEADERS ===")
			if !strings.Contains(info, "\nRequest ID: "+id+"\n") {
				t.Fatal("trusted request info is missing the full request ID")
			}
		})
	}
}
