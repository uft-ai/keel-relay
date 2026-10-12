package middleware

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

type logDirCounts struct {
	captures, errors, spools int
}

func countLogDir(t *testing.T, dir string) logDirCounts {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read logs dir: %v", err)
	}
	var counts logDirCounts
	for _, entry := range entries {
		name := entry.Name()
		switch {
		case strings.HasSuffix(name, ".tmp"):
			counts.spools++
		case strings.HasPrefix(name, "error-") && strings.HasSuffix(name, ".log"):
			counts.errors++
		case strings.HasSuffix(name, ".log"):
			counts.captures++
		}
	}
	return counts
}

func toggleRouter(logger *logging.FileRequestLogger) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequestLoggingMiddleware(logger))
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		switch c.Query("case") {
		case "fail":
			c.JSON(http.StatusInternalServerError, gin.H{"error": "upstream failed"})
		case "stream-off":
			c.Header("Content-Type", "text/event-stream")
			c.Status(http.StatusOK)
			_, _ = c.Writer.Write([]byte("data: first\n\n"))
			logger.SetEnabled(false)
			_, _ = c.Writer.Write([]byte("data: second\n\n"))
		default:
			c.JSON(http.StatusOK, gin.H{"ok": true})
		}
	})
	return router
}

func serveToggle(t *testing.T, router *gin.Engine, query string) int {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?case="+query, strings.NewReader(`{"model":"m"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response.Code
}

func TestRequestLoggingOffKeepsOnlyBoundedErrorLogs(t *testing.T) {
	logsDir := t.TempDir()
	logger := logging.NewFileRequestLogger(false, logsDir, "", 2)
	router := toggleRouter(logger)

	if code := serveToggle(t, router, "ok"); code != http.StatusOK {
		t.Fatalf("success status = %d", code)
	}
	if got := countLogDir(t, logsDir); got != (logDirCounts{}) {
		t.Fatalf("capture off stored a successful request: %+v", got)
	}

	for range 3 {
		if code := serveToggle(t, router, "fail"); code != http.StatusInternalServerError {
			t.Fatalf("failure status = %d", code)
		}
	}
	if got := countLogDir(t, logsDir); got != (logDirCounts{errors: 2}) {
		t.Fatalf("capture off kept %+v, want two error logs only", got)
	}
}

func TestRequestLoggingTurnedOffMidStreamFinishesItsLog(t *testing.T) {
	logsDir := t.TempDir()
	logger := logging.NewFileRequestLogger(true, logsDir, "", 10)
	router := toggleRouter(logger)

	if code := serveToggle(t, router, "stream-off"); code != http.StatusOK {
		t.Fatalf("stream status = %d", code)
	}
	if got := countLogDir(t, logsDir); got != (logDirCounts{captures: 1}) {
		t.Fatalf("stream started with capture on left %+v, want one finished log and no spools", got)
	}

	if code := serveToggle(t, router, "ok"); code != http.StatusOK {
		t.Fatalf("success status = %d", code)
	}
	if got := countLogDir(t, logsDir); got != (logDirCounts{captures: 1}) {
		t.Fatalf("capture off stored a later request: %+v", got)
	}

	logger.SetEnabled(true)
	if code := serveToggle(t, router, "ok"); code != http.StatusOK {
		t.Fatalf("success status = %d", code)
	}
	if got := countLogDir(t, logsDir); got != (logDirCounts{captures: 2}) {
		t.Fatalf("capture on again kept %+v, want the next request captured", got)
	}
}

func TestRequestLoggingToggleDuringRequestsLeavesNoSpools(t *testing.T) {
	logsDir := t.TempDir()
	logger := logging.NewFileRequestLogger(true, logsDir, "", 0)
	router := toggleRouter(logger)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for enabled := false; ; enabled = !enabled {
			select {
			case <-stop:
				return
			default:
				logger.SetEnabled(enabled)
				logger.SetErrorLogsMaxFiles(map[bool]int{true: 5, false: 0}[enabled])
			}
		}
	}()
	var requests sync.WaitGroup
	for i := range 40 {
		requests.Add(1)
		go func() {
			defer requests.Done()
			serveToggle(t, router, []string{"ok", "fail", "stream-off"}[i%3])
		}()
	}
	requests.Wait()
	close(stop)
	wg.Wait()

	if got := countLogDir(t, logsDir); got.spools != 0 {
		t.Fatalf("toggling left %d spool files", got.spools)
	}
}
