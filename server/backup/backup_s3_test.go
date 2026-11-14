package backup

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

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
