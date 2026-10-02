package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// AckIncident records who acknowledged an open incident. Acknowledging twice
// is not an error (first ack wins); acked reports whether this call did it.
func (s *Store) AckIncident(ctx context.Context, id int64, at time.Time, by string) (acked bool, err error) {
	res, err := s.db.ExecContext(ctx, s.q(`UPDATE incidents SET acked_at=?, acked_by=? WHERE id=? AND resolved_at IS NULL AND acked_at IS NULL`), ms(at), by, id)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return true, nil
	}
	var resolved, ackedAt sql.NullInt64
	err = s.db.QueryRowContext(ctx, s.q(`SELECT resolved_at, acked_at FROM incidents WHERE id=?`), id).Scan(&resolved, &ackedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, ErrNotFound
	case err != nil:
		return false, err
	case resolved.Valid:
		return false, ErrResolved
	}
	return false, nil // already acknowledged
}

// SetEscalationStep records the highest escalation step already notified.
func (s *Store) SetEscalationStep(ctx context.Context, id int64, step int) error {
	_, err := s.db.ExecContext(ctx, s.q(`UPDATE incidents SET esc_step=? WHERE id=? AND esc_step<?`), step, id, step)
	return err
}
