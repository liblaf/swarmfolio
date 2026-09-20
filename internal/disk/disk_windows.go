//go:build windows

package disk

import (
	"errors"
	"fmt"
	"math"

	"golang.org/x/sys/windows"
)

func Probe(path string) (Space, error) {
	if path == "" {
		return Space{}, errors.New("disk: path is required")
	}
	directory, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return Space{}, fmt.Errorf("disk: encode path %q: %w", path, err)
	}
	var available, capacity uint64
	if err := windows.GetDiskFreeSpaceEx(directory, &available, &capacity, nil); err != nil {
		return Space{}, fmt.Errorf("disk: get free space for %q: %w", path, err)
	}
	if capacity > math.MaxInt64 || available > math.MaxInt64 {
		return Space{}, fmt.Errorf("disk: byte count for %q overflows int64", path)
	}
	return Space{CapacityBytes: int64(capacity), FreeBytes: int64(available)}, nil
}
