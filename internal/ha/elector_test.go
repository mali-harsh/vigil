package ha_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mali-harsh/vigil/internal/ha"
	"github.com/mali-harsh/vigil/internal/store"
	"github.com/mali-harsh/vigil/internal/store/storetest"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// timeline records leadership intervals and fails on any overlap.
type timeline struct {
	mu      sync.Mutex
	leading atomic.Int32
	max     atomic.Int32
	terms   []string
}

func (tl *timeline) lead(id string) func(context.Context) {
	return func(ctx context.Context) {
		n := tl.leading.Add(1)
		for {
			m := tl.max.Load()
			if n <= m || tl.max.CompareAndSwap(m, n) {
				break
			}
		}
		tl.mu.Lock()
		tl.terms = append(tl.terms, id)
		tl.mu.Unlock()
		<-ctx.Done()
		tl.leading.Add(-1)
	}
}

func (tl *timeline) current() int32 { return tl.leading.Load() }

func pg(t *testing.T) string {
	if storetest.PostgresURL() == "" {
		t.Skip("set VIGIL_TEST_POSTGRES")
	}
	return storetest.DatabaseURL(t, filepath.Join(t.TempDir(), "ha"))
}

func node(t *testing.T, url, id string, ttl time.Duration) (*store.Store, *ha.Elector) {
	s, err := store.OpenPostgres(context.Background(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, &ha.Elector{Store: s, ID: id, TTL: ttl, Log: quiet}
}

func eventually(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out: %s", what)
}

func TestFailoverOnCrashAndGracefulHandover(t *testing.T) {
	url := pg(t)
	ttl := 600 * time.Millisecond
	tl := &timeline{}
	_, a := node(t, url, "a", ttl)
	_, b := node(t, url, "b", ttl)

	actx, crashA := context.WithCancel(context.Background())
	go a.Run(actx, tl.lead("a"))
	eventually(t, "a leads", 2*time.Second, func() bool { return tl.current() == 1 })
	bctx, stopB := context.WithCancel(context.Background())
	defer stopB()
	go b.Run(bctx, tl.lead("b"))
	time.Sleep(3 * ttl)
	tl.mu.Lock()
	terms := append([]string(nil), tl.terms...)
	tl.mu.Unlock()
	if tl.max.Load() != 1 || len(terms) != 1 {
		t.Fatalf("b must stand by while a renews: terms=%v", terms)
	}

	// graceful stop of a → lease released → b leads fast (well under TTL)
	start := time.Now()
	crashA()
	eventually(t, "b takes over", 2*time.Second, func() bool { tl.mu.Lock(); defer tl.mu.Unlock(); return len(tl.terms) == 2 })
	if d := time.Since(start); d > ttl {
		t.Errorf("graceful handover took %s (> TTL %s): release not used?", d, ttl)
	}
	if tl.max.Load() != 1 {
		t.Fatal("two leaders at once")
	}
}

func TestPartitionedLeaderStepsDownBeforeTakeover(t *testing.T) {
	url := pg(t)
	ttl := 900 * time.Millisecond
	var aLead, bLead atomic.Int64 // unix nanos: when a stopped, when b started
	sa, a := node(t, url, "a", ttl)
	_, b := node(t, url, "b", ttl)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	aStarted := make(chan struct{})
	go a.Run(ctx, func(lctx context.Context) {
		close(aStarted)
		<-lctx.Done()
		aLead.Store(time.Now().UnixNano())
	})
	<-aStarted
	go b.Run(ctx, func(lctx context.Context) {
		bLead.CompareAndSwap(0, time.Now().UnixNano())
		<-lctx.Done()
	})

	// partition a from the database: every renew now errors (it can't tell
	// anyone it's leaving — the lease must simply expire)
	breakDB(t, sa)
	eventually(t, "b leads after a's lease expires", 5*time.Second, func() bool { return bLead.Load() != 0 })
	if aLead.Load() == 0 {
		t.Fatal("partitioned leader never stepped down")
	}
	gap := time.Duration(bLead.Load() - aLead.Load())
	if gap <= 0 {
		t.Fatalf("overlap: b started %s before a stopped", -gap)
	}
	t.Logf("a stepped down %s before b took over", gap.Round(time.Millisecond))
}

// breakDB makes all further queries on s fail, simulating a network partition.
func breakDB(t *testing.T, s *store.Store) {
	t.Helper()
	db := store.DBForTest(s)
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(context.Background()) // hold the only connection...
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	var _ *sql.Conn = conn // ...so every other query blocks until its timeout
}
