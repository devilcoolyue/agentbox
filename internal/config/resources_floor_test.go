package config

import "testing"

func TestResourceFloorNeverExceedsTheReserve(t *testing.T) {
	for _, tc := range []struct{ reserve, floor, low int64 }{
		{0, 0, 0},
		{300 << 20, 300 << 20, 600 << 20},
		{3 << 30, 1 << 30, 6 << 30},
	} {
		limits := ResourceLimits{MinFreeBytes: tc.reserve}
		if limits.FloorBytes() != tc.floor || limits.LowWaterBytes() != tc.low {
			t.Fatalf("reserve %d: floor %d, low water %d", tc.reserve, limits.FloorBytes(), limits.LowWaterBytes())
		}
	}
}
