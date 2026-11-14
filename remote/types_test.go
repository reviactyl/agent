package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGetServerConfigurationRejectsMalformedReplacements(t *testing.T) {
	c, server := createTestClient(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/servers/test-server", r.URL.Path)
		_, err := w.Write([]byte(`{"process_configuration":{"configs":[{"file":"server.properties","parser":"properties","replace":[{"match":"port","replace_with":25565},{"match":"motd"}]}]}}`))
		require.NoError(t, err)
	})
	defer server.Close()

	_, err := c.GetServerConfiguration(context.Background(), "test-server")
	require.ErrorContains(t, err, "replacement")
	require.ErrorContains(t, err, "server.properties")
}

func TestOutputLineMatcher(t *testing.T) {
	for _, tt := range []struct {
		raw   string
		line  string
		match bool
	}{
		{raw: "Done", line: "[12:00] Done (1.2s)!", match: true},
		{raw: "Done", line: "Loading", match: false},
		{raw: `regex:^\[\d+:\d+\] Done`, line: "[12:00] Done (1.2s)!", match: true},
		{raw: `regex:^Done`, line: "[12:00] Done", match: false},
		// An expression that does not compile is matched as plain text.
		{raw: "regex:Done (", line: "[12:00] Done (1.2s)!", match: true},
		{raw: "regex:Done (", line: "[12:00] Loading", match: false},
	} {
		var m OutputLineMatcher
		b, err := json.Marshal(tt.raw)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(b, &m))
		require.Equal(t, tt.match, m.Matches([]byte(tt.line)), "%q against %q", tt.raw, tt.line)
		require.Equal(t, tt.raw, m.String())
	}
}
