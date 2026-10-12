package openai

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func (h *OpenAIAPIHandler) Usage(c *gin.Context) {
	index := strings.TrimSpace(c.Query("auth_index"))
	var eligible []*coreauth.Auth
	for _, auth := range h.AuthManager.List() {
		if auth.Disabled || auth.Status == coreauth.StatusDisabled || !coreauth.IsNanCompatibleAuth(auth) {
			continue
		}
		if index == "" || auth.Index == index {
			eligible = append(eligible, auth)
		}
	}
	if len(eligible) == 0 {
		code := "usage_not_configured"
		if index != "" {
			code = "credential_not_found"
		}
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": code, "message": "No eligible usage credential"}})
		return
	}
	if len(eligible) != 1 || len(c.Request.URL.Query()["auth_index"]) > 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "credential_required", "message": "Select one enabled NaN credential with auth_index"}})
		return
	}
	auth := eligible[0]
	executor, ok := h.AuthManager.Executor(auth.Provider)
	if !ok {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "usage_not_configured"}})
		return
	}
	reader, ok := executor.(interface {
		ReadUsage(context.Context, *coreauth.Auth, string) (coreexecutor.Response, error)
	})
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "unsupported_operation"}})
		return
	}
	parts := strings.Split(c.Request.URL.RawQuery, "&")
	kept := parts[:0]
	for _, part := range parts {
		name, _, _ := strings.Cut(part, "=")
		name, err := url.QueryUnescape(name)
		if err != nil || name != "auth_index" {
			kept = append(kept, part)
		}
	}
	ctx, cancel := h.GetContextWithCancel(h, c, context.Background())
	defer cancel(nil)
	logging.SetGinCPATraceID(c, auth.EnsureIndex())
	result, err := reader.ReadUsage(ctx, auth, strings.Join(kept, "&"))
	if err != nil {
		cancel(err)
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"code": "usage_upstream_error", "message": "Provider usage request failed"}})
		return
	}
	if retry := result.Headers.Get("Retry-After"); retry != "" {
		c.Header("Retry-After", retry)
	}
	contentType := result.Headers.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	c.Data(result.StatusCode, contentType, result.Payload)
}
