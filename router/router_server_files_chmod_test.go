package router

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChmodFileResponses(t *testing.T) {
	for _, tt := range []struct {
		name   string
		file   string
		mode   string
		status int
	}{
		{name: "existing", file: "existing.txt", mode: "600", status: http.StatusNoContent},
		{name: "missing", file: "missing.txt", mode: "644", status: http.StatusNotFound},
		{name: "removed", file: "removed.txt", mode: "644", status: http.StatusNotFound},
		{name: "missing parent", file: "missing/file.txt", mode: "644", status: http.StatusNotFound},
		{name: "symlink", file: "link", mode: "600", status: http.StatusInternalServerError},
		{name: "invalid mode", file: "existing.txt", mode: "invalid", status: http.StatusBadRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			u := newUploadTest(t, 1, 0)
			require.NoError(t, os.WriteFile(filepath.Join(u.root, "existing.txt"), []byte("original"), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(u.root, "removed.txt"), nil, 0o644))
			require.NoError(t, os.Remove(filepath.Join(u.root, "removed.txt")))
			require.NoError(t, os.Symlink("existing.txt", filepath.Join(u.root, "link")))

			body, err := json.Marshal(map[string]any{"root": "/", "files": []chmodFile{{File: tt.file, Mode: tt.mode}}})
			require.NoError(t, err)
			req, err := http.NewRequest(http.MethodPost, u.url+"/api/servers/"+uploadTestServerUuid+"/files/chmod", strings.NewReader(string(body)))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer test-token")
			req.Header.Set("Content-Type", "application/json")

			res, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer res.Body.Close()
			response, err := io.ReadAll(res.Body)
			require.NoError(t, err)
			require.Equal(t, tt.status, res.StatusCode, string(response))
			if tt.status == http.StatusNotFound {
				require.Contains(t, string(response), "file was not found")
			} else if tt.status != http.StatusNoContent {
				require.Contains(t, string(response), `"error"`)
			}
			info, err := os.Stat(filepath.Join(u.root, "existing.txt"))
			require.NoError(t, err)
			expectedMode := os.FileMode(0o644)
			if tt.status == http.StatusNoContent {
				expectedMode = 0o600
			}
			require.Equal(t, expectedMode, info.Mode().Perm())
			content, err := os.ReadFile(filepath.Join(u.root, "existing.txt"))
			require.NoError(t, err)
			require.Equal(t, "original", string(content))
		})
	}
}
