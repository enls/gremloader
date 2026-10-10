package clock

import (
	"sync"
	"testing"
	"time"
)

func TestFakeIsFrozenUntilAdvanced(t *testing.T) {
	start := time.Date(2026, 3, 28, 1, 30, 0, 0, time.UTC)
	f := NewFake(start)

	if !f.Now().Equal(start) {
		t.Fatalf("Now() = %v, want %v", f.Now(), start)
	}
	time.Sleep(5 * time.Millisecond)
	if !f.Now().Equal(start) {
		t.Fatal("fake clock moved on its own")
	}

	f.Advance(90 * time.Minute)
	if want := start.Add(90 * time.Minute); !f.Now().Equal(want) {
		t.Errorf("after Advance, Now() = %v, want %v", f.Now(), want)
	}

	f.Advance(-30 * time.Minute)
	if want := start.Add(60 * time.Minute); !f.Now().Equal(want) {
		t.Errorf("after negative Advance, Now() = %v, want %v", f.Now(), want)
	}

	jump := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	f.Set(jump)
	if !f.Now().Equal(jump) {
		t.Errorf("after Set, Now() = %v, want %v", f.Now(), jump)
	}
}

func TestFakeConcurrentUse(t *testing.T) {
	f := NewFake(time.Unix(0, 0))
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); f.Advance(time.Second) }()
		go func() { defer wg.Done(); _ = f.Now() }()
	}
	wg.Wait()
	if got, want := f.Now(), time.Unix(50, 0); !got.Equal(want) {
		t.Errorf("Now() = %v, want %v", got, want)
	}
}

func TestSystemClockIsNow(t *testing.T) {
	before := time.Now()
	got := System{}.Now()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Errorf("System.Now() = %v, outside [%v, %v]", got, before, after)
	}
}

var _ Clock = System{}
var _ Clock = (*Fake)(nil)
