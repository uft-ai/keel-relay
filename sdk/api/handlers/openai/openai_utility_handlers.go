package openai

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/tidwall/gjson"
)

func (h *OpenAIAPIHandler) Embeddings(c *gin.Context) { h.utility(c, "embeddings") }
func (h *OpenAIAPIHandler) Rerank(c *gin.Context)     { h.utility(c, "rerank") }

func (h *OpenAIAPIHandler) utility(c *gin.Context, operation string) {
	raw, err := handlers.ReadRequestBody(c)
	if err != nil {
		c.JSON(clienterror.HTTPStatusFromErrorOr(err, http.StatusBadRequest), handlers.ErrorResponse{Error: handlers.ErrorDetail{Message: fmt.Sprintf("Invalid request: %v", err), Type: "invalid_request_error"}})
		return
	}
	model := gjson.GetBytes(raw, "model")
	if !gjson.ValidBytes(raw) || model.Type != gjson.String || strings.TrimSpace(model.String()) == "" || gjson.GetBytes(raw, "stream").Bool() {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{Error: handlers.ErrorDetail{Message: "model is required", Type: "invalid_request_error"}})
		return
	}
	if operation == "rerank" && (!gjson.GetBytes(raw, "query").Exists() || !gjson.GetBytes(raw, "documents").IsArray()) {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{Error: handlers.ErrorDetail{Message: "query and documents are required", Type: "invalid_request_error"}})
		return
	}
	if operation == "embeddings" && !gjson.GetBytes(raw, "input").Exists() {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{Error: handlers.ErrorDetail{Message: "input is required", Type: "invalid_request_error"}})
		return
	}
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	defer cliCancel(nil)
	payload, headers, errMsg := h.ExecuteWithAuthManager(cliCtx, "openai-"+operation, gjson.GetBytes(raw, "model").String(), raw, operation)
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), headers)
	c.Data(http.StatusOK, "application/json", payload)
}
