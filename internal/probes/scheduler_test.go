package probes

import (
	"testing"
	"time"
)

func TestProbeOffsetIsStable(t *testing.T) {
	interval := 30 * time.Second
	a := ProbeOffset("default", "api-health", interval)
	b := ProbeOffset("default", "api-health", interval)

	if a != b {
		t.Fatalf("expected stable offset, got %v and %v", a, b)
	}
	if a < 0 || a >= interval {
		t.Fatalf("offset %v out of range", a)
	}
}

func TestInitialDelayWithinInterval(t *testing.T) {
	now := time.Unix(1710000000, 123)
	delay := initialDelay(now, 30*time.Second, 5*time.Second)
	if delay <= 0 || delay > 30*time.Second {
		t.Fatalf("delay %v outside expected bounds", delay)
	}
}
