package docker

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/docker/docker/client"
)

type readlogTransport func(*http.Request) (*http.Response, error)

func (f readlogTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type readlogBody struct {
	io.Reader
	closed bool
}

func (r *readlogBody) Close() error {
	r.closed = true
	return nil
}

func TestReadlog(t *testing.T) {
	readErr := errors.New("log transport interrupted")
	for _, tt := range []struct {
		name string
		data string
		err  error
		want []string
	}{
		{name: "empty stream"},
		{name: "clean EOF", data: "first\nsecond\n", want: []string{"first", "second"}},
		{name: "unterminated final line", data: "first\nsecond", want: []string{"first", "second"}},
		{name: "unchanged line contents", data: "  first  \n\nsecond\r\n", want: []string{"  first  ", "", "second"}},
		{name: "read failure", err: readErr},
		{name: "read failure after partial output", data: "first\nsecond\n", err: readErr, want: []string{"first", "second"}},
		{name: "overlong line after partial output", data: "first\n" + strings.Repeat("x", bufio.MaxScanTokenSize) + "\n", want: []string{"first"}, err: bufio.ErrTooLong},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var reader io.Reader = strings.NewReader(tt.data)
			if tt.err != nil && tt.err != bufio.ErrTooLong {
				reader = io.MultiReader(reader, iotest.ErrReader(tt.err))
			}
			body := &readlogBody{Reader: reader}
			cli, err := client.NewClientWithOpts(client.WithHost("http://docker.test"), client.WithVersion("1.41"), client.WithHTTPClient(&http.Client{
				Transport: readlogTransport(func(r *http.Request) (*http.Response, error) {
					if r.Method != http.MethodGet || r.URL.Path != "/v1.41/containers/server-id/logs" {
						t.Errorf("unexpected log request: %s %s", r.Method, r.URL.Path)
					}
					if q := r.URL.Query(); q.Get("tail") != "10" || q.Get("stdout") != "1" || q.Get("stderr") != "1" {
						t.Errorf("unexpected log options: %v", q)
					}
					return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header)}, nil
				}),
			}))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cli.Close() })
			e := &Environment{Id: "server-id", client: cli}
			got, err := e.Readlog(10)
			if !errors.Is(err, tt.err) {
				t.Errorf("Readlog error = %v, want %v", err, tt.err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Readlog lines = %q, want %q", got, tt.want)
			}
			if !body.closed {
				t.Error("log response body was not closed")
			}
		})
	}
}
