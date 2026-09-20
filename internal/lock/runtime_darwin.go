//go:build darwin

package lock

import (
	"fmt"
	"os"
)

func runtimeDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("find user cache directory: %w", err)
	}
	return dir, nil
}
