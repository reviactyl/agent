package remote

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
)

type trackingResponseBody struct {
	io.Reader
	closed atomic.Int32
}

func (b *trackingResponseBody) Close() error {
	b.closed.Add(1)
	return nil
}

type responseTransportFunc func(*http.Request) (*http.Response, error)

func (f responseTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type errorResponseReader struct{}

func (errorResponseReader) Read(p []byte) (int, error) {
	copy(p, "partial")
	return len("partial"), fmt.Errorf("read failed")
}

func createTestClient(h http.HandlerFunc) (*client, *httptest.Server) {
	s := httptest.NewServer(h)
	c := &client{
		httpClient:  s.Client(),
		baseUrl:     s.URL,
		maxAttempts: 1,
		tokenId:     "testid",
		token:       "testtoken",
	}
	return c, s
}

func TestRequest(t *testing.T) {
	c, _ := createTestClient(func(rw http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "application/vnd.reviactyl.v26+json", r.Header.Get("Accept"))
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		assert.Equal(t, "Bearer testid.testtoken", r.Header.Get("Authorization"))
		assert.Equal(t, "/test", r.URL.Path)

		rw.WriteHeader(http.StatusOK)
	})
	r, err := c.requestOnce(context.Background(), "", "/test", nil)
	assert.NoError(t, err)
	assert.NotNil(t, r)
}

func TestSetCredentials(t *testing.T) {
	var authorization []string
	c, server := createTestClient(func(rw http.ResponseWriter, r *http.Request) {
		authorization = append(authorization, r.Header.Get("Authorization"))
		rw.WriteHeader(http.StatusOK)
	})
	defer server.Close()

	if _, err := c.requestOnce(context.Background(), http.MethodGet, "/test", nil); err != nil {
		t.Fatal(err)
	}
	c.SetCredentials("rotated-id", "rotated-token")
	if _, err := c.requestOnce(context.Background(), http.MethodGet, "/test", nil); err != nil {
		t.Fatal(err)
	}

	assert.Equal(t, []string{
		"Bearer testid.testtoken",
		"Bearer rotated-id.rotated-token",
	}, authorization)
}

func TestRequestRetry(t *testing.T) {
	// Test if the client attempts failed requests
	i := 0
	c, _ := createTestClient(func(rw http.ResponseWriter, r *http.Request) {
		if i < 1 {
			rw.WriteHeader(http.StatusInternalServerError)
		} else {
			rw.WriteHeader(http.StatusOK)
		}
		i++
	})
	c.maxAttempts = 2
	r, err := c.request(context.Background(), "", "", nil)
	assert.NoError(t, err)
	assert.NotNil(t, r)
	assert.Equal(t, http.StatusOK, r.StatusCode)
	assert.Equal(t, 2, i)

	// Test whether the client returns the last request after retry limit is reached
	i = 0
	c, _ = createTestClient(func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusInternalServerError)
		i++
	})
	c.maxAttempts = 2
	r, err = c.request(context.Background(), "get", "", nil)
	assert.Error(t, err)
	assert.Nil(t, r)

	v := AsRequestError(err)
	assert.NotNil(t, v)
	assert.Equal(t, http.StatusInternalServerError, v.StatusCode())
	assert.Equal(t, 3, i)
}

func TestGet(t *testing.T) {
	c, _ := createTestClient(func(rw http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Len(t, r.URL.Query(), 1)
		assert.Equal(t, "world", r.URL.Query().Get("hello"))
	})
	r, err := c.Get(context.Background(), "/test", q{"hello": "world"})
	assert.NoError(t, err)
	assert.NotNil(t, r)
}

func TestPost(t *testing.T) {
	test := map[string]string{
		"hello": "world",
	}
	c, _ := createTestClient(func(rw http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
	})
	r, err := c.Post(context.Background(), "/test", test)
	assert.NoError(t, err)
	assert.NotNil(t, r)
}

func TestResponseReadClosesAndReplaysBody(t *testing.T) {
	body := &trackingResponseBody{Reader: bytes.NewBufferString(`{"ok":true}`)}
	response := &Response{Response: &http.Response{Body: body}}

	got, err := response.Read()
	assert.NoError(t, err)
	assert.Equal(t, `{"ok":true}`, string(got))
	assert.EqualValues(t, 1, body.closed.Load())

	got, err = response.Read()
	assert.NoError(t, err)
	assert.Equal(t, `{"ok":true}`, string(got))
	assert.EqualValues(t, 1, body.closed.Load())
}

func TestResponseReadReturnsReadErrorAfterClosingBody(t *testing.T) {
	body := &trackingResponseBody{Reader: errorResponseReader{}}
	response := &Response{Response: &http.Response{Body: body}}

	got, err := response.Read()
	assert.EqualError(t, err, "remote: could not read response: read failed")
	assert.Equal(t, "partial", string(got))
	assert.EqualValues(t, 1, body.closed.Load())
	got, err = response.Read()
	assert.NoError(t, err)
	assert.Equal(t, "partial", string(got))
	assert.EqualValues(t, 1, body.closed.Load())
}

func TestResponseReadMissingAndEmptyBody(t *testing.T) {
	_, err := (&Response{}).Read()
	assert.EqualError(t, err, "remote: attempting to read missing response")
	response := &Response{Response: &http.Response{}}
	got, err := response.Read()
	assert.NoError(t, err)
	assert.Empty(t, got)
	assert.NotNil(t, response.Body)
}

func TestResponseBindJSONFailureClosesBody(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reader  io.Reader
		message string
	}{
		{name: "invalid JSON", reader: bytes.NewBufferString("invalid"), message: "remote: could not unmarshal response"},
		{name: "read failure", reader: errorResponseReader{}, message: "remote: could not read response: read failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &trackingResponseBody{Reader: tc.reader}
			response := &Response{Response: &http.Response{Body: body}}
			var parsed map[string]interface{}
			err := response.BindJSON(&parsed)
			assert.ErrorContains(t, err, tc.message)
			assert.Nil(t, parsed)
			assert.EqualValues(t, 1, body.closed.Load())
		})
	}
}

func TestResponseBindJSONParsesSuccessAndErrorResponses(t *testing.T) {
	successBody := &trackingResponseBody{Reader: bytes.NewBufferString(`{"ok":true}`)}
	success := &Response{Response: &http.Response{StatusCode: http.StatusOK, Body: successBody}}
	var parsed struct {
		OK bool `json:"ok"`
	}
	assert.NoError(t, success.BindJSON(&parsed))
	assert.True(t, parsed.OK)
	assert.EqualValues(t, 1, successBody.closed.Load())

	errorBody := &trackingResponseBody{Reader: bytes.NewBufferString(`{"errors":[{"code":"missing","status":"404","detail":"not found"}]}`)}
	errorResponse := &Response{Response: &http.Response{StatusCode: http.StatusNotFound, Body: errorBody}}
	err := errorResponse.Error()
	requestErr := AsRequestError(err)
	if assert.NotNil(t, requestErr) {
		assert.Equal(t, "missing", requestErr.Code)
		assert.Equal(t, "not found", requestErr.Detail)
	}
	assert.EqualValues(t, 1, errorBody.closed.Load())
	err = errorResponse.Error()
	assert.Equal(t, "missing", AsRequestError(err).Code)
	assert.EqualValues(t, 1, errorBody.closed.Load())
}

func TestRemoteParsingMethodsCloseConsumedBodyOnce(t *testing.T) {
	for name, call := range map[string]func(*client) error{
		"server configuration": func(c *client) error { _, err := c.GetServerConfiguration(context.Background(), "server"); return err },
		"installation script":  func(c *client) error { _, err := c.GetInstallationScript(context.Background(), "server"); return err },
		"SFTP credentials": func(c *client) error {
			_, err := c.ValidateSftpCredentials(context.Background(), SftpAuthRequest{})
			return err
		},
		"backup upload URLs": func(c *client) error {
			_, err := c.GetBackupRemoteUploadURLs(context.Background(), "backup", 1)
			return err
		},
		"server list": func(c *client) error { _, err := c.GetServers(context.Background(), 10); return err },
	} {
		t.Run(name, func(t *testing.T) {
			for _, payload := range []string{`{}`, `invalid`} {
				body := &trackingResponseBody{Reader: bytes.NewBufferString(payload)}
				c := &client{baseUrl: "http://example", httpClient: &http.Client{Transport: responseTransportFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
				})}}
				err := call(c)
				if payload == `{}` {
					assert.NoError(t, err)
				} else {
					assert.Error(t, err)
				}
				assert.EqualValues(t, 1, body.closed.Load())
			}
		})
	}
}

func TestRequestClosesEveryFailedResponseBodyOnce(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		payload     string
		readFailure bool
		code        string
		attempts    int
	}{
		{name: "client error", status: http.StatusBadRequest, payload: `{"errors":[{"code":"failed","status":"400","detail":"invalid request"}]}`, code: "failed", attempts: 1},
		{name: "server error", status: http.StatusInternalServerError, payload: `{"errors":[{"code":"failed","status":"500","detail":"try again"}]}`, code: "failed", attempts: 2},
		{name: "invalid JSON", status: http.StatusInternalServerError, payload: "invalid", code: "_MissingResponseCode", attempts: 2},
		{name: "read failure", status: http.StatusInternalServerError, readFailure: true, code: "_MissingResponseCode", attempts: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var bodies []*trackingResponseBody
			c := &client{
				httpClient: &http.Client{Transport: responseTransportFunc(func(*http.Request) (*http.Response, error) {
					var reader io.Reader = bytes.NewBufferString(tc.payload)
					if tc.readFailure {
						reader = errorResponseReader{}
					}
					body := &trackingResponseBody{Reader: reader}
					bodies = append(bodies, body)
					return &http.Response{StatusCode: tc.status, Body: body}, nil
				})},
				baseUrl:     "http://example",
				maxAttempts: 1,
			}
			for range 3 {
				response, err := c.request(context.Background(), http.MethodGet, "/test", nil)
				assert.Nil(t, response)
				requestErr := AsRequestError(err)
				if assert.NotNil(t, requestErr) {
					assert.Equal(t, tc.code, requestErr.Code)
					assert.Equal(t, tc.status, requestErr.StatusCode())
				}
				for _, body := range bodies {
					assert.EqualValues(t, 1, body.closed.Load())
				}
			}
			assert.Len(t, bodies, tc.attempts*3)
		})
	}
}
