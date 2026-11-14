package router

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func renameFileRequest(t *testing.T, u *uploadTest, from, to string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, u.url+"/api/servers/"+uploadTestServerUuid+"/files/rename", bytes.NewBufferString(`{"root":"","files":[{"from":"`+from+`","to":"`+to+`"}]}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer test-token")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return res
}

func TestRenameFileReportsMissingSource(t *testing.T) {
	u := newUploadTest(t, 1, 0)
	res := renameFileRequest(t, u, "missing.txt", "renamed.txt")
	defer res.Body.Close()

	require.Equal(t, http.StatusNotFound, res.StatusCode)
	_, err := os.Stat(filepath.Join(u.root, "renamed.txt"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRenameFileReturnsNoContentOnSuccess(t *testing.T) {
	u := newUploadTest(t, 1, 0)
	require.NoError(t, os.WriteFile(filepath.Join(u.root, "source.txt"), []byte("content"), 0o644))

	res := renameFileRequest(t, u, "source.txt", "renamed.txt")
	defer res.Body.Close()

	require.Equal(t, http.StatusNoContent, res.StatusCode)
	_, err := os.Stat(filepath.Join(u.root, "source.txt"))
	require.ErrorIs(t, err, os.ErrNotExist)
	content, err := os.ReadFile(filepath.Join(u.root, "renamed.txt"))
	require.NoError(t, err)
	require.Equal(t, "content", string(content))
}
