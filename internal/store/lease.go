package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Leader lease (Postgres only). Every timestamp comparison uses the database
// clock (now()), so node clock skew can't make two nodes both believe they
// hold it. epoch increments on every takeover and fences renewals: a node
// that lost the lease can never renew someone else's.

var ErrNoLease = errors.New("leases need the postgres backend")

// AcquireLease takes the lease if it is free or expired. ok=false means
// another node holds an unexpired lease.
func (s *Store) AcquireLease(ctx context.Context, name, holder, address string, ttl time.Duration) (epoch int64, ok bool, err error) {
	if !s.pg {
		return 0, false, ErrNoLease
	}
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO leader_lease(name, holder, epoch, expires_at, address)
		VALUES ($1, $2, 1, now() + $3 * interval '1 millisecond', $4)
		ON CONFLICT (name) DO UPDATE
			SET holder = excluded.holder, epoch = leader_lease.epoch + 1, expires_at = excluded.expires_at, address = excluded.address
			WHERE leader_lease.expires_at < now()
		RETURNING epoch`, name, holder, ttl.Milliseconds(), address).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return epoch, err == nil, err
}

// RenewLease extends a lease we still hold. ok=false means it was lost
// (expired or taken over) — the caller must stop acting as leader at once.
func (s *Store) RenewLease(ctx context.Context, name, holder string, epoch int64, ttl time.Duration) (ok bool, err error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE leader_lease SET expires_at = now() + $4 * interval '1 millisecond'
		WHERE name = $1 AND holder = $2 AND epoch = $3 AND expires_at > now()`, name, holder, epoch, ttl.Milliseconds())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ReleaseLease expires our lease immediately (graceful shutdown → instant
// failover instead of waiting out the TTL).
func (s *Store) ReleaseLease(ctx context.Context, name, holder string, epoch int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE leader_lease SET expires_at = now() - interval '1 millisecond'
		WHERE name = $1 AND holder = $2 AND epoch = $3`, name, holder, epoch)
	return err
}

type LeaseInfo struct {
	Holder    string
	Address   string
	Epoch     int64
	ExpiresIn time.Duration // negative = expired
}

func (s *Store) LeaseInfo(ctx context.Context, name string) (*LeaseInfo, error) {
	if !s.pg {
		return nil, ErrNoLease
	}
	var li LeaseInfo
	var ms float64
	err := s.db.QueryRowContext(ctx, `SELECT holder, address, epoch, EXTRACT(EPOCH FROM (expires_at - now())) * 1000 FROM leader_lease WHERE name = $1`, name).
		Scan(&li.Holder, &li.Address, &li.Epoch, &ms)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	li.ExpiresIn = time.Duration(ms) * time.Millisecond
	return &li, err
}
