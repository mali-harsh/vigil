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
	out      chan<- check.Result
	location string // stamped on every result ("local" on the server, agent name on agents)
	wg       sync.WaitGroup

	mu      sync.Mutex
	beats   map[string]chan time.Time // push token → ping channel
	running map[string]running        // monitor id → its loop
}

type running struct {
	cancel context.CancelFunc
	done   chan struct{}
	token  string
}

func New(out chan<- check.Result) *Scheduler { return NewAt(out, config.LocalLocation) }

func NewAt(out chan<- check.Result, location string) *Scheduler {
	return &Scheduler{out: out, location: location, beats: map[string]chan time.Time{}, running: map[string]running{}}
}

// Start launches a loop for m (replacing any running one with the same ID).
// Active monitors are probed; push monitors wait for pings via Beat. seed is
// the last heartbeat seen before a restart, so a deadline already running
// isn't reset by restarting vigil.
func (s *Scheduler) Start(ctx context.Context, m config.Monitor, seed time.Time) error {
	if m.Paused {
		return nil
	}
	var c check.Checker
	if m.Type != "push" {
		var err error
		if c, err = check.New(m); err != nil {
			return err
		}
	}
	s.Stop(m.ID)
	lctx, cancel := context.WithCancel(ctx)
	r := running{cancel: cancel, done: make(chan struct{})}
	var ch chan time.Time
	if m.Type == "push" {
		ch = make(chan time.Time, 16)
		r.token = m.Token
	}
	s.mu.Lock()
	s.running[m.ID] = r
	if ch != nil {
		s.beats[m.Token] = ch
	}
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(r.done)
		if ch != nil {
			s.watchPush(lctx, m, ch, seed)
		} else {
			s.probeLoop(lctx, m, c)
		}
	}()
	return nil
}

// Stop ends a monitor's loop and waits for it to exit (so no stale result is
// emitted afterwards).
func (s *Scheduler) Stop(id string) {
	s.mu.Lock()
	r, ok := s.running[id]
	if ok {
		delete(s.running, id)
		if r.token != "" {
			delete(s.beats, r.token)
		}
	}
	s.mu.Unlock()
	if ok {
		r.cancel()
		<-r.done
	}
}

// Running lists the IDs of active loops.
func (s *Scheduler) Running() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.running))
	for id := range s.running {
		ids = append(ids, id)
	}
	return ids
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
	r.Location = s.location
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
func (s *Scheduler) watchPush(ctx context.Context, m config.Monitor, pings <-chan time.Time, seed time.Time) {
	interval, deadline := m.Interval.D(), m.Interval.D()+m.Grace.D()
	started := time.Now()
	last := seed      // last ping; zero = none yet
	healthy := false  // what we last reported
	reported := false // anything reported yet
	firstMiss := deadline
	if !seed.IsZero() {
		// resume the deadline that was running before the restart
		firstMiss = max(deadline-time.Since(seed), 0)
		healthy = time.Since(seed) <= deadline
	}

	tick := time.NewTimer(interval)
	miss := time.NewTimer(firstMiss)
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
		healthy, reported = up, true
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
			if healthy || !reported { // deadline passed: say so now, not at the next tick
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
