package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNextDelay(t *testing.T) {
	iv := 60 * time.Second
	tests := []struct {
		jitter, r float64
		want      time.Duration
	}{
		{0, 0.9, 60 * time.Second},
		{0.1, 0, 54 * time.Second},
		{0.1, 0.5, 60 * time.Second},
		{0.25, 0.999999, 75 * time.Second},
	}
	for _, tc := range tests {
		got := NextDelay(iv, tc.jitter, tc.r)
		if d := got - tc.want; d > time.Millisecond || d < -time.Millisecond {
			t.Errorf("NextDelay(60s, %v, %v) = %s, want %s", tc.jitter, tc.r, got, tc.want)
		}
	}
}

func TestSchedulerBoundsConcurrencyAndRunsEveryJob(t *testing.T) {
	const workers = 2
	var inFlight, peak atomic.Int32
	var mu sync.Mutex
	runs := map[string]int{}

	work := func(id string) func(context.Context) {
		return func(context.Context) {
			n := inFlight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			inFlight.Add(-1)
			mu.Lock()
			runs[id]++
			mu.Unlock()
		}
	}
	var jobs []Job
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		jobs = append(jobs, Job{ID: id, Interval: 10 * time.Millisecond, Jitter: 0.2, Run: work(id)})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		New(workers, nil).Run(ctx, jobs)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	if p := peak.Load(); p > workers {
		t.Fatalf("peak concurrency = %d, want <= %d", p, workers)
	}
	if inFlight.Load() != 0 {
		t.Fatal("runs still in flight after Run returned")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, j := range jobs {
		if runs[j.ID] < 2 {
			t.Errorf("job %s ran %d times, want >= 2", j.ID, runs[j.ID])
		}
	}
}

func TestSchedulerAppliesTimeoutAndRecoversPanics(t *testing.T) {
	var sawDeadline, afterPanic atomic.Bool
	jobs := []Job{
		{ID: "slow", Interval: time.Hour, Timeout: 20 * time.Millisecond, Run: func(ctx context.Context) {
			if _, ok := ctx.Deadline(); ok {
				<-ctx.Done()
				sawDeadline.Store(true)
			}
		}},
		{ID: "boom", Interval: 5 * time.Millisecond, Run: func(context.Context) {
			if afterPanic.Swap(true) {
				return
			}
			panic("probe exploded")
		}},
	}
	s := New(2, nil)
	s.rand = func() float64 { return 0 }
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	s.Run(ctx, jobs)

	if !sawDeadline.Load() {
		t.Error("job did not receive a deadline from Timeout")
	}
	if !afterPanic.Load() {
		t.Error("panicking job was not run")
	}
}
