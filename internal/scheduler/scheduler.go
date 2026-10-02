// Package scheduler runs every monitor on its own goroutine and timer so one
// slow or hanging target can never delay another. Results go to a single
// channel consumed by the engine.
package scheduler

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/mali-harsh/vigil/internal/check"
	"github.com/mali-harsh/vigil/internal/config"
)

type Scheduler struct {
	out chan<- check.Result
	wg  sync.WaitGroup

	mu    sync.Mutex
	beats map[string]chan time.Time // push token → ping channel
}

func New(out chan<- check.Result) *Scheduler {
	return &Scheduler{out: out, beats: map[string]chan time.Time{}}
}

// Start launches a loop for m. Active monitors are probed; push monitors wait
// for pings via Beat.
func (s *Scheduler) Start(ctx context.Context, m config.Monitor) error {
	if m.Paused {
		return nil
	}
	if m.Type == "push" {
		ch := make(chan time.Time, 16)
		s.mu.Lock()
		s.beats[m.Token] = ch
		s.mu.Unlock()
		s.wg.Add(1)
		go func() { defer s.wg.Done(); s.watchPush(ctx, m, ch) }()
		return nil
	}
	c, err := check.New(m)
	if err != nil {
		return err
	}
	s.wg.Add(1)
	go func() { defer s.wg.Done(); s.probeLoop(ctx, m, c) }()
	return nil
}

// Beat records a heartbeat ping. Returns false for unknown tokens.
func (s *Scheduler) Beat(token string, at time.Time) bool {
	s.mu.Lock()
	ch, ok := s.beats[token]
	s.mu.Unlock()
	if ok {
		select {
		case ch <- at:
		default: // a burst of pings carries no extra information
		}
	}
	return ok
}

func (s *Scheduler) Wait() { s.wg.Wait() }

func (s *Scheduler) emit(ctx context.Context, r check.Result) {
	select {
	case s.out <- r:
	case <-ctx.Done():
	}
}

func (s *Scheduler) probeLoop(ctx context.Context, m config.Monitor, c check.Checker) {
	interval := m.Interval.D()
	// Spread first runs across the interval so N monitors don't fire in one burst.
	first := time.Duration(rand.Int64N(int64(min(interval, 10*time.Second))))
	timer := time.NewTimer(first)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		start := time.Now()
		pctx, cancel := context.WithTimeout(ctx, m.Timeout.D())
		r := c.Check(pctx)
		cancel()
		if ctx.Err() != nil {
			return // shutting down: a cancelled probe is not an outage
		}
		r.MonitorID, r.At = m.ID, start.UTC()
		s.emit(ctx, r)
		// Next run is measured from this run's start, so cadence stays fixed
		// regardless of how long the probe took (timeout < interval enforced).
		timer.Reset(max(interval-time.Since(start), 0))
	}
}

// watchPush evaluates a heartbeat on a fixed cadence — one result per
// interval in every state — so result counts stay proportional to time and
// uptime % is honest (pings may arrive far more often than outages are
// sampled). State changes are still immediate: a missed deadline emits DOWN
// at once, and the first ping after silence emits UP at once.
func (s *Scheduler) watchPush(ctx context.Context, m config.Monitor, pings <-chan time.Time) {
	interval, deadline := m.Interval.D(), m.Interval.D()+m.Grace.D()
	started := time.Now()
	var last time.Time // last ping; zero = none yet
	healthy := false   // what we last reported

	tick := time.NewTimer(interval)
	miss := time.NewTimer(deadline)
	defer tick.Stop()
	defer miss.Stop()
	reset := func(t *time.Timer, d time.Duration) {
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
		t.Reset(d)
	}
	report := func(at time.Time, up bool) {
		healthy = up
		r := check.Result{MonitorID: m.ID, At: at.UTC(), Status: check.Up, Message: "heartbeat received"}
		if !up {
			r.Status, r.Message = check.Down, "no heartbeat since start"
			if !last.IsZero() {
				r.Message = "no heartbeat for " + at.Sub(last).Round(time.Second).String()
			}
		}
		s.emit(ctx, r)
		reset(tick, interval)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case at := <-pings:
			last = at
			reset(miss, deadline)
			if !healthy {
				report(at, true) // recovery (or first ping) is news right away
			}
		case now := <-miss.C:
			if healthy || last.IsZero() {
				report(now, false)
			}
		case now := <-tick.C:
			switch {
			case !last.IsZero() && now.Sub(last) <= deadline:
				report(now, true)
			case !last.IsZero() || now.Sub(started) > deadline:
				report(now, false)
			default:
				reset(tick, interval) // no ping yet, still within the first deadline
			}
		}
	}
}
