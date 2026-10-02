package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers driver "pgx"
)

// pgMigrations mirror the SQLite schema. Postgres support starts at the
// SQLite v3 schema, so its v1 is the whole thing. Append-only, like SQLite's.
var pgMigrations = []string{
	`CREATE TABLE monitor_state (
		monitor_id TEXT PRIMARY KEY,
		state      TEXT NOT NULL,
		since      BIGINT NOT NULL
	);
	CREATE TABLE results (
		monitor_id TEXT NOT NULL,
		at         BIGINT NOT NULL,
		status     TEXT NOT NULL,
		latency_ms BIGINT NOT NULL,
		message    TEXT NOT NULL,
		maint      INTEGER NOT NULL DEFAULT 0,
		location   TEXT NOT NULL DEFAULT 'local',
		eff        TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX results_monitor_at ON results(monitor_id, at);
	CREATE INDEX results_at ON results(at);
	CREATE TABLE incidents (
		id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		monitor_id       TEXT NOT NULL,
		started_at       BIGINT NOT NULL,
		resolved_at      BIGINT,
		cause            TEXT NOT NULL,
		last_notified_at BIGINT NOT NULL,
		title            TEXT NOT NULL DEFAULT '',
		status           TEXT NOT NULL DEFAULT 'investigating',
		impact           TEXT NOT NULL DEFAULT 'down',
		components       TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX incidents_open ON incidents(monitor_id) WHERE resolved_at IS NULL;
	CREATE INDEX incidents_started ON incidents(started_at);
	CREATE TABLE daily (
		monitor_id TEXT NOT NULL,
		day        TEXT NOT NULL,
		total      INTEGER NOT NULL,
		down       INTEGER NOT NULL,
		degraded   INTEGER NOT NULL,
		maint      INTEGER NOT NULL,
		PRIMARY KEY (monitor_id, day)
	);
	CREATE TABLE incident_updates (
		id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		incident_id BIGINT NOT NULL REFERENCES incidents(id),
		at          BIGINT NOT NULL,
		status      TEXT NOT NULL,
		message     TEXT NOT NULL
	);
	CREATE INDEX incident_updates_incident ON incident_updates(incident_id);
	CREATE TABLE leader_lease (
		name       TEXT PRIMARY KEY,
		holder     TEXT NOT NULL,
		epoch      BIGINT NOT NULL,
		expires_at TIMESTAMPTZ NOT NULL
	);`,

	// 2: the leader's internal URL, so standbys can proxy to it
	`ALTER TABLE leader_lease ADD COLUMN address TEXT NOT NULL DEFAULT '';`,
}

// OpenPostgres connects to a shared Postgres database (DSN or URL).
func OpenPostgres(ctx context.Context, url string, loc *time.Location) (*Store, error) {
	if loc == nil {
		loc = time.UTC
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxIdleTime(5 * time.Minute) // don't hold connections a proxy/LB will silently drop
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := migratePG(pctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db, loc: loc, pg: true}, nil
}

// migratePG applies pending migrations in one transaction holding an advisory
// lock, so several nodes starting together don't race each other.
func migratePG(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('vigil-migrate'))`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS vigil_schema (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var v int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM vigil_schema`).Scan(&v); err != nil {
		return err
	}
	if v > len(pgMigrations) {
		return fmt.Errorf("database is version %d, this vigil knows %d — refusing to downgrade", v, len(pgMigrations))
	}
	for i := v; i < len(pgMigrations); i++ {
		if _, err := tx.ExecContext(ctx, pgMigrations[i]); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO vigil_schema(version) VALUES($1)`, i+1); err != nil {
			return err
		}
	}
	return tx.Commit()
}
