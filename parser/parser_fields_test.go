package parser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Control: a present key is substituted into the placeholder.
func TestPresentConfigurationKeySubstitutes(t *testing.T) {
	f := ConfigurationFile{configuration: []byte(`{"docker":{"interface":"eth0"}}`)}
	cfr := replacement(t, `{"match":"bind-address","replace_with":"{{config.docker.interface}}"}`)

	got, err := f.LookupConfigurationValue(cfr)
	require.NoError(t, err)
	assert.Equal(t, "eth0", got)
}

// A placeholder for a key that does not exist is left in place.
func TestMissingConfigurationKeyKeepsPlaceholder(t *testing.T) {
	f := ConfigurationFile{configuration: []byte(`{"docker":{"interface":"eth0"}}`)}
	cfr := replacement(t, `{"match":"bind-address","replace_with":"{{config.docker.missing}}"}`)

	got, err := f.LookupConfigurationValue(cfr)
	require.NoError(t, err)
	assert.Equal(t, "{{config.docker.missing}}", got, "a missing key should leave the placeholder in place")
}

// A configuration file entry that is missing a field is refused when it is decoded.
func TestConfigurationFileWithoutParserKeyReturnsError(t *testing.T) {
	var f ConfigurationFile
	var err error
	require.NotPanics(t, func() {
		err = json.Unmarshal([]byte(`{"file":"server.properties","replace":[]}`), &f)
	})
	require.Error(t, err)

	for _, raw := range []string{`{"parser":"file","replace":[]}`, `{"file":"a","parser":"file"}`} {
		require.NotPanics(t, func() {
			err = json.Unmarshal([]byte(raw), &f)
		})
		require.Error(t, err, raw)
	}
}

// An invalid escape in replace_with is refused while the document is validated,
// before any replacement value is built.
func TestInvalidReplacementEscapeIsRejectedBeforeDecoding(t *testing.T) {
	var f ConfigurationFile
	var err error
	require.NotPanics(t, func() {
		err = json.Unmarshal([]byte(`{"file":"a","parser":"file","replace":[{"match":"k","replace_with":"\z"}]}`), &f)
	})
	require.Error(t, err)
}

func TestConfigurationFilePreservesReplacementScalarTypes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want interface{}
	}{
		{"leading zero string", `"001234"`, "001234"},
		{"numeric string", `"1234"`, "1234"},
		{"decimal string", `"12.50"`, "12.50"},
		{"integer", `1234`, float64(1234)},
		{"unsigned integer", `9223372036854775809`, uint64(9223372036854775809)},
		{"decimal", `12.5`, 12.5},
		{"exponent", `1.25e3`, float64(1250)},
		{"boolean", `true`, true},
	}
	for _, format := range []ConfigurationParser{Json, Yaml} {
		for _, tc := range cases {
			for _, condition := range []string{"", "original"} {
				t.Run(string(format)+"/"+tc.name+"/if_value="+condition, func(t *testing.T) {
					path := filepath.Join(t.TempDir(), "config")
					content := `{"value":"original"}`
					if format == Yaml {
						content = "value: original\n"
					}
					require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
					file, err := os.OpenFile(path, os.O_RDWR, 0)
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, file.Close()) })
					cf := ConfigurationFile{Replace: []ConfigurationFileReplacement{
						replacement(t, `{"match":"value","if_value":"`+condition+`","replace_with":`+tc.raw+`}`),
					}}
					if format == Json {
						require.NoError(t, cf.parseJsonFile(file))
					} else {
						require.NoError(t, cf.parseYamlFile(file))
					}
					output, err := os.ReadFile(path)
					require.NoError(t, err)
					var got map[string]interface{}
					if format == Json {
						if _, ok := tc.want.(uint64); ok {
							var exact map[string]json.Number
							require.NoError(t, json.Unmarshal(output, &exact))
							require.Equal(t, tc.raw, exact["value"].String())
							return
						}
						require.NoError(t, json.Unmarshal(output, &got))
					} else {
						require.NoError(t, yaml.Unmarshal(output, &got))
						if value, ok := got["value"].(int); ok {
							got["value"] = float64(value)
						}
					}
					require.Equal(t, tc.want, got["value"], "written config: %s", output)
				})
			}
		}
	}
}
