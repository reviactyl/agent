package parser

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
			_, err = file.Seek(0, 0)
			require.NoError(t, err)
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

func TestPropertiesRewriteNonBMPUnicode(t *testing.T) {
	for i, tc := range []struct{ value, escaped, input string }{
		{"Welcome 😀", `Welcome \ud83d\ude00`, ""},
		{"𐀀😀􏿿", `\ud800\udc00\ud83d\ude00\udbff\udfff`, ""},
		{`literal \U0001f600`, `literal \\U0001f600`, ""},
		{"Hello §世界", `Hello \u00a7\u4e16\u754c`, ""},
		{"Welcome 😀", `Welcome \ud83d\ude00`, `Welcome \ud83d\` + "\n \t\f" + `\ude00`},
		{"Welcome 😀", `Welcome \ud83d\ude00`, `Welcome \ud83d\` + "\r\n \t\f" + `\ude00`},
		{"Welcome 😀", `Welcome \ud83d\ude00`, `Welcome \ud83d\` + "\r \t\f" + `\ude00`},
	} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "server.properties")
			input := strings.ReplaceAll(tc.value, `\`, `\\`)
			if tc.input != "" {
				input = tc.input
			}
			require.NoError(t, os.WriteFile(path, []byte("motd="+input+"\nserver-port=25565\n"+`\ud83d\ude00=value`+"\n"), 0o600))
			file, err := os.OpenFile(path, os.O_RDWR, 0)
			require.NoError(t, err)
			defer file.Close()
			cf := ConfigurationFile{}
			require.NoError(t, cf.parsePropertiesFile(file))
			_, err = file.Seek(0, 0)
			require.NoError(t, err)
			require.NoError(t, cf.parsePropertiesFile(file))
			content, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Contains(t, string(content), "motd="+tc.escaped+"\n")
			require.Contains(t, string(content), `\ud83d\ude00=value`)
			require.Contains(t, string(content), "server-port=25565\n")

			if _, err := exec.LookPath("java"); err != nil {
				t.Skip("Java required for Properties.load integration check")
			}
			if _, err := exec.LookPath("javac"); err != nil {
				t.Skip("javac required for Properties.load integration check")
			}
			source := filepath.Join(t.TempDir(), "ReadProperties.java")
			require.NoError(t, os.WriteFile(source, []byte(`
import java.io.*;
import java.util.Properties;
public class ReadProperties {
 public static void main(String[] args) throws Exception {
  Properties p = new Properties();
  try (InputStream in = new FileInputStream(args[0])) { p.load(in); }
  if (!args[1].equals(p.getProperty("motd"))) throw new AssertionError(p.getProperty("motd"));
  if (!"value".equals(p.getProperty("\ud83d\ude00"))) throw new AssertionError("emoji key changed");
  if (!"25565".equals(p.getProperty("server-port"))) throw new AssertionError("port changed");
 }
}`), 0o600))
			output, err := exec.Command("javac", source).CombinedOutput()
			require.NoError(t, err, "%s", output)
			output, err = exec.Command("java", "-cp", filepath.Dir(source), "ReadProperties", path, tc.value).CombinedOutput()
			require.NoError(t, err, "%s", output)
		})
	}
}
