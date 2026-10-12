package helps

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// ValidateNanImagesPayload runs after payload rules without changing the business payload.
func ValidateNanImagesPayload(body []byte, contentType string) error {
	if gjson.ValidBytes(body) {
		if gjson.GetBytes(body, "mask").Exists() {
			return errors.New("NaN image editing does not support mask")
		}
		if gjson.GetBytes(body, "stream").Bool() {
			return errors.New("NaN image operations do not support stream")
		}
		return nil
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" {
		return errors.New("NaN image request must be JSON or multipart/form-data")
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return errors.New("invalid NaN image multipart body")
		}
		if part.FormName() == "mask" {
			_ = part.Close()
			return errors.New("NaN image editing does not support mask")
		}
		if part.FormName() == "stream" {
			value, err := io.ReadAll(io.LimitReader(part, 16))
			_ = part.Close()
			if err != nil || strings.TrimSpace(string(value)) != "false" {
				return errors.New("NaN image operations do not support stream")
			}
		}
		if part.FormName() == "image" || part.FormName() == "image[]" {
			var prefix [512]byte
			n, readErr := io.ReadFull(part, prefix[:])
			if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
				return errors.New("invalid NaN reference image")
			}
			kind := http.DetectContentType(prefix[:n])
			if kind != "image/png" && kind != "image/jpeg" && kind != "image/webp" {
				return errors.New("NaN reference images must be PNG, JPEG or WebP")
			}
			const maxImageBytes = 25_000_000
			rest, readErr := io.CopyN(io.Discard, part, maxImageBytes-int64(n))
			if int64(n)+rest >= maxImageBytes {
				return errors.New("NaN reference images must be under 25 MB each")
			}
			if readErr != nil && readErr != io.EOF {
				return errors.New("invalid NaN reference image")
			}
		}
		if err := part.Close(); err != nil {
			return err
		}
	}
}
