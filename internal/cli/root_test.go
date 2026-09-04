package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigPathAndInitUseXDG(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	var stdout, stderr bytes.Buffer
	command := New(&stdout, &stderr)
	command.SetArgs([]string{"config", "path"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "swarmfolio", "config.toml")
	if strings.TrimSpace(stdout.String()) != want {
		t.Fatalf("path output = %q, want %q", stdout.String(), want)
	}
	stdout.Reset()
	command = New(&stdout, &stderr)
	command.SetArgs([]string{"config", "init"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(want)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
	}
	if err := os.Chmod(want, 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	command = New(&stdout, &stderr)
	command.SetArgs([]string{"config", "init", "--force"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(want)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("forced config mode = %o, want 600", info.Mode().Perm())
	}
}

func TestFishCompletion(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	command := New(&stdout, &stderr)
	command.SetArgs([]string{"completion", "fish"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "swarmfolio") || !strings.Contains(stdout.String(), "__swarmfolio") {
		t.Fatal("generated output does not look like Fish completion")
	}
}

func TestSystemdPrint(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	command := New(&stdout, &stderr)
	command.SetArgs([]string{"systemd", "print", "timer"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "OnCalendar=hourly") {
		t.Fatalf("timer output = %q", stdout.String())
	}
}

func TestSystemdPrintServiceHasNoEnvironmentFile(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	command := New(&stdout, &stderr)
	command.SetArgs([]string{"systemd", "print", "service"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	service := stdout.String()
	if !strings.Contains(service, "ExecStart=") {
		t.Fatalf("service output has no ExecStart: %q", service)
	}
	if strings.Contains(service, "EnvironmentFile=") {
		t.Fatalf("service output unexpectedly contains EnvironmentFile: %q", service)
	}
}

func TestSystemdInstallUsesXDGConfigHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	var stdout, stderr bytes.Buffer
	command := New(&stdout, &stderr)
	command.SetArgs([]string{"systemd", "install"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"swarmfolio.service", "swarmfolio.timer"} {
		path := filepath.Join(dir, "systemd", "user", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read installed %s: %v", name, err)
		}
		if !strings.Contains(string(data), "[Unit]") {
			t.Fatalf("installed %s is not a systemd unit: %q", name, data)
		}
	}
}
