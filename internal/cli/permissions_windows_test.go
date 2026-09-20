//go:build windows

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liblaf/swarmfolio/internal/config"
)

func TestConfigInitProtectsCredentialsWithWindowsACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	var stdout, stderr bytes.Buffer
	command := New(&stdout, &stderr)
	command.SetArgs([]string{"--config", path, "config", "init"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `api_key = ""`, `api_key = "mteam-secret"`, 1))
	data = []byte(strings.Replace(string(data), `api_key = ""`, `api_key = "qbt-secret"`, 1))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path); err != nil {
		t.Fatalf("Load(config initialized with Windows ACL) = %v", err)
	}
}
