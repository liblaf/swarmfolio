//go:build !windows

package config

import (
	"fmt"
	"os"
)

func checkConfigPermissions(file *os.File, info os.FileInfo) error {
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("config %q contains credentials and must not be accessible by group or others (mode %04o)", file.Name(), info.Mode().Perm())
	}
	return nil
}
