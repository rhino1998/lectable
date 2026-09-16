package audioworker

import (
	"sync"
	"testing"
	"time"
)

// TestFCFSMutexOrdering verifies fcfsMutex actually grants access in
// arrival order, the one property a plain sync.Mutex doesn't guarantee
// under contention (see fcfsMutex's own doc comment) - the whole reason
// Worker.runMu uses this type instead. Goroutines are spawned one at a
// time with a small delay between each, so each one's own Lock() call
// reliably lands (and queues) before the next is even spawned - not a
// mathematical guarantee against pathological scheduling, but reliable in
// practice and enough to catch a real ordering bug.
func TestFCFSMutexOrdering(t *testing.T) {
	var f fcfsMutex
	f.Lock() // held up front so every waiter below actually queues

	const n = 20
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	wg.Add(n)

	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			f.Lock()
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			f.Unlock()
		}(i)
		time.Sleep(2 * time.Millisecond) // let goroutine i's Lock() actually queue before spawning i+1
	}

	time.Sleep(10 * time.Millisecond) // let every spawned goroutine reach and queue on Lock()
	f.Unlock()                        // release the initial hold, kicking off the FCFS chain
	wg.Wait()

	if len(order) != n {
		t.Fatalf("got %d completions, want %d", len(order), n)
	}
	for i, v := range order {
		if v != i {
			t.Fatalf("order[%d] = %d, want %d (not FCFS): %v", i, v, i, order)
		}
	}
}

// TestFCFSMutexMutualExclusion is a plain "actually excludes concurrent
// access" sanity check (run with -race) - fcfsMutex's ordering guarantee
// is worthless if it doesn't still do the one thing a mutex is for.
func TestFCFSMutexMutualExclusion(t *testing.T) {
	var f fcfsMutex
	counter := 0
	var wg sync.WaitGroup
	const n = 200
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			f.Lock()
			counter++
			f.Unlock()
		}()
	}
	wg.Wait()
	if counter != n {
		t.Fatalf("counter = %d, want %d", counter, n)
	}
}
