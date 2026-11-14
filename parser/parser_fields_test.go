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

func TestConfigurationPlaceholderExpansion(t *testing.T) {
	f := ConfigurationFile{configuration: []byte(`{"docker":{"interface":"eth0","network":{"interface":"172.18.0.1"}},"name":"$1-${value}-$$","nested":"{{config.docker.interface}}","enabled":true,"port":25565,"items":["first"]}`)}
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{"distinct", "{{config.docker.interface}}:{{config.docker.network.interface}}", "eth0:172.18.0.1"},
		{"repeated and distinct", "{{config.docker.interface}}/{{config.docker.network.interface}}/{{config.docker.interface}}", "eth0/172.18.0.1/eth0"},
		{"literal text", "prefix [{{ config.docker.interface }}]:{{config.docker.network.interface}} suffix $1", "prefix [eth0]:172.18.0.1 suffix $1"},
		{"known then unknown", "{{config.docker.interface}}:{{config.missing}}", "eth0:{{config.missing}}"},
		{"unknown then known", "{{config.missing}}:{{config.docker.network.interface}}", "{{config.missing}}:172.18.0.1"},
		{"non scalar then known", "{{config.docker}}:{{config.items}}:{{config.docker.interface}}", "{{config.docker}}:{{config.items}}:eth0"},
		{"literal dollar values", "{{config.name}}:{{config.docker.interface}}", "$1-${value}-$$:eth0"},
		{"single pass", "{{config.nested}}:{{config.docker.network.interface}}", "{{config.docker.interface}}:172.18.0.1"},
		{"scalar values", "{{config.enabled}}:{{config.port}}", "true:25565"},
		{"no placeholders", "literal $1 {{other.value}}", "literal $1 {{other.value}}"},
		{"unknown only", "{{config.missing}}", "{{config.missing}}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value, err := json.Marshal(tc.value)
			require.NoError(t, err)
			cfr := replacement(t, `{"match":"value","replace_with":`+string(value)+`}`)
			got, err := f.LookupConfigurationValue(cfr)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
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

func TestConfigurationFileRejectsMalformedReplacements(t *testing.T) {
	for name, replacements := range map[string]string{
		"object":             `{}`,
		"string":             `"invalid"`,
		"number":             `1`,
		"boolean":            `false`,
		"null entry":         `[null]`,
		"scalar entry":       `[1]`,
		"missing match":      `[{"replace_with":"new"}]`,
		"missing value":      `[{"match":"key"}]`,
		"invalid match":      `[{"match":{},"replace_with":"new"}]`,
		"invalid condition":  `[{"match":"key","if_value":[],"replace_with":"new"}]`,
		"valid then invalid": `[{"match":"key","replace_with":"new"},{"match":"other"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			var f ConfigurationFile
			err := json.Unmarshal([]byte(`{"file":"server.properties","parser":"properties","replace":`+replacements+`}`), &f)
			require.ErrorContains(t, err, "replacement")
			require.ErrorContains(t, err, "server.properties")
		})
	}
}

func TestConfigurationFileDecodesValidReplacements(t *testing.T) {
	for name, tc := range map[string]struct {
		replacements string
		match        string
		condition    string
		value        string
	}{
		"empty":         {replacements: `[]`},
		"string":        {`[{"match":"key","replace_with":"new"}]`, "key", "", "new"},
		"legacy value":  {`[{"match":"key","value":"legacy"}]`, "key", "", "legacy"},
		"numeric key":   {`[{"match":25565,"if_value":1,"replace_with":25566}]`, "25565", "1", "25566"},
		"boolean key":   {`[{"match":true,"if_value":false,"replace_with":false}]`, "true", "false", "false"},
		"null scalars":  {`[{"match":null,"if_value":null,"replace_with":null}]`, "", "", "<nil>"},
		"escaped value": {`[{"match":"key","replace_with":"\u00a7new"}]`, "key", "", "§new"},
	} {
		t.Run(name, func(t *testing.T) {
			var f ConfigurationFile
			err := json.Unmarshal([]byte(`{"file":"server.properties","parser":"properties","replace":`+tc.replacements+`}`), &f)
			require.NoError(t, err)
			if tc.replacements == `[]` {
				require.NotNil(t, f.Replace)
				require.Empty(t, f.Replace)
				return
			}
			require.Len(t, f.Replace, 1)
			require.Equal(t, tc.match, f.Replace[0].Match)
			require.Equal(t, tc.condition, f.Replace[0].IfValue)
			require.Equal(t, tc.value, f.Replace[0].ReplaceWith.String())
		})
	}
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

func TestConfigurationFileRejectsOutOfRangeIntegersWithoutRewriting(t *testing.T) {
	for _, format := range []ConfigurationParser{Json, Yaml} {
		for _, raw := range []string{"18446744073709551617", "-9223372036854775809"} {
			for _, condition := range []string{"", "original"} {
				t.Run(string(format)+"/"+raw+"/if_value="+condition, func(t *testing.T) {
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
						replacement(t, `{"match":"value","if_value":"`+condition+`","replace_with":`+raw+`}`),
					}}
					if format == Json {
						err = cf.parseJsonFile(file)
					} else {
						err = cf.parseYamlFile(file)
					}
					require.ErrorContains(t, err, "integer replacement exceeds supported range")
					output, err := os.ReadFile(path)
					require.NoError(t, err)
					require.Equal(t, content, string(output))
				})
			}
		}
	}
}

func TestConfigurationFilePreservesUnrelatedLargeIDs(t *testing.T) {
	for _, format := range []ConfigurationParser{Json, Yaml} {
		for _, id := range []string{"9007199254740993", "9223372036854775809", "18446744073709551615"} {
			t.Run(string(format)+"/"+id, func(t *testing.T) {
				content := `{"id":` + id + `,"nested":{"id":` + id + `},"ids":[` + id + `],"value":"original"}`
				if format == Yaml {
					content = "id: " + id + "\nnested:\n  id: " + id + "\nids: [" + id + "]\nvalue: original\n"
				}
				path := filepath.Join(t.TempDir(), "config")
				require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
				file, err := os.OpenFile(path, os.O_RDWR, 0)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, file.Close()) })
				cf := ConfigurationFile{Replace: []ConfigurationFileReplacement{
					replacement(t, `{"match":"value","replace_with":"updated"}`),
				}}
				if format == Json {
					err = cf.parseJsonFile(file)
				} else {
					err = cf.parseYamlFile(file)
				}
				require.NoError(t, err)
				output, err := os.ReadFile(path)
				require.NoError(t, err)
				var document yaml.Node
				require.NoError(t, yaml.Unmarshal(output, &document))
				root := document.Content[0]
				values := make(map[string]*yaml.Node)
				for i := 0; i < len(root.Content); i += 2 {
					values[root.Content[i].Value] = root.Content[i+1]
				}
				for _, node := range []*yaml.Node{values["id"], values["nested"].Content[1], values["ids"].Content[0]} {
					require.Equal(t, "!!int", node.Tag, "written config: %s", output)
					require.Equal(t, id, node.Value, "written config: %s", output)
				}
				require.Equal(t, "updated", values["value"].Value)
			})
		}
	}
}

func TestJSONRewriteRejectsTrailingContent(t *testing.T) {
	for _, content := range []string{`{"id":9007199254740993} {}`, `{"id":9007199254740993} garbage`} {
		cf := ConfigurationFile{}
		_, err := cf.IterateOverJson([]byte(content))
		require.Error(t, err)
	}
}

func TestYAMLRewriteNullRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("null\n"), 0o600))
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	cf := ConfigurationFile{Replace: []ConfigurationFileReplacement{
		replacement(t, `{"match":"value","replace_with":"updated"}`),
	}}
	require.NoError(t, cf.parseYamlFile(file))
	output, err := os.ReadFile(path)
	require.NoError(t, err)
	var got map[string]string
	require.NoError(t, yaml.Unmarshal(output, &got))
	require.Equal(t, "updated", got["value"])
}
