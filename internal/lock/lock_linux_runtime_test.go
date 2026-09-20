//go:build linux

package lock

import "testing"

func TestAcquireRequiresAbsoluteRuntimeDirectory(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "relative")
	if _, err := Acquire(); err == nil {
		t.Fatal("Acquire succeeded with a relative XDG_RUNTIME_DIR")
	}
}
