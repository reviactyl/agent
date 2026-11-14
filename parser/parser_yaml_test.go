package parser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestYAMLRewriteDocuments(t *testing.T) {
	for _, tc := range []struct {
		name, input, replacements, want string
		wantError                       bool
	}{
		{
			name:         "sequence of mappings",
			input:        "- name: first\n  enabled: false\n- name: second\n  id: 18446744073709551615\n",
			replacements: `[{"match":"0.name","replace_with":"updated"},{"match":"0.enabled","replace_with":true}]`,
			want:         "- name: updated\n  enabled: true\n- name: second\n  id: 18446744073709551615\n",
		},
		{
			name:         "sequence of scalars",
			input:        "[original, 12, false, null, '00123']",
			replacements: `[{"match":"0","replace_with":"updated"},{"match":"1","replace_with":42}]`,
			want:         "[updated, 42, false, null, '00123']",
		},
		{
			name:         "nested sequences and numeric mapping keys",
			input:        "- [original, {1: original}]\n- [untouched]\n",
			replacements: `[{"match":"0.0","replace_with":"updated"},{"match":"0.1.1","replace_with":"00123"}]`,
			want:         "- [updated, {'1': '00123'}]\n- [untouched]\n",
		},
		{
			name:         "sequence conditions",
			input:        "[original, keep, prefix-old, 123]",
			replacements: `[{"match":"0","if_value":"original","replace_with":"updated"},{"match":"1","if_value":"different","replace_with":"wrong"},{"match":"2","if_value":"regex:old$","replace_with":"new"},{"match":"3","if_value":"123","replace_with":456}]`,
			want:         "[updated, keep, prefix-new, 456]",
		},
		{
			name:         "mapping wildcards and arrays",
			input:        "servers:\n  one: {port: 1}\n  two: {port: 2}\nitems: [{name: original}]\nkeep: '00123'\n",
			replacements: `[{"match":"servers.*.port","replace_with":25565},{"match":"items[0].name","replace_with":"updated"},{"match":"new.value","replace_with":true}]`,
			want:         "servers:\n  one: {port: 25565}\n  two: {port: 25565}\nitems: [{name: updated}]\nkeep: '00123'\nnew: {value: true}\n",
		},
		{name: "empty sequence", input: "[]", replacements: `[]`, want: "[]"},
		{name: "empty document", input: "", replacements: `[{"match":"value","replace_with":"updated"}]`, want: "value: updated"},
		{name: "null document", input: "null", replacements: `[{"match":"value","replace_with":"updated"}]`, want: "value: updated"},
		{name: "string root", input: "'00123'", replacements: `[]`, want: "'00123'"},
		{name: "number root", input: "18446744073709551615", replacements: `[]`, want: "18446744073709551615"},
		{name: "boolean root", input: "true", replacements: `[]`, want: "true"},
		{name: "invalid YAML", input: "[broken", replacements: `[]`, wantError: true},
		{name: "invalid list index", input: "[original]", replacements: `[{"match":"2","replace_with":"updated"}]`, wantError: true},
		{name: "invalid scalar path", input: "original", replacements: `[{"match":"value","replace_with":"updated"}]`, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(path, []byte(tc.input), 0o600))
			file, err := os.OpenFile(path, os.O_RDWR, 0)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, file.Close()) })
			var cf ConfigurationFile
			require.NoError(t, json.Unmarshal([]byte(`{"file":"config.yaml","parser":"yaml","replace":`+tc.replacements+`}`), &cf))
			err = cf.parseYamlFile(file)
			output, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			if tc.wantError {
				require.Error(t, err)
				require.Equal(t, tc.input, string(output))
				return
			}
			require.NoError(t, err)
			var want, got interface{}
			require.NoError(t, yaml.Unmarshal([]byte(tc.want), &want))
			require.NoError(t, yaml.Unmarshal(output, &got))
			require.Equal(t, want, got, "written YAML: %s", output)
		})
	}
}
