//go:build linux || darwin

package disk

import (
	"errors"
	"fmt"
	"syscall"
)

func Probe(path string) (Space, error) {
	if path == "" {
		return Space{}, errors.New("disk: path is required")
	}
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil {
		return Space{}, fmt.Errorf("disk: statfs %q: %w", path, err)
	}
	capacity, err := multiply(stats.Blocks, uint64(stats.Bsize))
	if err != nil {
		return Space{}, fmt.Errorf("disk: capacity for %q: %w", path, err)
	}
	free, err := multiply(stats.Bavail, uint64(stats.Bsize))
	if err != nil {
		return Space{}, fmt.Errorf("disk: free space for %q: %w", path, err)
	}
	return Space{CapacityBytes: capacity, FreeBytes: free}, nil
}
