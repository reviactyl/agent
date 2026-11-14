package server

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reviactyl/agent/config"
	"github.com/reviactyl/agent/parser"
	"github.com/reviactyl/agent/remote"
	"github.com/reviactyl/agent/server/filesystem"
)

func newConfigParserServer(t *testing.T, diskLimit int64, raw string) (*Server, string) {
	t.Helper()
	cfg, err := config.NewAtPath(filepath.Join(t.TempDir(), "config.yml"))
	require.NoError(t, err)
	cfg.AuthenticationToken = "test-token"
	cfg.System.User.Uid = os.Getuid()
	cfg.System.User.Gid = os.Getgid()
	config.Set(cfg)

	root := filepath.Join(t.TempDir(), "server")
	require.NoError(t, os.Mkdir(root, 0o755))
	fs, err := filesystem.New(root, diskLimit, nil)
	require.NoError(t, err)

	var cf parser.ConfigurationFile
	require.NoError(t, json.Unmarshal([]byte(raw), &cf))

	s, err := New(nil)
	require.NoError(t, err)
	s.fs = fs
	s.procConfig = &remote.ProcessConfiguration{ConfigurationFiles: []parser.ConfigurationFile{cf}}
	return s, root
}

// Rewriting a configuration file counts against the server's disk limit, so a
// rewrite that would grow the file past it is refused.
func TestConfigurationFilesRespectDiskLimit(t *testing.T) {
	raw := `{"file":"log.txt","parser":"file","replace":[{"match":"k","replace_with":"` + strings.Repeat("x", 1024) + `"}]}`
	s, root := newConfigParserServer(t, 1<<20, raw)
	original := bytes.Repeat([]byte("k\n"), 32768)
	require.NoError(t, os.WriteFile(filepath.Join(root, "log.txt"), original, 0o644))

	_, err := s.Filesystem().DiskUsage(false)
	require.NoError(t, err)

	s.UpdateConfigurationFiles()

	// The file is left as it was rather than partly rewritten.
	b, err := os.ReadFile(filepath.Join(root, "log.txt"))
	require.NoError(t, err)
	require.Equal(t, original, b, "the configuration file grew past the disk limit")
	require.LessOrEqual(t, s.Filesystem().CachedUsage(), int64(1<<20))
}

// Within the disk limit, configuration files are rewritten and counted.
func TestConfigurationFilesAreUpdated(t *testing.T) {
	raw := `{"file":"server.properties","parser":"file","replace":[{"match":"port=","replace_with":"port=25565"}]}`
	s, root := newConfigParserServer(t, 0, raw)
	require.NoError(t, os.WriteFile(filepath.Join(root, "server.properties"), []byte("motd=hi\nport=1\n"), 0o644))

	s.UpdateConfigurationFiles()

	b, err := os.ReadFile(filepath.Join(root, "server.properties"))
	require.NoError(t, err)
	require.Equal(t, "motd=hi\nport=25565\n", string(b))
}

func TestConfigurationFilesExpandEverySupportedPlaceholder(t *testing.T) {
	raw := `{"file":"config.json","parser":"json","replace":[{"match":"bind","replace_with":"prefix {{config.missing}} [{{ config.docker.interface }}:{{config.docker.network.interface}}:{{config.docker.interface}}] {{config.authentication_token}} suffix"}]}`
	s, root := newConfigParserServer(t, 0, raw)
	path := filepath.Join(root, "config.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"bind":"original","keep":"untouched"}`), 0o600))

	s.UpdateConfigurationFiles()

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	var got map[string]string
	require.NoError(t, json.Unmarshal(content, &got))
	address := config.Get().Docker.Network.Interface
	require.NotEmpty(t, address)
	require.Equal(t, "prefix {{config.missing}} ["+address+":"+address+":"+address+"] {{config.authentication_token}} suffix", got["bind"])
	require.Equal(t, "untouched", got["keep"])
	require.NotContains(t, string(content), "test-token")
}
