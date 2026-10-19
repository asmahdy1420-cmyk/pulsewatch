// Package scheduler runs jobs at per-job intervals with jitter on a bounded
// worker pool.
package scheduler

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"
)

// Job is a periodic unit of work.
type Job struct {
	ID       string
	Interval time.Duration
	// Jitter is the fraction (0..1) of Interval by which each wait is
	// randomly shortened or lengthened.
	Jitter float64
	// Timeout bounds a single run; zero means no extra deadline.
	Timeout time.Duration
	Run     func(ctx context.Context)
}

// Scheduler runs jobs. Each job has at most one run in flight, and at most
// Workers runs execute concurrently across all jobs; a job whose turn comes
// while the pool is saturated waits for a free worker rather than piling
// up runs.
type Scheduler struct {
	Workers int
	Log     *slog.Logger
	// rand returns a value in [0,1); replaceable in tests.
	rand func() float64
}

// New returns a Scheduler with the given pool size (minimum 1).
func New(workers int, log *slog.Logger) *Scheduler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Scheduler{Workers: max(workers, 1), Log: log, rand: rand.Float64}
}

// NextDelay returns interval scaled by a uniform factor in
// [1-jitter, 1+jitter), given r in [0,1).
func NextDelay(interval time.Duration, jitter, r float64) time.Duration {
	if jitter <= 0 {
		return interval
	}
	f := 1 - jitter + 2*jitter*r
	return time.Duration(float64(interval) * f)
}

type task struct {
	job  Job
	done chan struct{}
}

// Run blocks until ctx is cancelled and every in-flight run has returned.
func (s *Scheduler) Run(ctx context.Context, jobs []Job) {
	work := make(chan task)
	var workers sync.WaitGroup
	for range s.Workers {
		workers.Go(func() {
			for t := range work {
				s.exec(ctx, t.job)
				close(t.done)
			}
		})
	}

	var loops sync.WaitGroup
	for _, j := range jobs {
		loops.Go(func() { s.loop(ctx, j, work) })
	}
	loops.Wait()
	close(work)
	workers.Wait()
}

func (s *Scheduler) loop(ctx context.Context, j Job, work chan<- task) {
	// Spread the first runs over a fraction of the interval so a restart
	// does not fire every check at once.
	first := time.Duration(float64(j.Interval) * max(j.Jitter, 0.1) * s.rand())
	timer := time.NewTimer(first)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		t := task{job: j, done: make(chan struct{})}
		select {
		case <-ctx.Done():
			return
		case work <- t:
		}
		<-t.done // the worker always finishes; run contexts are cancelled with ctx
		timer.Reset(NextDelay(j.Interval, j.Jitter, s.rand()))
	}
}

func (s *Scheduler) exec(ctx context.Context, j Job) {
	defer func() {
		if r := recover(); r != nil {
			s.Log.Error("job panicked", "job", j.ID, "panic", r)
		}
	}()
	if j.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, j.Timeout)
		defer cancel()
	}
	j.Run(ctx)
}
