package parser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reviactyl/agent/config"
)

func TestConfigurationFileParseRejectsUnknownParser(t *testing.T) {
	config.Set(&config.Configuration{AuthenticationToken: "test-token"})
	for _, name := range []string{"unsupported", "", "JSON"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			original := []byte("value=original\n")
			require.NoError(t, os.WriteFile(path, original, 0o600))
			file, err := os.OpenFile(path, os.O_RDWR, 0)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, file.Close()) })
			cf := ConfigurationFile{
				Parser: ConfigurationParser(name),
				Replace: []ConfigurationFileReplacement{
					replacement(t, `{"match":"value","replace_with":"updated"}`),
				},
			}

			err = cf.Parse(file)
			content, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			require.Equal(t, original, content)
			require.ErrorContains(t, err, "unsupported configuration parser")
			require.Contains(t, err.Error(), `"`+name+`"`)
		})
	}
}

func TestConfigurationFileParseSupportedParsers(t *testing.T) {
	config.Set(&config.Configuration{AuthenticationToken: "test-token"})
	for _, tc := range []struct {
		parser ConfigurationParser
		input  string
		match  string
		want   string
	}{
		{Properties, "value=original\n", "value", "value=updated\n"},
		{File, "original\n", "original", "updated\n"},
		{Yaml, "value: original\n", "value", "value: updated\n"},
		{"yml", "value: original\n", "value", "value: updated\n"},
		{Json, `{"value":"original"}`, "value", "{\n    \"value\": \"updated\"\n}"},
		{Ini, "value=original\n", "value", "value = updated\n"},
		{Xml, "<root><value>original</value></root>", "root.value", "<root>\n  <value>updated</value>\n</root>\n"},
	} {
		t.Run(tc.parser.String(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			require.NoError(t, os.WriteFile(path, []byte(tc.input), 0o600))
			file, err := os.OpenFile(path, os.O_RDWR, 0)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, file.Close()) })
			raw, err := json.Marshal(map[string]interface{}{
				"file": "config", "parser": tc.parser,
				"replace": []map[string]string{{"match": tc.match, "replace_with": "updated"}},
			})
			require.NoError(t, err)
			var cf ConfigurationFile
			require.NoError(t, json.Unmarshal(raw, &cf))

			require.NoError(t, cf.Parse(file))
			content, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, tc.want, string(content))
		})
	}
}
