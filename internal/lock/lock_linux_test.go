//go:build linux

package lock

import "testing"

func TestAcquireSerializesAndReleases(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	first, err := Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(); err == nil {
		t.Fatal("second lock acquisition succeeded")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire()
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}
