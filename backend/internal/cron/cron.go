// Package cron runs the server's periodic background tasks: each is a
// named function called on a fixed interval for as long as the server
// runs. One place for "check every so often" work, so each task doesn't
// carry its own ticker loop, logging and panic handling.
//
// A task never overlaps itself - a run that takes longer than its interval
// delays the next one rather than stacking - and tasks don't wait on each
// other. Nothing is persisted: a task's schedule starts over with the
// server, so a task must work out what is due from stored state, not from
// how many times it has run.
package cron

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// Task is one periodic job.
type Task struct {
	// Name identifies the task in logs.
	Name string
	// Every is the time from the end of one run to the start of the next.
	Every time.Duration
	// Delay is how long after Run starts the first run happens; zero means
	// one Every in.
	Delay time.Duration
	// Run does one pass. Its error is logged; the task runs again on
	// schedule either way. It should return promptly when ctx ends.
	Run func(ctx context.Context) error
}

// Scheduler holds the tasks to run. The zero value is ready to use.
type Scheduler struct {
	tasks []Task
	// SlowAfter is the run time past which a pass is logged as slow; zero
	// means a minute.
	SlowAfter time.Duration
}

// Add registers t. Call before Run.
func (s *Scheduler) Add(t Task) {
	if t.Every <= 0 {
		panic(fmt.Sprintf("cron: task %q has no interval", t.Name))
	}
	s.tasks = append(s.tasks, t)
}

// Run runs every registered task on its schedule until ctx ends, then
// waits for the passes in flight to return.
func (s *Scheduler) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, t := range s.tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.loop(ctx, t)
		}()
	}
	wg.Wait()
}

func (s *Scheduler) loop(ctx context.Context, t Task) {
	wait := t.Delay
	if wait <= 0 {
		wait = t.Every
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.runOnce(ctx, t)
		timer.Reset(t.Every)
	}
}

// runOnce does one pass of t, logging a failure, a panic or a slow run.
func (s *Scheduler) runOnce(ctx context.Context, t Task) {
	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("cron: %s panicked: %v", t.Name, r)
		}
	}()
	err := t.Run(ctx)
	slow := s.SlowAfter
	if slow <= 0 {
		slow = time.Minute
	}
	switch took := time.Since(start); {
	case err != nil && ctx.Err() == nil:
		log.Printf("cron: %s: %v", t.Name, err)
	case took > slow:
		log.Printf("cron: %s took %s", t.Name, took.Round(time.Millisecond))
	}
}
