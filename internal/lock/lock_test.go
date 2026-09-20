package lock

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireSerializesAndReleases(t *testing.T) {
	configureTestRuntimeDir(t)
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	release := filepath.Join(dir, "release")
	command := exec.Command(os.Args[0], "-test.run=^TestLockHelperProcess$")
	command.Env = append(os.Environ(), "SWARMFOLIO_TEST_LOCK_HELPER=1", "SWARMFOLIO_TEST_LOCK_READY="+ready, "SWARMFOLIO_TEST_LOCK_RELEASE="+release)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatalf("start lock holder: %v", err)
	}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	if !waitForFile(ready) {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("lock holder did not become ready: %s", output.String())
	}
	if _, err := Acquire(); err == nil {
		t.Fatal("second lock acquisition succeeded")
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("lock holder failed: %v: %s", err, output.String())
	}
	second, err := Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLockHelperProcess(t *testing.T) {
	if os.Getenv("SWARMFOLIO_TEST_LOCK_HELPER") != "1" {
		return
	}
	lock, err := Acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lock.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := os.WriteFile(os.Getenv("SWARMFOLIO_TEST_LOCK_READY"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitForFile(os.Getenv("SWARMFOLIO_TEST_LOCK_RELEASE")) {
		t.Fatal("lock holder was not released")
	}
}

func waitForFile(path string) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
