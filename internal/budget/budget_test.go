package budget

import (
	"math"
	"testing"
)

func TestCalculateReservesOneTiBAndOutstandingDownloads(t *testing.T) {
	t.Parallel()
	result, err := Calculate(Input{
		FreeBytes: 2 << 40, UsedBytes: 500,
		OutstandingBytes: 100, MinimumFreeBytes: 1 << 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequiredFreeBytes != 1<<40 || result.LimitBytes != (1<<40)+400 {
		t.Fatalf("result = %#v", result)
	}
}

func TestCalculateUsesHardLimitAndCanDemandDownsizing(t *testing.T) {
	t.Parallel()
	result, err := Calculate(Input{
		FreeBytes: 1 << 40, UsedBytes: 600,
		MinimumFreeBytes: 1 << 40, HardLimitBytes: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.LimitBytes != 500 {
		t.Fatalf("limit = %d, want 500", result.LimitBytes)
	}
}

func TestCalculateRejectsImpossibleInputs(t *testing.T) {
	t.Parallel()
	for _, input := range []Input{
		{FreeBytes: -1},
		{OutstandingBytes: -1},
		{MinimumFreeBytes: -1},
	} {
		if _, err := Calculate(input); err == nil {
			t.Fatalf("Calculate(%#v) succeeded", input)
		}
	}
}

func TestCalculateClampsExtremeReserveDeficitToZero(t *testing.T) {
	t.Parallel()
	result, err := Calculate(Input{
		FreeBytes: 0, UsedBytes: math.MaxInt64, OutstandingBytes: math.MaxInt64,
		MinimumFreeBytes: 1,
	})
	if err != nil || result.LimitBytes != 0 {
		t.Fatalf("Calculate() = %#v, %v; want zero limit", result, err)
	}
}

func TestCalculateDemandsDownsizingForReserveDeficit(t *testing.T) {
	t.Parallel()
	result, err := Calculate(Input{FreeBytes: 100, UsedBytes: 500, MinimumFreeBytes: 256})
	if err != nil || result.LimitBytes != 344 {
		t.Fatalf("Calculate() = %#v, %v; want 344-byte limit", result, err)
	}
}

func TestCalculateRejectsLimitOverflow(t *testing.T) {
	t.Parallel()
	_, err := Calculate(Input{FreeBytes: math.MaxInt64, UsedBytes: math.MaxInt64})
	if err == nil {
		t.Fatal("Calculate() accepted an overflowing limit")
	}
}
