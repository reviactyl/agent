package parser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/magiconair/properties"
	"github.com/stretchr/testify/require"
)

func TestPropertiesRewritePreservesMOTDAndFollowingProperty(t *testing.T) {
	for _, motd := range []string{`Welcome"`, `"Welcome"`, `Welcome`, `Welcome\`, `Welcome\"`, `§Welcome"`, ""} {
		t.Run(motd, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "server.properties")
			require.NoError(t, os.WriteFile(path, []byte("motd=Old MOTD\nserver-port=25565\n"), 0o600))
			raw, err := json.Marshal(map[string]interface{}{
				"file": "server.properties", "parser": Properties,
				"replace": []map[string]string{{"match": "motd", "replace_with": motd}},
			})
			require.NoError(t, err)
			var cf ConfigurationFile
			require.NoError(t, json.Unmarshal(raw, &cf))
			file, err := os.OpenFile(path, os.O_RDWR, 0)
			require.NoError(t, err)
			defer file.Close()

			require.NoError(t, cf.parsePropertiesFile(file))
			content, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Contains(t, string(content), "\nserver-port=25565\n")
			parsed, err := properties.Load(content, properties.UTF8)
			require.NoError(t, err)
			require.Equal(t, motd, parsed.GetString("motd", "missing"))
			require.Equal(t, "25565", parsed.GetString("server-port", "missing"))
			require.Equal(t, []string{"motd", "server-port"}, parsed.Keys())
		})
	}
}
