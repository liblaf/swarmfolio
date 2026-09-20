// Package disk reports capacity for the filesystem that qBittorrent writes to.
package disk

import (
	"errors"
	"math"
)

type Space struct {
	CapacityBytes int64
	FreeBytes     int64
}

func multiply(blocks, blockSize uint64) (int64, error) {
	if blockSize != 0 && blocks > math.MaxInt64/blockSize {
		return 0, errors.New("byte count overflows int64")
	}
	return int64(blocks * blockSize), nil
}
