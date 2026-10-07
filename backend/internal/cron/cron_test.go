package cron

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// A task runs repeatedly, never overlapping itself, through errors and
// panics, and stops with the context.
func TestSchedulerRunsTasks(t *testing.T) {
	var runs, overlapping, inRun atomic.Int32
	var s Scheduler
	s.Add(Task{Name: "steady", Every: 2 * time.Millisecond, Delay: time.Millisecond, Run: func(context.Context) error {
		if inRun.Add(1) > 1 {
			overlapping.Add(1)
		}
		defer inRun.Add(-1)
		time.Sleep(4 * time.Millisecond) // longer than its interval
		switch runs.Add(1) {
		case 1:
			return errors.New("first pass fails")
		case 2:
			panic("second pass panics")
		}
		return nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	for deadline := time.Now().Add(2 * time.Second); runs.Load() < 4 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run didn't return after cancel")
	}
	if runs.Load() < 4 {
		t.Fatalf("ran %d times, want it to keep going past an error and a panic", runs.Load())
	}
	if overlapping.Load() > 0 {
		t.Fatal("a task overlapped itself")
	}
	after := runs.Load()
	time.Sleep(10 * time.Millisecond)
	if runs.Load() != after {
		t.Fatal("a task ran after its context ended")
	}
}
