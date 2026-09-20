//go:build linux

package lock

import (
	"errors"
	"os"
	"path/filepath"
)

func runtimeDir() (string, error) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" || !filepath.IsAbs(dir) {
		return "", errors.New("XDG_RUNTIME_DIR must be set to an absolute path for an applied run")
	}
	return dir, nil
}
