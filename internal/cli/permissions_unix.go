//go:build !windows

package cli

import "os"

func setFilePermissions(file *os.File, mode os.FileMode) error {
	return file.Chmod(mode)
}
