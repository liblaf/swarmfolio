package disk

import "testing"

func TestProbe(t *testing.T) {
	t.Parallel()
	space, err := Probe(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if space.CapacityBytes <= 0 || space.FreeBytes < 0 || space.FreeBytes > space.CapacityBytes {
		t.Fatalf("invalid space: %#v", space)
	}
}

func TestProbeRequiresPath(t *testing.T) {
	t.Parallel()
	if _, err := Probe(""); err == nil {
		t.Fatal("Probe(\"\") succeeded")
	}
}

func TestMultiply(t *testing.T) {
	t.Parallel()
	value, err := multiply(2, 3)
	if err != nil || value != 6 {
		t.Fatalf("multiply(2, 3) = %d, %v", value, err)
	}
	if _, err := multiply(^uint64(0), 2); err == nil {
		t.Fatal("overflow was accepted")
	}
}
