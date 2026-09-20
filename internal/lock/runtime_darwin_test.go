//go:build darwin

package lock

import "testing"

func configureTestRuntimeDir(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}
