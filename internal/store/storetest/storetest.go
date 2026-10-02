// Package storetest opens stores for tests. By default it uses SQLite; with
// VIGIL_TEST_POSTGRES set (an admin URL, e.g.
// postgres://vigil:pw@127.0.0.1:5432/postgres?sslmode=disable) every test gets
// its own fresh Postgres database, so the whole suite runs on both backends.
package storetest

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"sync"
	"testing"

	"github.com/mali-harsh/vigil/internal/store"
)

var (
	mu      sync.Mutex
	created = map[string]bool{}
)

// PostgresURL is the admin URL from the environment ("" = SQLite only).
func PostgresURL() string { return os.Getenv("VIGIL_TEST_POSTGRES") }

// OpenAt opens the store identified by key (a file path for SQLite). Opening
// the same key twice within a test reaches the same data (restart tests).
// The caller closes the store.
func OpenAt(t testing.TB, key string) *store.Store {
	t.Helper()
	base := PostgresURL()
	if base == "" {
		s, err := store.Open(key, nil)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s, err := store.OpenPostgres(context.Background(), DatabaseURL(t, key), nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// DatabaseURL creates (once per test+key) a fresh database and returns its URL.
func DatabaseURL(t testing.TB, key string) string {
	t.Helper()
	base := PostgresURL()
	sum := sha1.Sum([]byte(t.Name() + "\x00" + key))
	name := "vt_" + hex.EncodeToString(sum[:8])
	mu.Lock()
	defer mu.Unlock()
	if !created[name] {
		admin, err := sql.Open("pgx", base)
		if err != nil {
			t.Fatal(err)
		}
		defer admin.Close()
		if _, err := admin.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
			t.Fatal(err)
		}
		created[name] = true
		t.Cleanup(func() {
			a, err := sql.Open("pgx", base)
			if err == nil {
				a.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`)
				a.Close()
			}
			mu.Lock()
			delete(created, name)
			mu.Unlock()
		})
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}
