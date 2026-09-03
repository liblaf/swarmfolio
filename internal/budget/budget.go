// Package budget derives a safe logical torrent ceiling from physical disk
// space and qBittorrent's outstanding downloads.
package budget

import (
	"errors"
	"fmt"
	"math"
)

type Input struct {
	CapacityBytes      int64
	FreeBytes          int64
	UsedBytes          int64
	OutstandingBytes   int64
	MinimumFreePercent float64
	HardLimitBytes     int64
}

type Result struct {
	CapacityBytes      int64   `json:"capacity_bytes"`
	FreeBytes          int64   `json:"free_bytes"`
	UsedBytes          int64   `json:"used_bytes"`
	OutstandingBytes   int64   `json:"outstanding_bytes"`
	RequiredFreeBytes  int64   `json:"required_free_bytes"`
	MinimumFreePercent float64 `json:"minimum_free_percent"`
	LimitBytes         int64   `json:"limit_bytes"`
}

// Calculate returns the maximum logical size of qBittorrent's portfolio that
// preserves the requested physical free space after every current download
// completes. Outstanding bytes are subtracted even when qBittorrent may have
// preallocated them; this conservative choice never spends promised headroom.
func Calculate(input Input) (Result, error) {
	if input.CapacityBytes <= 0 {
		return Result{}, errors.New("budget: disk capacity must be positive")
	}
	if input.FreeBytes < 0 || input.FreeBytes > input.CapacityBytes {
		return Result{}, errors.New("budget: disk free space must be within capacity")
	}
	if input.UsedBytes < 0 || input.OutstandingBytes < 0 || input.HardLimitBytes < 0 {
		return Result{}, errors.New("budget: byte counts cannot be negative")
	}
	if input.MinimumFreePercent < 0 || input.MinimumFreePercent >= 100 || math.IsNaN(input.MinimumFreePercent) || math.IsInf(input.MinimumFreePercent, 0) {
		return Result{}, errors.New("budget: minimum free percent must be between 0 and 100")
	}
	required := int64(math.Ceil(float64(input.CapacityBytes) * input.MinimumFreePercent / 100))
	adjustment := input.FreeBytes - required - input.OutstandingBytes
	if adjustment > 0 && input.UsedBytes > math.MaxInt64-adjustment {
		return Result{}, errors.New("budget: logical limit overflows int64")
	}
	limit := input.UsedBytes + adjustment
	if limit < 0 {
		limit = 0
	}
	if input.HardLimitBytes > 0 && limit > input.HardLimitBytes {
		limit = input.HardLimitBytes
	}
	if limit > input.CapacityBytes {
		limit = input.CapacityBytes
	}
	return Result{
		CapacityBytes: input.CapacityBytes, FreeBytes: input.FreeBytes,
		UsedBytes: input.UsedBytes, OutstandingBytes: input.OutstandingBytes,
		RequiredFreeBytes: required, MinimumFreePercent: input.MinimumFreePercent,
		LimitBytes: limit,
	}, nil
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
