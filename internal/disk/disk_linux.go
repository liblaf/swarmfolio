//go:build linux

// Package disk reports capacity for the filesystem that qBittorrent writes to.
package disk

import (
	"errors"
	"fmt"
	"math"
	"syscall"
)

type Space struct {
	CapacityBytes int64
	FreeBytes     int64
}

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

func multiply(blocks, blockSize uint64) (int64, error) {
	if blockSize != 0 && blocks > math.MaxInt64/blockSize {
		return 0, errors.New("byte count overflows int64")
	}
	return int64(blocks * blockSize), nil
}
