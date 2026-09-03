package budget

import "testing"

func TestCalculateReservesDiskAndOutstandingDownloads(t *testing.T) {
	t.Parallel()
	result, err := Calculate(Input{
		CapacityBytes: 1000, FreeBytes: 400, UsedBytes: 500,
		OutstandingBytes: 100, MinimumFreePercent: 25,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Once the outstanding 100 bytes finish, only 50 of today's free space is
	// spendable while preserving 250 bytes. The logical ceiling is therefore 550.
	if result.RequiredFreeBytes != 250 || result.LimitBytes != 550 {
		t.Fatalf("result = %#v", result)
	}
}

func TestCalculateUsesHardLimitAndCanDemandDownsizing(t *testing.T) {
	t.Parallel()
	result, err := Calculate(Input{
		CapacityBytes: 1000, FreeBytes: 200, UsedBytes: 600,
		MinimumFreePercent: 25, HardLimitBytes: 500,
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
		{},
		{CapacityBytes: 10, FreeBytes: 11},
		{CapacityBytes: 10, MinimumFreePercent: 100},
		{CapacityBytes: 10, OutstandingBytes: -1},
	} {
		if _, err := Calculate(input); err == nil {
			t.Fatalf("Calculate(%#v) succeeded", input)
		}
	}
}
