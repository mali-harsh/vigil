// Package ha elects one active vigil server among several sharing a Postgres
// database. Exactly one node — the leader — runs probes, the engine and
// alerting; the others stand by (not ready) and take over when it stops.
//
// Timeline with TTL = 15s (renew every TTL/3):
//
//	leader renews ──5s──► renews ──5s──► (DB unreachable) ...
//	                                     10s without a renewal → leader STEPS DOWN
//	                                     15s → lease expires (DB clock)
//	standby polls every TTL/3 ───────────────────► acquires → becomes leader
//
// The leader always stops at least TTL/3 before anyone else may start, so
// two leaders never overlap — even if the old one is partitioned and can't
// tell anybody. Graceful shutdown releases the lease for instant failover.
package ha

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"crypto/rand"
	"encoding/hex"
)

const LeaseName = "leader"

// Leaser is the subset of the store used for election.
type Leaser interface {
	AcquireLease(ctx context.Context, name, holder, address string, ttl time.Duration) (int64, bool, error)
	RenewLease(ctx context.Context, name, holder string, epoch int64, ttl time.Duration) (bool, error)
	ReleaseLease(ctx context.Context, name, holder string, epoch int64) error
}

type Elector struct {
	Store   Leaser
	ID      string
	Address string // this node's internal URL, published while leading
	TTL     time.Duration
	Log     *slog.Logger
}

// NodeID is hostname + random suffix: unique per process, readable in logs.
func NodeID() string {
	h, _ := os.Hostname()
	b := make([]byte, 3)
	rand.Read(b)
	return fmt.Sprintf("%s-%s", h, hex.EncodeToString(b))
}

// Run blocks until ctx is done. Each time this node wins the lease, lead is
// called with a context that is cancelled the moment leadership is lost;
// Run waits for lead to return before competing again.
func (e *Elector) Run(ctx context.Context, lead func(ctx context.Context)) {
	every := e.TTL / 3
	for {
		epoch, ok, err := e.try(ctx)
		if err != nil {
			e.Log.Warn("lease acquire failed", "node", e.ID, "err", err)
		}
		if ok {
			e.term(ctx, epoch, lead)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

func (e *Elector) try(ctx context.Context) (int64, bool, error) {
	cctx, cancel := context.WithTimeout(ctx, e.TTL/3)
	defer cancel()
	return e.Store.AcquireLease(cctx, LeaseName, e.ID, e.Address, e.TTL)
}

func (e *Elector) term(ctx context.Context, epoch int64, lead func(context.Context)) {
	e.Log.Info("became leader", "node", e.ID, "epoch", epoch)
	lctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); lead(lctx) }()

	every, stepDownAfter := e.TTL/3, e.TTL*2/3
	lastOK := time.Now() // monotonic: immune to wall-clock jumps
	t := time.NewTicker(every)
	defer t.Stop()
	reason := ""
loop:
	for {
		select {
		case <-ctx.Done():
			reason = "shutdown"
			break loop
		case <-done:
			reason = "leader routine exited"
			break loop
		case <-t.C:
			rctx, cancel := context.WithTimeout(ctx, every)
			ok, err := e.Store.RenewLease(rctx, LeaseName, e.ID, epoch, e.TTL)
			cancel()
			switch {
			case err == nil && ok:
				lastOK = time.Now()
			case err == nil:
				reason = "lease lost (expired or taken over)"
				break loop
			case time.Since(lastOK) >= stepDownAfter:
				reason = "cannot renew lease: " + err.Error()
				break loop
			default:
				e.Log.Warn("lease renew failed, retrying", "node", e.ID, "err", err, "since_last_renew", time.Since(lastOK).Round(time.Millisecond))
			}
		}
	}
	stop()
	<-done
	e.Log.Warn("stepped down", "node", e.ID, "epoch", epoch, "reason", reason)
	if reason == "shutdown" || reason == "leader routine exited" {
		rctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := e.Store.ReleaseLease(rctx, LeaseName, e.ID, epoch); err != nil {
			e.Log.Warn("lease release failed (standby takes over after TTL)", "err", err)
		}
		cancel()
	}
}
