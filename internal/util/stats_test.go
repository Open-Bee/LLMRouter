package util

import (
	"sync"
	"testing"
	"time"
)

func TestRequestStats_Basic(t *testing.T) {
	s := NewRequestStats()

	if s.Total() != 0 || s.Success() != 0 || s.Failures() != 0 {
		t.Error("initial counts should be zero")
	}

	s.RecordSuccess("b1", 100*time.Millisecond)
	s.RecordSuccess("b1", 200*time.Millisecond)
	s.RecordFailure("b1", "timeout")

	if s.Total() != 3 {
		t.Errorf("total: expected 3, got %d", s.Total())
	}
	if s.Success() != 2 {
		t.Errorf("success: expected 2, got %d", s.Success())
	}
	if s.Failures() != 1 {
		t.Errorf("failures: expected 1, got %d", s.Failures())
	}
}

func TestRequestStats_BackendStats(t *testing.T) {
	s := NewRequestStats()

	s.RecordSuccess("b1", 100*time.Millisecond)
	s.RecordSuccess("b1", 300*time.Millisecond)
	s.RecordFailure("b2", "5xx")

	bs1 := s.GetBackendStats("b1")
	if bs1 == nil {
		t.Fatal("expected stats for b1")
	}
	if bs1.Total.Load() != 2 {
		t.Errorf("b1 total: expected 2, got %d", bs1.Total.Load())
	}
	if bs1.Success.Load() != 2 {
		t.Errorf("b1 success: expected 2, got %d", bs1.Success.Load())
	}

	avgLatency := bs1.AvgLatency()
	if avgLatency != 200.0 {
		t.Errorf("b1 avg latency: expected 200, got %f", avgLatency)
	}

	bs2 := s.GetBackendStats("b2")
	if bs2 == nil {
		t.Fatal("expected stats for b2")
	}
	if bs2.Failures.Load() != 1 {
		t.Errorf("b2 failures: expected 1, got %d", bs2.Failures.Load())
	}

	// Unknown backend
	if s.GetBackendStats("unknown") != nil {
		t.Error("should return nil for unknown")
	}
}

func TestRequestStats_AllBackendStats(t *testing.T) {
	s := NewRequestStats()
	s.RecordSuccess("b1", time.Millisecond)
	s.RecordFailure("b2", "err")

	all := s.AllBackendStats()
	if len(all) != 2 {
		t.Errorf("expected 2 backends, got %d", len(all))
	}
}

func TestRequestStats_Uptime(t *testing.T) {
	s := NewRequestStats()
	time.Sleep(10 * time.Millisecond)
	if s.Uptime() < 10*time.Millisecond {
		t.Error("uptime should be at least 10ms")
	}
}

func TestBackendStats_AvgLatency_NoData(t *testing.T) {
	bs := &BackendStats{}
	if bs.AvgLatency() != 0 {
		t.Error("avg latency with no data should be 0")
	}
}

func TestRequestStats_Concurrent(t *testing.T) {
	s := NewRequestStats()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			s.RecordSuccess("b1", time.Millisecond)
		}()
		go func() {
			defer wg.Done()
			s.RecordFailure("b1", "err")
		}()
	}
	wg.Wait()

	if s.Total() != 200 {
		t.Errorf("expected 200 total, got %d", s.Total())
	}
	if s.Success() != 100 {
		t.Errorf("expected 100 success, got %d", s.Success())
	}
	if s.Failures() != 100 {
		t.Errorf("expected 100 failures, got %d", s.Failures())
	}
}
