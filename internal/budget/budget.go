// Package budget derives a safe logical torrent ceiling from physical disk
// space and qBittorrent's outstanding downloads.
package budget

import (
	"errors"
	"fmt"
	"math"
)

type Input struct {
	FreeBytes        int64
	UsedBytes        int64
	OutstandingBytes int64
	MinimumFreeBytes int64
	HardLimitBytes   int64
}

type Result struct {
	FreeBytes         int64 `json:"free_bytes"`
	UsedBytes         int64 `json:"used_bytes"`
	OutstandingBytes  int64 `json:"outstanding_bytes"`
	RequiredFreeBytes int64 `json:"required_free_bytes"`
	LimitBytes        int64 `json:"limit_bytes"`
}

// Calculate returns the maximum logical size of qBittorrent's portfolio that
// preserves the requested physical free space after every current download
// completes. Outstanding bytes are subtracted even when qBittorrent may have
// preallocated them; this conservative choice never spends promised headroom.
func Calculate(input Input) (Result, error) {
	if input.FreeBytes < 0 || input.UsedBytes < 0 || input.OutstandingBytes < 0 || input.MinimumFreeBytes < 0 || input.HardLimitBytes < 0 {
		return Result{}, errors.New("budget: byte counts cannot be negative")
	}

	limit := input.UsedBytes
	if input.FreeBytes > input.MinimumFreeBytes {
		spendable := input.FreeBytes - input.MinimumFreeBytes
		if spendable >= input.OutstandingBytes {
			addition := spendable - input.OutstandingBytes
			if limit > math.MaxInt64-addition {
				return Result{}, errors.New("budget: logical limit overflows int64")
			}
			limit += addition
		} else {
			limit = subtractFloor(limit, input.OutstandingBytes-spendable)
		}
	} else {
		limit = subtractFloor(limit, input.MinimumFreeBytes-input.FreeBytes)
		limit = subtractFloor(limit, input.OutstandingBytes)
	}
	if input.HardLimitBytes > 0 && limit > input.HardLimitBytes {
		limit = input.HardLimitBytes
	}
	return Result{
		FreeBytes: input.FreeBytes, UsedBytes: input.UsedBytes,
		OutstandingBytes: input.OutstandingBytes, RequiredFreeBytes: input.MinimumFreeBytes,
		LimitBytes: limit,
	}, nil
}

func subtractFloor(value, amount int64) int64 {
	if value <= amount {
		return 0
	}
	return value - amount
}
func Sum(values ...int64) (int64, error) {
	total := int64(0)
	for _, value := range values {
		if value < 0 {
			return 0, errors.New("budget: cannot sum a negative byte count")
		}
		if value > math.MaxInt64-total {
			return 0, fmt.Errorf("budget: byte total overflows int64")
		}
		total += value
	}
	return total, nil
}
