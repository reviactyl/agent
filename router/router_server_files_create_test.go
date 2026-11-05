package router

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCreateFileInterruptedBodyCanBeRetried(t *testing.T) {
	u := newUploadTest(t, 1, 0)
	address, err := url.Parse(u.url)
	require.NoError(t, err)
	conn, err := net.Dial("tcp", address.Host)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = fmt.Fprintf(conn, "POST /api/servers/%s/files/create?file=retry.txt HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer test-token\r\nContent-Length: 20\r\n\r\npartial", uploadTestServerUuid, address.Host)
	require.NoError(t, err)
	require.NoError(t, conn.(*net.TCPConn).CloseWrite())
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusInternalServerError, response.StatusCode)
	_, err = os.Lstat(filepath.Join(u.root, "retry.txt"))
	require.True(t, os.IsNotExist(err))
	entries, err := os.ReadDir(u.root)
	require.NoError(t, err)
	require.Empty(t, entries)

	req, err := http.NewRequest(http.MethodPost, u.url+"/api/servers/"+uploadTestServerUuid+"/files/create?file=retry.txt", strings.NewReader("complete"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer test-token")
	response, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusNoContent, response.StatusCode)
	data, err := os.ReadFile(filepath.Join(u.root, "retry.txt"))
	require.NoError(t, err)
	require.Equal(t, "complete", string(data))
}

func TestCreateFileRefusesExistingTargets(t *testing.T) {
	u := newUploadTest(t, 1, 0)
	require.NoError(t, os.WriteFile(filepath.Join(u.root, "existing.txt"), []byte("original"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(u.root, "directory"), 0o755))
	require.NoError(t, os.Symlink("existing.txt", filepath.Join(u.root, "link")))
	require.NoError(t, os.Symlink("missing.txt", filepath.Join(u.root, "dangling")))
	for _, name := range []string{"existing.txt", "directory", "link", "dangling"} {
		req, err := http.NewRequest(http.MethodPost, u.url+"/api/servers/"+uploadTestServerUuid+"/files/create?file="+name, strings.NewReader("replacement"))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer test-token")
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.NoError(t, res.Body.Close())
		require.Equal(t, http.StatusConflict, res.StatusCode, name)
	}
	data, err := os.ReadFile(filepath.Join(u.root, "existing.txt"))
	require.NoError(t, err)
	require.Equal(t, "original", string(data))
	_, err = os.Lstat(filepath.Join(u.root, "missing.txt"))
	require.True(t, os.IsNotExist(err))
}

func TestCreateFileConcurrentRequestsPreserveFirstWriter(t *testing.T) {
	u := newUploadTest(t, 1, 0)
	const count = 12
	start := make(chan struct{})
	type result struct {
		status  int
		content string
		err     error
	}
	results := make(chan result, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			content := fmt.Sprintf("writer-%d", i)
			req, err := http.NewRequest(http.MethodPost, u.url+"/api/servers/"+uploadTestServerUuid+"/files/create?file=nested/new.txt", strings.NewReader(content))
			if err != nil {
				results <- result{err: err}
				return
			}
			req.Header.Set("Authorization", "Bearer test-token")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				results <- result{err: err}
				return
			}
			err = res.Body.Close()
			results <- result{res.StatusCode, content, err}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	winner := ""
	for r := range results {
		require.NoError(t, r.err)
		if r.status == http.StatusNoContent {
			successes++
			winner = r.content
		} else {
			require.Equal(t, http.StatusConflict, r.status)
		}
	}
	require.Equal(t, 1, successes)
	data, err := os.ReadFile(filepath.Join(u.root, "nested/new.txt"))
	require.NoError(t, err)
	require.Equal(t, winner, string(data))
}
