package monitor

import (
	"sync"
	"testing"
)

func TestCompletionRPM_Basic(t *testing.T) {
	c := NewCompletionRPM()

	if c.CurrentRPM("b1") != 0 {
		t.Error("initial RPM should be 0")
	}

	c.Record("b1")
	c.Record("b1")
	c.Record("b1")

	if c.CurrentRPM("b1") != 3 {
		t.Errorf("expected RPM 3, got %d", c.CurrentRPM("b1"))
	}

	// Different backend
	c.Record("b2")
	if c.CurrentRPM("b2") != 1 {
		t.Errorf("expected RPM 1 for b2, got %d", c.CurrentRPM("b2"))
	}
}

func TestCompletionRPM_AllRPMs(t *testing.T) {
	c := NewCompletionRPM()

	c.Record("b1")
	c.Record("b1")
	c.Record("b2")

	rpms := c.AllRPMs()
	if rpms["b1"] != 2 {
		t.Errorf("b1: expected 2, got %d", rpms["b1"])
	}
	if rpms["b2"] != 1 {
		t.Errorf("b2: expected 1, got %d", rpms["b2"])
	}
}

func TestCompletionRPM_UnknownBackend(t *testing.T) {
	c := NewCompletionRPM()
	if c.CurrentRPM("nonexistent") != 0 {
		t.Error("unknown backend should return 0")
	}
}

func TestCompletionRPM_Concurrent(t *testing.T) {
	c := NewCompletionRPM()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Record("b1")
		}()
	}
	wg.Wait()

	if c.CurrentRPM("b1") != 100 {
		t.Errorf("expected 100, got %d", c.CurrentRPM("b1"))
	}
}
