package backup

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
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

			uploader := newS3FileUploader(strings.NewReader("archive data"), test.format)
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

func TestS3FileUploaderRetriesMultipartPartWithFullBody(t *testing.T) {
	partOne := []byte("first-part")
	partTwo := []byte("second-part")
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			return
		}
		bodies = append(bodies, body)
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("ETag", `"etag"`)
	}))
	defer server.Close()

	uploader := newS3FileUploader(bytes.NewReader(append(partOne, partTwo...)))
	uploader.client = server.Client()

	firstETag, err := uploader.uploadPart(t.Context(), server.URL, int64(len(partOne)))
	if err != nil {
		t.Fatalf("upload first part: %v", err)
	}
	secondETag, err := uploader.uploadPart(t.Context(), server.URL, int64(len(partTwo)))
	if err != nil {
		t.Fatalf("upload second part: %v", err)
	}

	if firstETag != `"etag"` || secondETag != `"etag"` {
		t.Fatalf("ETags = %q and %q, want %q", firstETag, secondETag, `"etag"`)
	}
	wantBodies := [][]byte{partOne, partOne, partTwo}
	if !reflect.DeepEqual(bodies, wantBodies) {
		t.Fatalf("request bodies = %q, want %q", bodies, wantBodies)
	}
}
