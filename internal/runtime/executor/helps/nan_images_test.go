package helps

import (
	"bytes"
	"mime/multipart"
	"testing"
)

func TestNanReferenceImageValidationKeepsDocumentedFormatsAndSizeBoundary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix []byte
		size   int
		valid  bool
	}{
		{"png", []byte("\x89PNG\r\n\x1a\n"), 32, true},
		{"jpeg", []byte("\xff\xd8\xff"), 32, true},
		{"webp", []byte("RIFF\x00\x00\x00\x00WEBPVP8 "), 32, true},
		{"wrong format", []byte("not an image"), 32, false},
		{"empty", nil, 0, false},
		{"under maximum", []byte("\x89PNG\r\n\x1a\n"), 24_999_999, true},
		{"at maximum", []byte("\x89PNG\r\n\x1a\n"), 25_000_000, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			part, err := writer.CreateFormFile("image[]", "reference.bin")
			if err != nil {
				t.Fatal(err)
			}
			data := make([]byte, tc.size)
			copy(data, tc.prefix)
			if _, err := part.Write(data); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			err = ValidateNanImagesPayload(body.Bytes(), writer.FormDataContentType())
			if (err == nil) != tc.valid {
				t.Fatalf("size/format validation: valid=%v error=%v", tc.valid, err)
			}
		})
	}
}
