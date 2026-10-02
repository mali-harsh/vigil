package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"
	"time"
)

type Subscriber struct {
	ID          int64      `json:"id"`
	Kind        string     `json:"kind"` // email | webhook
	Address     string     `json:"address"`
	Token       string     `json:"-"` // confirm/unsubscribe secret; also the webhook signing key
	CreatedAt   time.Time  `json:"created_at"`
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
}

func newToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// AddSubscriber registers address. Email starts unconfirmed (double opt-in);
// re-subscribing a pending address issues a fresh token, a confirmed one is
// left alone (confirmed=true tells the caller not to mail again).
func (s *Store) AddSubscriber(ctx context.Context, kind, address string, confirmNow bool) (sub *Subscriber, confirmed bool, err error) {
	address = strings.TrimSpace(address)
	if kind == "email" {
		address = strings.ToLower(address)
	}
	now := time.Now().UTC()
	var conf any
	if confirmNow {
		conf = ms(now)
	}
	existing, err := s.subscriberWhere(ctx, `kind=? AND address=?`, kind, address)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		if existing.ConfirmedAt != nil {
			return existing, true, nil
		}
		existing.Token = newToken()
		_, err := s.db.ExecContext(ctx, s.q(`UPDATE subscribers SET token=?, confirmed_at=? WHERE id=?`), existing.Token, conf, existing.ID)
		return existing, confirmNow, err
	}
	tok := newToken()
	var id int64
	err = s.db.QueryRowContext(ctx, s.q(`INSERT INTO subscribers(kind, address, token, created_at, confirmed_at) VALUES(?,?,?,?,?) RETURNING id`),
		kind, address, tok, ms(now), conf).Scan(&id)
	if err != nil {
		return nil, false, err
	}
	return &Subscriber{ID: id, Kind: kind, Address: address, Token: tok, CreatedAt: now}, confirmNow, nil
}

func (s *Store) ConfirmSubscriber(ctx context.Context, token string) (*Subscriber, error) {
	sub, err := s.subscriberWhere(ctx, `token=?`, token)
	if err != nil || sub == nil {
		if err == nil {
			err = ErrNotFound
		}
		return nil, err
	}
	if sub.ConfirmedAt == nil {
		now := time.Now().UTC()
		if _, err := s.db.ExecContext(ctx, s.q(`UPDATE subscribers SET confirmed_at=? WHERE id=?`), ms(now), sub.ID); err != nil {
			return nil, err
		}
		sub.ConfirmedAt = &now
	}
	return sub, nil
}

func (s *Store) SubscriberByToken(ctx context.Context, token string) (*Subscriber, error) {
	return s.subscriberWhere(ctx, `token=?`, token)
}

func (s *Store) Unsubscribe(ctx context.Context, token string) (bool, error) {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM subscribers WHERE token=?`), token)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) DeleteSubscriber(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM subscribers WHERE id=?`), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Subscribers lists subscribers; confirmedOnly for delivery.
func (s *Store) Subscribers(ctx context.Context, confirmedOnly bool) ([]Subscriber, error) {
	where := ``
	if confirmedOnly {
		where = `WHERE confirmed_at IS NOT NULL`
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, kind, address, token, created_at, confirmed_at FROM subscribers `+where+` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subscriber
	for rows.Next() {
		sub, err := scanSub(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sub)
	}
	return out, rows.Err()
}

func (s *Store) subscriberWhere(ctx context.Context, where string, args ...any) (*Subscriber, error) {
	row := s.db.QueryRowContext(ctx, s.q(`SELECT id, kind, address, token, created_at, confirmed_at FROM subscribers WHERE `+where), args...)
	sub, err := scanSub(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return sub, err
}

func scanSub(r interface{ Scan(...any) error }) (*Subscriber, error) {
	var sub Subscriber
	var created int64
	var conf sql.NullInt64
	if err := r.Scan(&sub.ID, &sub.Kind, &sub.Address, &sub.Token, &created, &conf); err != nil {
		return nil, err
	}
	sub.CreatedAt = fromMS(created)
	if conf.Valid {
		t := fromMS(conf.Int64)
		sub.ConfirmedAt = &t
	}
	return &sub, nil
}
