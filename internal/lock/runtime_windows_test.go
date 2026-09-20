//go:build windows

package lock

import "testing"

func configureTestRuntimeDir(t *testing.T) {
	t.Helper()
	t.Setenv("LocalAppData", t.TempDir())
}
