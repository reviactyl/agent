package backup

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestS3UploadPartContentTypeMatchesArchiveFormat(t *testing.T) {
	for _, test := range []struct {
		format      string
		contentType string
	}{
		{format: "zip", contentType: "application/zip"},
		{format: "tar.gz", contentType: "application/x-gzip"},
	} {
		t.Run(test.format, func(t *testing.T) {
			var contentType string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				contentType = r.Header.Get("Content-Type")
				w.Header().Set("ETag", "etag")
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			uploader := newS3FileUploader(io.NopCloser(strings.NewReader("archive data")), test.format)
			uploader.client = server.Client()
			if _, err := uploader.uploadPart(context.Background(), server.URL, int64(len("archive data"))); err != nil {
				t.Fatal(err)
			}
			if contentType != test.contentType {
				t.Fatalf("part content type = %q, want %q", contentType, test.contentType)
			}
		})
	}
}
