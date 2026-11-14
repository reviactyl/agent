package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTextFileMatchBoundaries(t *testing.T) {
	tests := []struct{ name, match, input, want string }{
		{"equals", "server-port", "server-port=25565\nserver-port-extra=25566\n", "updated\nserver-port-extra=25566\n"},
		{"colon", "server-port", "server-port: 25565\nserver-port-extra: 25566\n", "updated\nserver-port-extra: 25566\n"},
		{"whitespace", "server-port", "server-port 25565\nserver-port\t25565\nserver-port_extra=25566\n", "updated\nupdated\nserver-port_extra=25566\n"},
		{"exact line", "server-port", "server-port\nserver-ports\nserver-port2\nserver-port.extra\nserver-port/extra\n", "updated\nserver-ports\nserver-port2\nserver-port.extra\nserver-port/extra\n"},
		{"equals prefix", "server-port=", "server-port=25565\nserver-port-extra=25566\n", "updated\nserver-port-extra=25566\n"},
		{"colon prefix", "server-port:", "server-port:25565\nserver-port-extra:25566\n", "updated\nserver-port-extra:25566\n"},
		{"space prefix", "set port ", "set port 25565\nset ports 25566\n", "updated\nset ports 25566\n"},
		{"double quoted prefix", `hostname "`, "hostname \"Old server\"\nhostname-extra \"unchanged\"\n", "updated\nhostname-extra \"unchanged\"\n"},
		{"single quoted prefix", "hostname '", "hostname 'Old server'\nhostname-extra 'unchanged'\n", "updated\nhostname-extra 'unchanged'\n"},
		{"tab prefix", "set port\t", "set port\t25565\n", "updated\n"},
		{"literal metacharacters", "server.port[0]", "server.port[0]=1\nserverXport0=2\nserver.port[0]-extra=3\n", "updated\nserverXport0=2\nserver.port[0]-extra=3\n"},
		{"literal regex prefix", "regex:^server-port", "server-port=1\nregex:^server-port=2\n", "server-port=1\nupdated\n"},
		{"empty prefix", "", "one\ntwo\n", "updated\nupdated\n"},
		{"formatting", "server-port", "# comment\r\n\r\n  server-port=1\r\nserver-port=2\r\nkeep = value", "# comment\n\n  server-port=1\nupdated\nkeep = value\n"},
		{"empty file", "server-port", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			require.NoError(t, os.WriteFile(path, []byte(tt.input), 0o600))
			file, err := os.OpenFile(path, os.O_RDWR, 0)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, file.Close()) })
			r := replacement(t, `{"match":"ignored","replace_with":"updated"}`)
			r.Match = tt.match
			f := ConfigurationFile{Replace: []ConfigurationFileReplacement{r}}
			require.NoError(t, f.parseTextFile(file))
			output, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, tt.want, string(output))
		})
	}
}
