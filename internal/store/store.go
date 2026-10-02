// Package store persists results, monitor state and incidents.
//
// Two backends share one implementation:
//   - SQLite (default): WAL mode, pure-Go driver, no CGO — a single binary and
//     a single file.
//   - Postgres: for HA, several vigil servers share one database and elect a
//     leader through a lease row (see lease.go).
//
// Queries are written once with "?" placeholders and portable SQL; q() rebinds
// them to $n for Postgres.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mali-harsh/vigil/internal/check"
	"github.com/mali-harsh/vigil/internal/monitor"
	_ "modernc.org/sqlite"
)

type Store struct {
	db  *sql.DB
	loc *time.Location // day boundaries for daily rollups
	pg  bool
}

// q rebinds "?" placeholders for Postgres. Queries never contain a literal "?".
func (s *Store) q(query string) string {
	if !s.pg {
		return query
	}
	var b strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Postgres reports whether this store is shared (HA-capable).
func (s *Store) Postgres() bool { return s.pg }

// migrations run in order inside a transaction; PRAGMA user_version records
// how many have been applied. Never edit a released migration — append.
var migrations = []string{ // SQLite
	// 1: phase 0 schema (IF NOT EXISTS: phase 0 DBs predate user_version)
	`CREATE TABLE IF NOT EXISTS monitor_state (
		monitor_id TEXT PRIMARY KEY,
		state      TEXT NOT NULL,
		since      INTEGER NOT NULL
	);
	CREATE TABLE IF NOT EXISTS results (
		monitor_id TEXT NOT NULL,
		at         INTEGER NOT NULL,
		status     TEXT NOT NULL,
		latency_ms INTEGER NOT NULL,
		message    TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS results_monitor_at ON results(monitor_id, at);
	CREATE TABLE IF NOT EXISTS incidents (
		id               INTEGER PRIMARY KEY AUTOINCREMENT,
		monitor_id       TEXT NOT NULL,
		started_at       INTEGER NOT NULL,
		resolved_at      INTEGER,
		cause            TEXT NOT NULL,
		last_notified_at INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS incidents_open ON incidents(monitor_id) WHERE resolved_at IS NULL;`,

	// 2: maintenance flag, daily rollups (outlive raw retention), public incidents
	`ALTER TABLE results ADD COLUMN maint INTEGER NOT NULL DEFAULT 0;
	CREATE TABLE daily (
		monitor_id TEXT NOT NULL,
		day        TEXT NOT NULL,
		total      INTEGER NOT NULL,
		down       INTEGER NOT NULL,
		degraded   INTEGER NOT NULL,
		maint      INTEGER NOT NULL,
		PRIMARY KEY (monitor_id, day)
	);
	ALTER TABLE incidents ADD COLUMN title TEXT NOT NULL DEFAULT '';
	ALTER TABLE incidents ADD COLUMN status TEXT NOT NULL DEFAULT 'investigating';
	ALTER TABLE incidents ADD COLUMN impact TEXT NOT NULL DEFAULT 'down';
	ALTER TABLE incidents ADD COLUMN components TEXT NOT NULL DEFAULT '';
	CREATE TABLE incident_updates (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		incident_id INTEGER NOT NULL REFERENCES incidents(id),
		at          INTEGER NOT NULL,
		status      TEXT NOT NULL,
		message     TEXT NOT NULL
	);
	CREATE INDEX incident_updates_incident ON incident_updates(incident_id);
	CREATE INDEX incidents_started ON incidents(started_at);`,

	// 3: multi-location. status stays the raw probe result of that location;
	// eff is the monitor-level (quorum-combined) status uptime is computed from.
	`ALTER TABLE results ADD COLUMN location TEXT NOT NULL DEFAULT 'local';
	ALTER TABLE results ADD COLUMN eff TEXT NOT NULL DEFAULT '';
	UPDATE results SET eff = status;`,
}

// Open opens (creating if needed) the SQLite database at path.
func Open(path string, loc *time.Location) (*Store, error) {
	if loc == nil {
		loc = time.UTC
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite has one writer; serialise instead of SQLITE_BUSY
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db, loc: loc}, nil
}

func migrate(db *sql.DB) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("database is version %d, this vigil knows %d — refusing to downgrade", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func ms(t time.Time) int64 { return t.UnixMilli() }
func boolInt(c bool) int {
	if c {
		return 1
	}
	return 0
}
func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

// Day is the rollup key for t in the store's timezone.
func (s *Store) Day(t time.Time) string { return t.In(s.loc).Format(time.DateOnly) }

type StateRow struct {
	State monitor.State
	Since time.Time
}

func (s *Store) LoadStates(ctx context.Context) (map[string]StateRow, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT monitor_id, state, since FROM monitor_state`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]StateRow{}
	for rows.Next() {
		var id, st string
		var since int64
		if err := rows.Scan(&id, &st, &since); err != nil {
			return nil, err
		}
		out[id] = StateRow{monitor.State(st), fromMS(since)}
	}
	return out, rows.Err()
}

func (s *Store) SaveState(ctx context.Context, id string, st monitor.State, since time.Time) error {
	_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO monitor_state(monitor_id, state, since) VALUES(?,?,?)
		ON CONFLICT(monitor_id) DO UPDATE SET state=excluded.state, since=excluded.since`), id, st, ms(since))
	return err
}

// InsertResult stores a raw result and folds it into the daily rollup in one
// transaction, so the 90-day bars always agree with raw data. eff is the
// monitor-level status at that moment (equal to r.Status for single-location
// monitors); rollups and uptime count eff.
func (s *Store) InsertResult(ctx context.Context, r check.Result, eff check.Status, maint bool) error {
	if r.Location == "" {
		r.Location = "local"
	}
	if eff == "" {
		eff = r.Status
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO results(monitor_id, at, status, latency_ms, message, maint, location, eff) VALUES(?,?,?,?,?,?,?,?)`),
		r.MonitorID, ms(r.At), r.Status, r.Latency.Milliseconds(), r.Message, boolInt(maint), r.Location, eff); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO daily(monitor_id, day, total, down, degraded, maint) VALUES(?,?,1,?,?,?)
		ON CONFLICT(monitor_id, day) DO UPDATE SET total=daily.total+1, down=daily.down+excluded.down,
		degraded=daily.degraded+excluded.degraded, maint=daily.maint+excluded.maint`),
		r.MonitorID, s.Day(r.At), boolInt(!maint && eff == check.Down), boolInt(!maint && eff == check.Degraded), boolInt(maint)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Results(ctx context.Context, id string, since time.Time, limit int) ([]check.Result, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT at, status, latency_ms, message, location FROM results
		WHERE monitor_id=? AND at>=? ORDER BY at DESC LIMIT ?`), id, ms(since), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []check.Result
	for rows.Next() {
		var at, lat int64
		r := check.Result{MonitorID: id}
		if err := rows.Scan(&at, &r.Status, &lat, &r.Message, &r.Location); err != nil {
			return nil, err
		}
		r.At, r.Latency = fromMS(at), time.Duration(lat)*time.Millisecond
		out = append(out, r)
	}
	return out, rows.Err()
}

// Uptime returns the share of non-down results since t, excluding maintenance
// (degraded counts as up: the service answered). ok=false when there is no
// data — never report 100% for a monitor that has not run.
func (s *Store) Uptime(ctx context.Context, id string, since time.Time) (pct float64, ok bool, err error) {
	var total, good int64
	err = s.db.QueryRowContext(ctx, s.q(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN eff<>'down' THEN 1 ELSE 0 END),0) FROM results
		WHERE monitor_id=? AND at>=? AND maint=0`), id, ms(since)).Scan(&total, &good)
	if err != nil || total == 0 {
		return 0, false, err
	}
	return float64(good) * 100 / float64(total), true, nil
}

// LastUp returns the time of the most recent UP result of a monitor (zero if
// none) — used to resume heartbeat deadlines across restarts.
func (s *Store) LastUp(ctx context.Context, id string) (time.Time, error) {
	var at sql.NullInt64
	err := s.db.QueryRowContext(ctx, s.q(`SELECT MAX(at) FROM results WHERE monitor_id=? AND status='up'`), id).Scan(&at)
	if err != nil || !at.Valid {
		return time.Time{}, err
	}
	return fromMS(at.Int64), nil
}

type DayStat struct {
	Total, Down, Degraded, Maint int64
}

// Counted is the number of results that count toward uptime.
func (d DayStat) Counted() int64 { return d.Total - d.Maint }

// Daily returns rollups for the given monitors from fromDay (inclusive),
// keyed monitor → day.
func (s *Store) Daily(ctx context.Context, ids []string, fromDay string) (map[string]map[string]DayStat, error) {
	out := map[string]map[string]DayStat{}
	if len(ids) == 0 {
		return out, nil
	}
	args := []any{fromDay}
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT monitor_id, day, total, down, degraded, maint FROM daily
		WHERE day>=? AND monitor_id IN (?`+strings.Repeat(",?", len(ids)-1)+`)`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, day string
		var d DayStat
		if err := rows.Scan(&id, &day, &d.Total, &d.Down, &d.Degraded, &d.Maint); err != nil {
			return nil, err
		}
		if out[id] == nil {
			out[id] = map[string]DayStat{}
		}
		out[id][day] = d
	}
	return out, rows.Err()
}

func (s *Store) Prune(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM results WHERE at<?`), ms(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- incidents ----

// Incident statuses, as shown publicly.
const (
	Investigating = "investigating"
	Identified    = "identified"
	Monitoring    = "monitoring"
	Resolved      = "resolved"
)

func ValidStatus(s string) bool {
	return s == Investigating || s == Identified || s == Monitoring || s == Resolved
}

func ValidImpact(s string) bool { return s == "down" || s == "degraded" || s == "none" }

type Update struct {
	ID      int64     `json:"id"`
	At      time.Time `json:"at"`
	Status  string    `json:"status"`
	Message string    `json:"message"`
}

type Incident struct {
	ID             int64      `json:"id"`
	MonitorID      string     `json:"monitor_id,omitempty"` // empty = manually declared
	Title          string     `json:"title"`
	Status         string     `json:"status"`
	Impact         string     `json:"impact"`
	Components     []string   `json:"components"`
	StartedAt      time.Time  `json:"started_at"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
	Cause          string     `json:"cause,omitempty"` // internal: raw probe error
	LastNotifiedAt time.Time  `json:"-"`
	Updates        []Update   `json:"updates,omitempty"`
}

type NewIncident struct {
	MonitorID  string
	Title      string
	Impact     string
	Components []string
	At         time.Time
	Cause      string
	Message    string // first public update
}

// OpenIncident creates an incident with its first update. For monitor
// incidents it is idempotent: never two open incidents per monitor.
func (s *Store) OpenIncident(ctx context.Context, n NewIncident) (int64, error) {
	if n.MonitorID != "" {
		if inc, err := s.ActiveIncident(ctx, n.MonitorID); err != nil || inc != nil {
			if inc != nil {
				return inc.ID, nil
			}
			return 0, err
		}
	}
	if n.Impact == "" {
		n.Impact = "down"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var id int64 // RETURNING works on both SQLite (3.35+) and Postgres; LastInsertId doesn't
	if err := tx.QueryRowContext(ctx, s.q(`INSERT INTO incidents(monitor_id, started_at, cause, last_notified_at, title, status, impact, components)
		VALUES(?,?,?,?,?,?,?,?) RETURNING id`), n.MonitorID, ms(n.At), n.Cause, ms(n.At), n.Title, Investigating, n.Impact, strings.Join(n.Components, ",")).Scan(&id); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO incident_updates(incident_id, at, status, message) VALUES(?,?,?,?)`),
		id, ms(n.At), Investigating, n.Message); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// AddUpdate posts a public update; status "resolved" closes the incident.
// title (optional) renames it.
func (s *Store) AddUpdate(ctx context.Context, incID int64, at time.Time, status, message, title string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var resolved sql.NullInt64
	if err := tx.QueryRowContext(ctx, s.q(`SELECT resolved_at FROM incidents WHERE id=?`), incID).Scan(&resolved); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if resolved.Valid {
		return ErrResolved
	}
	if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO incident_updates(incident_id, at, status, message) VALUES(?,?,?,?)`),
		incID, ms(at), status, message); err != nil {
		return err
	}
	q, args := `UPDATE incidents SET status=?`, []any{status}
	if status == Resolved {
		q, args = q+`, resolved_at=?`, append(args, ms(at))
	}
	if title != "" {
		q, args = q+`, title=?`, append(args, title)
	}
	if _, err := tx.ExecContext(ctx, s.q(q+` WHERE id=?`), append(args, incID)...); err != nil {
		return err
	}
	return tx.Commit()
}

var (
	ErrNotFound = errors.New("incident not found")
	ErrResolved = errors.New("incident already resolved")
)

// ResolveIncident closes the open incident of a monitor with an automatic
// update. Returns nil if none was open.
func (s *Store) ResolveIncident(ctx context.Context, monitorID string, at time.Time, message string) (*Incident, error) {
	inc, err := s.ActiveIncident(ctx, monitorID)
	if err != nil || inc == nil {
		return nil, err
	}
	if err := s.AddUpdate(ctx, inc.ID, at, Resolved, message, ""); err != nil {
		return nil, err
	}
	inc.ResolvedAt, inc.Status = &at, Resolved
	return inc, nil
}

func (s *Store) TouchIncident(ctx context.Context, incID int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, s.q(`UPDATE incidents SET last_notified_at=? WHERE id=?`), ms(at), incID)
	return err
}

func (s *Store) ActiveIncident(ctx context.Context, monitorID string) (*Incident, error) {
	incs, err := s.incidents(ctx, `WHERE monitor_id=? AND resolved_at IS NULL`, monitorID)
	if err != nil || len(incs) == 0 {
		return nil, err
	}
	return &incs[0], nil
}

func (s *Store) Incident(ctx context.Context, id int64) (*Incident, error) {
	incs, err := s.incidents(ctx, `WHERE id=?`, id)
	if err != nil {
		return nil, err
	}
	if len(incs) == 0 {
		return nil, ErrNotFound
	}
	return &incs[0], s.loadUpdates(ctx, incs)
}

func (s *Store) OpenIncidents(ctx context.Context) ([]Incident, error) {
	return s.incidents(ctx, `WHERE resolved_at IS NULL ORDER BY started_at`)
}

func (s *Store) RecentIncidents(ctx context.Context, limit int) ([]Incident, error) {
	return s.incidents(ctx, `ORDER BY started_at DESC LIMIT ?`, limit)
}

// IncidentsSince returns incidents open at or started after t, with updates.
func (s *Store) IncidentsSince(ctx context.Context, t time.Time) ([]Incident, error) {
	incs, err := s.incidents(ctx, `WHERE resolved_at IS NULL OR resolved_at>=? ORDER BY started_at DESC`, ms(t))
	if err != nil {
		return nil, err
	}
	return incs, s.loadUpdates(ctx, incs)
}

func (s *Store) loadUpdates(ctx context.Context, incs []Incident) error {
	for i := range incs {
		rows, err := s.db.QueryContext(ctx, s.q(`SELECT id, at, status, message FROM incident_updates WHERE incident_id=? ORDER BY at DESC, id DESC`), incs[i].ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var u Update
			var at int64
			if err := rows.Scan(&u.ID, &at, &u.Status, &u.Message); err != nil {
				rows.Close()
				return err
			}
			u.At = fromMS(at)
			incs[i].Updates = append(incs[i].Updates, u)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) incidents(ctx context.Context, where string, args ...any) ([]Incident, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT id, monitor_id, started_at, resolved_at, cause, last_notified_at, title, status, impact, components
		FROM incidents `+where), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Incident
	for rows.Next() {
		var i Incident
		var started, notified int64
		var resolved sql.NullInt64
		var comps string
		if err := rows.Scan(&i.ID, &i.MonitorID, &started, &resolved, &i.Cause, &notified, &i.Title, &i.Status, &i.Impact, &comps); err != nil {
			return nil, err
		}
		i.StartedAt, i.LastNotifiedAt = fromMS(started), fromMS(notified)
		if resolved.Valid {
			t := fromMS(resolved.Int64)
			i.ResolvedAt = &t
		}
		i.Components = []string{}
		if comps != "" {
			i.Components = strings.Split(comps, ",")
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// Ping verifies the database is usable (for /healthz).
func (s *Store) Ping(ctx context.Context) error {
	if s == nil {
		return errors.New("no store")
	}
	return s.db.PingContext(ctx)
}
