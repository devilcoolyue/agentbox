package server

import (
	"testing"
	"time"

	"agentbox/internal/dockerx"
)

func TestProcCPUPercent(t *testing.T) {
	// 100 ticks == 1s CPU (clkTck=100); over a 2s window that is 50% of one core.
	if got := procCPUPercent(1000, 1100, 2*time.Second); got != 50 {
		t.Fatalf("want 50, got %v", got)
	}
	// A full core busy for the whole window is 100%.
	if got := procCPUPercent(0, 100, time.Second); got != 100 {
		t.Fatalf("want 100, got %v", got)
	}
	// Guards: zero window and counter going backwards both yield 0.
	if got := procCPUPercent(1000, 1100, 0); got != 0 {
		t.Fatalf("zero window want 0, got %v", got)
	}
	if got := procCPUPercent(1100, 1000, time.Second); got != 0 {
		t.Fatalf("backwards want 0, got %v", got)
	}
}

func TestHostCPUPercent(t *testing.T) {
	// 30 busy of 100 total jiffies over the window == 30%.
	a := cpuTimes{busy: 100, total: 1000}
	b := cpuTimes{busy: 130, total: 1100}
	if got := hostCPUPercent(a, b); got != 30 {
		t.Fatalf("want 30, got %v", got)
	}
	// No total advance (or counter reset) yields 0 rather than dividing by zero.
	if got := hostCPUPercent(a, a); got != 0 {
		t.Fatalf("no advance want 0, got %v", got)
	}
}

func TestContainerCPUPercent(t *testing.T) {
	base := time.Now()
	// 0.5s of CPU nanoseconds over a 1s wall window == 50% of one core.
	a := dockerx.RawStat{CPUTotal: 0, Read: base, OK: true}
	b := dockerx.RawStat{CPUTotal: uint64(500 * time.Millisecond), Read: base.Add(time.Second), OK: true}
	if got := containerCPUPercent(a, b); got != 50 {
		t.Fatalf("want 50, got %v", got)
	}
	// Two cores fully busy for the window reads as 200%.
	b2 := dockerx.RawStat{CPUTotal: uint64(2 * time.Second), Read: base.Add(time.Second), OK: true}
	if got := containerCPUPercent(a, b2); got != 200 {
		t.Fatalf("want 200, got %v", got)
	}
	// Same read time (zero wall) must not divide by zero.
	if got := containerCPUPercent(a, dockerx.RawStat{CPUTotal: 100, Read: base}); got != 0 {
		t.Fatalf("zero wall want 0, got %v", got)
	}
}
