//go:build linux

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
