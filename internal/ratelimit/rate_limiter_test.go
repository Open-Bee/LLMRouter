package ratelimit

import (
	"sync"
	"testing"
)

func intPtr(v int) *int { return &v }

func TestRateLimiter_NilLimit_AlwaysAllows(t *testing.T) {
	rl := NewRateLimiter()

	for i := 0; i < 1000; i++ {
		if !rl.Allow("backend1", nil) {
			t.Fatal("nil limit should always allow")
		}
	}
}

func TestRateLimiter_IsLimited_NilLimit(t *testing.T) {
	rl := NewRateLimiter()
	if rl.IsLimited("b", nil) {
		t.Error("nil limit should never be limited")
	}
}

func TestRateLimiter_EnforcesLimit(t *testing.T) {
	rl := NewRateLimiter()
	limit := intPtr(5)

	for i := 0; i < 5; i++ {
		if !rl.Allow("b1", limit) {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}

	// 6th should be denied
	if rl.Allow("b1", limit) {
		t.Error("6th request should be denied")
	}

	if !rl.IsLimited("b1", limit) {
		t.Error("should be limited at 5/5")
	}
}

func TestRateLimiter_CurrentRPM(t *testing.T) {
	rl := NewRateLimiter()
	limit := intPtr(100)

	if rl.CurrentRPM("b1") != 0 {
		t.Error("initial RPM should be 0")
	}

	rl.Allow("b1", limit)
	rl.Allow("b1", limit)
	rl.Allow("b1", limit)

	if rl.CurrentRPM("b1") != 3 {
		t.Errorf("expected RPM 3, got %d", rl.CurrentRPM("b1"))
	}
}

func TestRateLimiter_Record(t *testing.T) {
	rl := NewRateLimiter()

	rl.Record("b1")
	rl.Record("b1")

	if rl.CurrentRPM("b1") != 2 {
		t.Errorf("expected RPM 2 after Record, got %d", rl.CurrentRPM("b1"))
	}
}

func TestRateLimiter_IndependentBackends(t *testing.T) {
	rl := NewRateLimiter()
	limit := intPtr(2)

	rl.Allow("b1", limit)
	rl.Allow("b1", limit)
	// b1 is now at limit

	// b2 should still be allowed
	if !rl.Allow("b2", limit) {
		t.Error("b2 should be independent from b1")
	}
}

func TestRateLimiter_RemoveBackend(t *testing.T) {
	rl := NewRateLimiter()
	limit := intPtr(2)

	rl.Allow("b1", limit)
	rl.Allow("b1", limit)

	rl.RemoveBackend("b1")

	if rl.CurrentRPM("b1") != 0 {
		t.Error("RPM should be 0 after removal")
	}

	// Should be allowed again after removal
	if !rl.Allow("b1", limit) {
		t.Error("should be allowed after removal")
	}
}

func TestRateLimiter_IsLimited_NoWindow(t *testing.T) {
	rl := NewRateLimiter()
	limit := intPtr(10)

	// Backend not yet seen
	if rl.IsLimited("unknown", limit) {
		t.Error("unknown backend should not be limited")
	}
}

func TestRateLimiter_ConcurrentAccess(t *testing.T) {
	rl := NewRateLimiter()
	limit := intPtr(10000) // high limit to not block

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				rl.Allow("b1", limit)
			}
		}()
	}
	wg.Wait()

	rpm := rl.CurrentRPM("b1")
	if rpm != 10000 {
		t.Errorf("expected 10000, got %d", rpm)
	}
}
