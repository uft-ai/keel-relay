package openai

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
)

func (h *OpenAIAPIHandler) AudioTranscriptions(c *gin.Context) {
	mediaType, params, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		writeSpeechError(c, http.StatusBadRequest, "transcription requires multipart/form-data with a boundary")
		return
	}
	if encoding := c.GetHeader("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		writeSpeechError(c, http.StatusUnsupportedMediaType, "compressed multipart uploads are not supported")
		return
	}
	// The gateway's max-request-body-bytes setting bounds this read. Each provider
	// adapter applies its own upload limit.
	raw, err := handlers.ReadRequestBody(c)
	if err != nil {
		status := clienterror.HTTPStatusFromErrorOr(err, http.StatusBadRequest)
		message := "transcription request could not be read"
		if status == http.StatusRequestEntityTooLarge {
			message = "transcription request exceeds the request size limit"
		}
		writeSpeechError(c, status, message)
		return
	}
	models, files, streams, err := transcriptionFields(raw, params["boundary"])
	if err != nil {
		writeSpeechError(c, http.StatusBadRequest, "invalid transcription multipart form")
		return
	}
	if len(models) != 1 || strings.TrimSpace(models[0]) == "" || len(files) != 1 || files[0] == 0 {
		writeSpeechError(c, http.StatusBadRequest, "transcription requires one model and one nonempty file")
		return
	}
	for _, stream := range streams {
		if stream != "" && stream != "false" {
			writeSpeechError(c, http.StatusBadRequest, "transcription does not support stream")
			return
		}
	}
	h.handleCompatibleAudio(c, raw, models[0], "openai-transcription")
}

// transcriptionFields reads the buffered form one part at a time, so validation
// makes no second copy of the uploaded file. It copies only the model and stream
// values and returns the size of each file part named "file".
func transcriptionFields(raw []byte, boundary string) (models []string, files []int64, streams []string, err error) {
	reader := multipart.NewReader(bytes.NewReader(raw), boundary)
	for {
		part, errPart := reader.NextPart()
		if errors.Is(errPart, io.EOF) {
			return models, files, streams, nil
		}
		if errPart != nil {
			return nil, nil, nil, errPart
		}
		name, isFile := part.FormName(), part.FileName() != ""
		switch {
		case isFile && name == "file":
			size, errCopy := io.Copy(io.Discard, part)
			if errCopy != nil {
				return nil, nil, nil, errCopy
			}
			files = append(files, size)
		case !isFile && (name == "model" || name == "stream"):
			value, errRead := io.ReadAll(part)
			if errRead != nil {
				return nil, nil, nil, errRead
			}
			if name == "model" {
				models = append(models, string(value))
			} else {
				streams = append(streams, string(value))
			}
		default:
			if _, errCopy := io.Copy(io.Discard, part); errCopy != nil {
				return nil, nil, nil, errCopy
			}
		}
	}
}

func (h *OpenAIAPIHandler) handleCompatibleAudio(c *gin.Context, raw []byte, model, protocol string) {
	ctx, cancel := h.GetContextWithCancel(h, c, context.Background())
	defer cancel(nil)
	// Keep-alive whitespace would corrupt binary audio and plain transcription formats.
	body, headers, err := h.ExecuteWithAuthManager(ctx, protocol, model, raw, "")
	if err != nil {
		h.WriteErrorResponse(c, err)
		cancel(err.Error)
		return
	}
	contentType := headers.Get("Content-Type")
	if contentType == "" {
		contentType = http.DetectContentType(body)
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), headers)
	c.Data(http.StatusOK, contentType, body)
}
